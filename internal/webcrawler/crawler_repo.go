package webcrawler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"web-crawler/internal/config"
	"web-crawler/internal/networker"
	"web-crawler/internal/networker/sugaredworker"
	"web-crawler/internal/pageparser"
	"web-crawler/internal/pages"
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

func (repo *CrawlerRepo) StartCrawler(tChan <-chan *config.Task, workersNum int) {
	for idx := range workersNum {
		go repo.crawl(tChan, idx)
	}
}

func (repo *CrawlerRepo) crawl(tChan <-chan *config.Task, index int) {
	repo.Logger.Infof("Started Crawling goroutine %d", index)

	for task := range tChan {
		repo.processTask(task, index)
	}
}

func (repo *CrawlerRepo) processTask(task *config.Task, index int) {
	defer repo.onTaskDone(task.Run)

	canParse := repo.isAllowedByRobots(task.URL)
	if !canParse {
		repo.Logger.Warnw("Skipping link because of robots.txt", "url", task.URL, "goroutine index", index)
		return
	}

	repo.Logger.Infow("Crawling link", "url", task.URL, "goroutine index", index)

	if task.Run.UseCacheFlag {
		cachedLinks, errCachedLinks := repo.getCachedLinks(task)
		if errCachedLinks == nil {
			repo.sendNewTasksFromLinks(task, cachedLinks)
			return
		}
	}

	fetchRes, errFetch := repo.Networker.Fetch(task.URL)
	if errFetch != nil {
		repo.Logger.Warnw("Failed to fetch link", "url", task.URL, "depth", task.CurrentDepth, "err", errFetch, "goroutine index", index)
		return
	}

	if task.Run.ExtraFlags != nil {
		extra := repo.ExtraWorker.PerformExtraTask(task.URL, task.Run.ExtraFlags)

		if task.Run.ExtraFlags.ParseRenderedHTML {
			fetchRes.Body = extra.HTMLTask
		}
	}

	var linksFromThePage []string
	if strings.HasSuffix(strings.TrimSuffix(task.URL, "/"), ".js") {
		baseURL, err := utils.GetBaseURL(task.URL)

		if err != nil {
			repo.Logger.Warnw("failed to get URL", "url", task.URL, "err", err, "goroutine index", index)
		}

		linksFromThePage, err = repo.Parser.ExtractLinksFromJS(baseURL, string(fetchRes.Body))
		if err != nil {
			repo.Logger.Warnw("failed to extract links from js", "url", task.URL, "err", err, "goroutine index", index)
		}
	} else {
		linksFromThePage = repo.Parser.ParseHTML(fetchRes.Body, task.URL)

		var err error
		jsonLinks, err := repo.Parser.ExtractLinksFromJSON(task.URL, fetchRes.Body)
		linksFromThePage = append(linksFromThePage, jsonLinks...)
		if err != nil {
			repo.Logger.Warnw("failed to extract links from json", "url", task.URL, "err", err, "goroutine index", index)
		}
	}

	pageData := &pages.PageData{
		URL:           task.URL,
		Status:        fetchRes.Status,
		Links:         linksFromThePage,
		LastRunID:     task.Run.ID,
		LastUpdatedAt: time.Now(),
		FoundAt:       time.Now(),
		ContentType:   fetchRes.ContentType,
	}

	errSaving := repo.PageRepo.SavePage(pageData)

	if errSaving != nil {
		repo.Logger.Warnw("Failed to save page", "url", task.URL, "depth", task.CurrentDepth, "err", errSaving, "goroutine index", index)
	}

	errCache := repo.CachePages.Set(task.URL, pageData, cache.BaseTTL)
	if errCache != nil {
		repo.Logger.Warnw("Failed to cache page", "url", task.URL, "depth", task.CurrentDepth, "err", errCache, "goroutine index", index)
	}

	repo.sendNewTasksFromLinks(task, linksFromThePage)
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

	var cachedPageData pages.PageData
	errUnmarshal := json.Unmarshal([]byte(cachedPageRaw), &cachedPageData)

	if errCache == nil && errUnmarshal == nil {
		repo.Logger.Infow("using cached page", "url", task.URL)
		return cachedPageData.Links, nil
	}

	return nil, errors.Join(errCache, errUnmarshal)
}

func (repo *CrawlerRepo) sendNewTasksFromLinks(prevTask *config.Task, links []string) {
	for _, link := range links {
		newTask := &config.Task{
			URL:          link,
			CurrentDepth: prevTask.CurrentDepth + 1,
			Run:          prevTask.Run,
		}

		err := repo.Processor.SendTask(newTask)
		if err != nil {
			repo.Logger.Warnw("failed to send task to the queue", "url", newTask.URL, "depth", prevTask.CurrentDepth, "err", err)
		}
	}
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
