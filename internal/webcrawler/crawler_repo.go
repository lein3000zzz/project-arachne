package webcrawler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
	"web-crawler/internal/networker"
	"web-crawler/internal/networker/sugaredworker"
	"web-crawler/internal/pageparser"
	"web-crawler/internal/pages"
	"web-crawler/internal/processor"
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
	Processor   processor.Processor
	PageRepo    pages.PageDataRepo
	CachePages  cache.CachedStorage
	CacheRobots cache.CachedStorage

	RunLimiter chan struct{}
}

func NewCrawlerRepo(logger *zap.SugaredLogger, parser pageparser.PageParser, networker networker.Networker, extraWorker sugaredworker.SugaredWorker, pageRepo pages.PageDataRepo, cachePages cache.CachedStorage, cacheRobots cache.CachedStorage, processor processor.Processor) *CrawlerRepo {
	return &CrawlerRepo{
		Logger:      logger,
		Parser:      parser,
		Networker:   networker,
		ExtraWorker: extraWorker,
		PageRepo:    pageRepo,
		Processor:   processor,
		CachePages:  cachePages,
		CacheRobots: cacheRobots,

		// Вот эта сказка гарантирует последовательность ранов
		RunLimiter: make(chan struct{}, ConcurrentRunsLimit),
	}
}

func (repo *CrawlerRepo) StartCrawler() error {
	err := repo.PageRepo.EnsureConnectivity()
	if err != nil {
		repo.Logger.Errorf("Error ensuring connectivity: %v", err)
		return err
	}

	for idx := range ConcurrentTasksLimit {
		go repo.crawl(idx)
	}

	return nil
}

func (repo *CrawlerRepo) crawl(index int) {
	repo.Logger.Infof("Started Crawling goroutine %d", index)
	for {
		task, err := repo.Processor.GetTask()
		if err != nil {
			repo.Logger.Errorw("Error getting task", "err", err, "goroutine index", index)
			continue
		}

		repo.processTask(task, index)
	}
}

func (repo *CrawlerRepo) processTask(task *processor.Task, index int) {
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

	linksFromThePage := repo.Parser.ParseLinks(fetchRes.Body, task.URL)

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

func (repo *CrawlerRepo) onTaskDone(run *processor.Run) {
	left := run.DecrementActiveWithMutex()
	if left == 0 {
		select {
		case <-repo.RunLimiter:
			repo.Logger.Infow("Run finished; released run slot", "runID", run.ID)
		default:
			repo.Logger.Infow("Run finished, but the run slot was already empty for some reason", "runID", run.ID)
		}
	}
}

func (repo *CrawlerRepo) getCachedLinks(task *processor.Task) ([]string, error) {
	cachedPageRaw, errCache := repo.CachePages.Get(task.URL)

	var cachedPageData pages.PageData
	errUnmarshal := json.Unmarshal([]byte(cachedPageRaw), &cachedPageData)

	if errCache == nil && errUnmarshal == nil {
		repo.Logger.Infow("using cached page", "url", task.URL)
		return cachedPageData.Links, nil
	}

	return nil, errors.Join(errCache, errUnmarshal)
}

func (repo *CrawlerRepo) sendNewTasksFromLinks(prevTask *processor.Task, links []string) {
	for _, link := range links {
		newTask := &processor.Task{
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

func (repo *CrawlerRepo) StartRun() error {
	repo.RunLimiter <- struct{}{}

	run, err := repo.Processor.GetRun()
	if err != nil {
		repo.Logger.Errorf("Error getting run: %v", err)
		<-repo.RunLimiter
		return err
	}

	firstTask := &processor.Task{
		URL:          utils.CorrectURLScheme(run.StartURL),
		Run:          run,
		CurrentDepth: 0,
	}

	err = repo.Processor.SendTask(firstTask)
	if err != nil {
		repo.Logger.Errorf("Error sending task: %v", err)
		<-repo.RunLimiter
		return err
	}

	return nil
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

	errSaveCache := repo.CacheRobots.Set(urlToCheck, string(responseData.Body), cache.BaseTTL)
	if errSaveCache != nil {
		repo.Logger.Warnw("failed to save cache", "url", baseURL, "err", errSaveCache)
	}

	return grobotstxt.AgentAllowed(robots, "project-arachne", urlToCheck)
}
