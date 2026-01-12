package webcrawler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/domain/data"
	"web-crawler/internal/networker"
	"web-crawler/internal/networker/sugaredworker"
	"web-crawler/internal/pageparser"
	"web-crawler/internal/utils"
	"web-crawler/internal/webcrawler/cache"

	"github.com/jimsmart/grobotstxt"
	"go.uber.org/zap"
)

type CrawlerRepo struct {
	logger      *zap.SugaredLogger
	parser      pageparser.PageParser
	networker   networker.Networker
	extraWorker sugaredworker.SugaredWorker
	cachePages  cache.CachedStorage
	cacheRobots cache.CachedStorage

	cfg *CrawlerConfig
}

func NewCrawlerRepo(
	logger *zap.SugaredLogger,
	parser pageparser.PageParser,
	networker networker.Networker,
	extraWorker sugaredworker.SugaredWorker,
	cachePages cache.CachedStorage,
	cacheRobots cache.CachedStorage,
	cfg *CrawlerConfig,
) *CrawlerRepo {
	return &CrawlerRepo{
		logger:      logger,
		parser:      parser,
		networker:   networker,
		extraWorker: extraWorker,
		cachePages:  cachePages,
		cacheRobots: cacheRobots,
		cfg:         cfg,
	}
}

func (repo *CrawlerRepo) StartCrawler() {
	for range repo.cfg.WorkersNumber {
		go repo.crawlWorker(repo.cfg.TaskConsumerChan, repo.cfg.TaskProducerChan, repo.cfg.SaverChan)
	}
}

func (repo *CrawlerRepo) crawlWorker(tcChan <-chan *config.Task, tpChan chan<- []*config.Task, saverChan chan<- *data.PageData) {
	repo.logger.Infof("Started crawlWorker")

	for task := range tcChan {
		err := repo.processTask(task, tpChan, saverChan)
		if err != nil {
			repo.logger.Warnf("Error processing task: %s", err)
			continue
		}
	}
}

func (repo *CrawlerRepo) processTask(task *config.Task, tpChan chan<- []*config.Task, saverChan chan<- *data.PageData) error {
	defer repo.onTaskDone(task.Run)

	if task.Run.UseCacheFlag {
		cachedLinks, errCachedLinks := repo.getCachedLinks(task)

		if errCachedLinks == nil {
			repo.createNewTasksFromLinks(task, cachedLinks)
			return ErrCacheHit
		}
	}

	pd, err := repo.processCrawlTask(task)
	if err != nil {
		repo.logger.Warnw("Failed to process task", "task", task, "error", err)
		return err
	}

	select {
	case saverChan <- pd:
		repo.logger.Debugw("Sent pageData to saverChan", "pd", pd)
	case <-time.After(3 * time.Second):
		repo.logger.Warnw("Saver channel full, dropping page data", "url", task.URL)
	}

	errCache := repo.cachePages.Set(task.URL, pd, cache.BaseTTL)
	if errCache != nil {
		repo.logger.Warnw("Failed to cache page", "url", task.URL, "depth", task.CurrentDepth, "err", errCache)
	}

	newTasks := repo.createNewTasksFromLinks(task, pd.Links)

	tpChan <- newTasks

	return nil
}

func (repo *CrawlerRepo) processCrawlTask(task *config.Task) (*data.PageData, error) {
	canParse := repo.isAllowedByRobots(task.URL)
	if !canParse {
		repo.logger.Warnw("Skipping link because of robots.txt", "url", task.URL)
		return nil, ErrNotAllowedByRobots
	}

	pd, err := repo.scrap(task)
	if err != nil {
		repo.logger.Warnw("Failed to scrap page", "error", err)
		return nil, err
	}

	return pd, nil
}

func (repo *CrawlerRepo) scrap(task *config.Task) (*data.PageData, error) {
	repo.logger.Infow("scraping link", "url", task.URL)

	fetchRes, errFetch := repo.networker.Fetch(task.URL)
	if errFetch != nil {
		repo.logger.Warnw("Failed to fetch link", "url", task.URL, "depth", task.CurrentDepth, "err", errFetch)
		return nil, ErrFetching
	}

	// TODO: fix and refactor
	//if task.Run.ExtraFlags != nil {
	//	extra := repo.extraWorker.PerformExtraTask(task.URL, task.Run.ExtraFlags)
	//
	//	if task.Run.ExtraFlags.ParseRenderedHTML {
	//		fetchRes.Body = extra.HTMLTask
	//	}
	//}

	linksFromThePage, errExtract := repo.extractLinksFromPage(task, fetchRes.Body)
	if errExtract != nil {
		repo.logger.Warnw("Failed to extract links", "url", task.URL, "err", errExtract)
		linksFromThePage = []string{}
	}

	pageData := &data.PageData{
		URL:           task.URL,
		Status:        fetchRes.Status,
		Links:         linksFromThePage,
		LastRunID:     task.Run.ID,
		LastUpdatedAt: time.Now(),
		FoundAt:       time.Now(),
		ContentType:   fetchRes.ContentType,
	}

	return pageData, nil
}

func (repo *CrawlerRepo) extractLinksFromPage(task *config.Task, body []byte) ([]string, error) {
	if strings.HasSuffix(strings.TrimSuffix(task.URL, "/"), ".js") {
		baseURL, err := utils.GetBaseURL(task.URL)
		if err != nil {
			return nil, fmt.Errorf("failed to get base URL: %w", err)
		}

		links, err := repo.parser.ExtractLinksFromJS(baseURL, string(body))
		if err != nil {
			return nil, fmt.Errorf("failed to extract links from JS: %w", err)
		}

		return links, nil
	}

	links := repo.parser.ParseHTML(body, task.URL)
	jsonLinks, err := repo.parser.ExtractLinksFromJSON(task.URL, body)
	if err != nil {
		return nil, fmt.Errorf("failed to extract links from JSON: %w", err)
	}

	links = append(links, jsonLinks...)
	return links, nil
}

func (repo *CrawlerRepo) onTaskDone(run *config.Run) {
	left := run.DecrementActiveWithMutex()
	// вообще тут было раньше <= 0, и я хз, почему оно не вызывало несколько раз освобождение семафора, но теперь
	// логика адекватна, да и я еще Run делаю с Once, гарантируя всего ОДНО освобождение семафора ранов
	if left == 0 {
		// run.Do - вызов к once.Do из sync.Once
		run.Do(func() {
			select {
			case repo.cfg.CrawlCallbackChan <- struct{}{}:
				repo.logger.Infow("Run finished; released run slot", "runID", run.ID)
			default:
				repo.logger.Infow("Run finished, but the run slot was already empty for some reason", "runID", run.ID)
			}
		})
	}
}

func (repo *CrawlerRepo) getCachedLinks(task *config.Task) ([]string, error) {
	cachedPageRaw, errCache := repo.cachePages.Get(task.URL)

	var cachedPageData data.PageData
	errUnmarshal := json.Unmarshal([]byte(cachedPageRaw), &cachedPageData)

	if errCache == nil && errUnmarshal == nil {
		repo.logger.Infow("using cached page", "url", task.URL)
		return cachedPageData.Links, nil
	}

	return nil, errors.Join(errCache, errUnmarshal)
}

func (repo *CrawlerRepo) createNewTasksFromLinks(prevTask *config.Task, links []string) []*config.Task {
	newTasks := make([]*config.Task, len(links))
	newDepth := prevTask.CurrentDepth + 1

	for _, link := range links {
		newTask := &config.Task{
			URL:          link,
			CurrentDepth: newDepth,
			Run:          prevTask.Run,
		}

		newTasks = append(newTasks, newTask)
	}

	return newTasks
}

func (repo *CrawlerRepo) isAllowedByRobots(urlToCheck string) bool {
	baseURL, err := utils.GetBaseURL(urlToCheck)
	if err != nil {
		repo.logger.Errorw("Failed to get robots URL", "url", urlToCheck, "err", err)
		return false
	}

	robots, errRobotsCache := repo.cacheRobots.Get(baseURL)
	if errRobotsCache == nil {
		repo.logger.Infow("Robots cache hit", "url", urlToCheck)
		return grobotstxt.AgentAllowed(robots, "project-arachne", urlToCheck)
	}

	repo.logger.Warnw("cache miss or some other redis error", "errCache", errRobotsCache)

	robotsURL := baseURL + "/robots.txt"
	responseData, errFetch := repo.networker.Fetch(robotsURL)
	if errFetch != nil {
		repo.logger.Errorw("failed to fetch robots", "url", robotsURL, "err", errFetch)
		return false
	}

	if responseData.Status == http.StatusNotFound {
		repo.logger.Warnw("robots URL not found", "url", robotsURL)
		return true
	}

	robots = string(responseData.Body)

	errSaveCache := repo.cacheRobots.Set(baseURL, string(responseData.Body), cache.BaseTTL)
	if errSaveCache != nil {
		repo.logger.Warnw("failed to save cache", "url", baseURL, "err", errSaveCache)
	}

	return grobotstxt.AgentAllowed(robots, "project-arachne", urlToCheck)
}
