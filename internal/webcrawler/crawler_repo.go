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
	Logger      *zap.SugaredLogger
	Parser      pageparser.PageParser
	Networker   networker.Networker
	ExtraWorker sugaredworker.SugaredWorker
	CachePages  cache.CachedStorage
	CacheRobots cache.CachedStorage
}

func NewCrawlerRepo(logger *zap.SugaredLogger, parser pageparser.PageParser, networker networker.Networker, extraWorker sugaredworker.SugaredWorker, cachePages cache.CachedStorage, cacheRobots cache.CachedStorage) *CrawlerRepo {
	return &CrawlerRepo{
		Logger:      logger,
		Parser:      parser,
		Networker:   networker,
		ExtraWorker: extraWorker,
		CachePages:  cachePages,
		CacheRobots: cacheRobots,
	}
}

func (repo *CrawlerRepo) StartCrawler(cfg *CrawlerConfig) {
	for range cfg.WorkersNumber {
		go repo.crawlWorker(cfg.TaskConsumerChan, cfg.TaskProducerChan, cfg.SaverChan)
	}
}

func (repo *CrawlerRepo) crawlWorker(tcChan <-chan *config.Task, tpChan chan<- []*config.Task, saverChan chan<- *data.PageData) {
	repo.Logger.Infof("Started crawlWorker")

	for task := range tcChan {
		newTasks, err := repo.processTask(task, saverChan)
		if err != nil {
			repo.Logger.Warnf("Error processing task: %s", err)
			continue
		}

		tpChan <- newTasks
	}
}

func (repo *CrawlerRepo) processTask(task *config.Task, saverChan chan<- *data.PageData) ([]*config.Task, error) {
	defer repo.onTaskDone(task.Run)

	if task.Run.UseCacheFlag {
		cachedLinks, errCachedLinks := repo.getCachedLinks(task)

		if errCachedLinks == nil {
			repo.createNewTasksFromLinks(task, cachedLinks)
			return nil, ErrCacheHit
		}
	}

	pd, err := repo.processCrawlTask(task)
	if err != nil {
		repo.Logger.Warnw("Failed to process task", "task", task, "error", err)
		return nil, err
	}

	select {
	case saverChan <- pd:
		repo.Logger.Debugw("Sent pageData to saverChan", "pd", pd)
	case <-time.After(3 * time.Second):
		repo.Logger.Warnw("Saver channel full, dropping page data", "url", task.URL)
	}

	errCache := repo.CachePages.Set(task.URL, pd, cache.BaseTTL)
	if errCache != nil {
		repo.Logger.Warnw("Failed to cache page", "url", task.URL, "depth", task.CurrentDepth, "err", errCache)
	}

	newTasks := repo.createNewTasksFromLinks(task, pd.Links)

	return newTasks, nil
}

func (repo *CrawlerRepo) processCrawlTask(task *config.Task) (*data.PageData, error) {
	canParse := repo.isAllowedByRobots(task.URL)
	if !canParse {
		repo.Logger.Warnw("Skipping link because of robots.txt", "url", task.URL)
		return nil, ErrNotAllowedByRobots
	}

	pd, err := repo.scrap(task)
	if err != nil {
		repo.Logger.Warnw("Failed to scrap page", "error", err)
		return nil, err
	}

	return pd, nil
}

func (repo *CrawlerRepo) scrap(task *config.Task) (*data.PageData, error) {
	repo.Logger.Infow("scraping link", "url", task.URL)

	fetchRes, errFetch := repo.Networker.Fetch(task.URL)
	if errFetch != nil {
		repo.Logger.Warnw("Failed to fetch link", "url", task.URL, "depth", task.CurrentDepth, "err", errFetch)
		return nil, ErrFetching
	}

	// TODO: fix and refactor
	//if task.Run.ExtraFlags != nil {
	//	extra := repo.ExtraWorker.PerformExtraTask(task.URL, task.Run.ExtraFlags)
	//
	//	if task.Run.ExtraFlags.ParseRenderedHTML {
	//		fetchRes.Body = extra.HTMLTask
	//	}
	//}

	linksFromThePage, errExtract := repo.extractLinksFromPage(task, fetchRes.Body)
	if errExtract != nil {
		repo.Logger.Warnw("Failed to extract links", "url", task.URL, "err", errExtract)
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

		links, err := repo.Parser.ExtractLinksFromJS(baseURL, string(body))
		if err != nil {
			return nil, fmt.Errorf("failed to extract links from JS: %w", err)
		}

		return links, nil
	}

	links := repo.Parser.ParseHTML(body, task.URL)
	jsonLinks, err := repo.Parser.ExtractLinksFromJSON(task.URL, body)
	if err != nil {
		return nil, fmt.Errorf("failed to extract links from JSON: %w", err)
	}

	links = append(links, jsonLinks...)
	return links, nil
}

func (repo *CrawlerRepo) onTaskDone(run *config.Run) {
	left := run.DecrementActiveWithMutex()
	if left <= 0 {
		select {
		case <-repo.RunLimiter:
			repo.Logger.Infow("Run finished; released run slot", "runID", run.ID)
		default:
			repo.Logger.Infow("Run finished, but the run slot was already empty for some reason", "runID", run.ID)
		}
	}
}

func (repo *CrawlerRepo) getCachedLinks(task *config.Task) ([]string, error) {
	cachedPageRaw, errCache := repo.CachePages.Get(task.URL)

	var cachedPageData data.PageData
	errUnmarshal := json.Unmarshal([]byte(cachedPageRaw), &cachedPageData)

	if errCache == nil && errUnmarshal == nil {
		repo.Logger.Infow("using cached page", "url", task.URL)
		return cachedPageData.Links, nil
	}

	return nil, errors.Join(errCache, errUnmarshal)
}

func (repo *CrawlerRepo) createNewTasksFromLinks(prevTask *config.Task, links []string) []*config.Task {
	newTasks := make([]*config.Task, len(links))

	for _, link := range links {
		newTask := &config.Task{
			URL:          link,
			CurrentDepth: prevTask.CurrentDepth + 1,
			Run:          prevTask.Run,
		}

		newTasks = append(newTasks, newTask)
	}

	return newTasks
}

func (repo *CrawlerRepo) StartRunListener() {
	for {
		run, err := repo.Processor.GetRun()
		if err != nil {
			repo.Logger.Errorf("Error getting run: %v", err)
			continue
		}

		repo.RunLimiter <- struct{}{}

		firstTask := &config.Task{
			URL:          utils.CorrectURLScheme(run.StartURL),
			Run:          run,
			CurrentDepth: 0,
		}

		err = repo.Processor.SendTask(firstTask)
		if err != nil {
			repo.Logger.Errorf("Error sending task: %v", err)
			<-repo.RunLimiter
		}
	}
}

func (repo *CrawlerRepo) isAllowedByRobots(urlToCheck string) bool {
	baseURL, err := utils.GetBaseURL(urlToCheck)
	if err != nil {
		repo.Logger.Errorw("Failed to get robots URL", "url", urlToCheck, "err", err)
		return false
	}

	robots, errRobotsCache := repo.CacheRobots.Get(baseURL)
	if errRobotsCache == nil {
		repo.Logger.Infow("Robots cache hit", "url", urlToCheck)
		return grobotstxt.AgentAllowed(robots, "project-arachne", urlToCheck)
	}

	repo.Logger.Warnw("cache miss or some other redis error", "errCache", errRobotsCache)

	robotsURL := baseURL + "/robots.txt"
	responseData, errFetch := repo.Networker.Fetch(robotsURL)
	if errFetch != nil {
		repo.Logger.Errorw("failed to fetch robots", "url", robotsURL, "err", errFetch)
		return false
	}

	if responseData.Status == http.StatusNotFound {
		repo.Logger.Warnw("robots URL not found", "url", robotsURL)
		return true
	}

	robots = string(responseData.Body)

	errSaveCache := repo.CacheRobots.Set(baseURL, string(responseData.Body), cache.BaseTTL)
	if errSaveCache != nil {
		repo.Logger.Warnw("failed to save cache", "url", baseURL, "err", errSaveCache)
	}

	return grobotstxt.AgentAllowed(robots, "project-arachne", urlToCheck)
}
