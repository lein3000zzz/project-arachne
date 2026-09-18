package webcrawler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"web-crawler/internal/appconfig"
	"web-crawler/internal/documents"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/domain/data"
	"web-crawler/internal/networker"
	"web-crawler/internal/networker/sugaredworker"
	"web-crawler/internal/parser"
	"web-crawler/internal/utils"
	"web-crawler/internal/webcrawler/cache"
	"web-crawler/internal/webcrawler/runstates"

	"github.com/jimsmart/grobotstxt"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type CrawlerRepo struct {
	logger          *zap.SugaredLogger
	parser          parser.Parser
	networker       networker.Networker
	extraWorker     sugaredworker.SugaredWorker
	cachePages      cache.CachedStorage
	cacheRobots     cache.CachedStorage
	runStateManager runstates.RunStateManager
	documents       documents.Sink
	settings        appconfig.Config

	cfg *CrawlerConfig
}

func NewCrawlerRepo(
	logger *zap.SugaredLogger,
	pageParser parser.Parser,
	networker networker.Networker,
	extraWorker sugaredworker.SugaredWorker,
	cachePages cache.CachedStorage,
	cacheRobots cache.CachedStorage,
	runStateManager runstates.RunStateManager,
	documentSink documents.Sink,
	settings appconfig.Config,
) *CrawlerRepo {
	return &CrawlerRepo{
		logger:          logger,
		parser:          pageParser,
		networker:       networker,
		extraWorker:     extraWorker,
		cachePages:      cachePages,
		cacheRobots:     cacheRobots,
		runStateManager: runStateManager,
		documents:       documentSink,
		settings:        settings,
	}
}

func (repo *CrawlerRepo) StartCrawler(cfg *CrawlerConfig) {
	repo.cfg = cfg

	for range repo.cfg.WorkersNumber {
		go repo.crawlWorker(repo.cfg.TaskConsumerChan, repo.cfg.TaskProducerChan, repo.cfg.SaverChan)
	}
}

func (repo *CrawlerRepo) crawlWorker(tcChan <-chan *config.Task, tpChan chan<- []*config.Task, saverChan chan<- *data.PageData) {
	repo.logger.Infof("Started crawlWorker")

	for task := range tcChan {
		err := repo.processTask(task, tpChan, saverChan)
		if err != nil && !errors.Is(err, ErrCacheHit) {
			repo.logger.Warnf("Error processing task: %s", err)
			continue
		}
	}
}

func (repo *CrawlerRepo) processTask(task *config.Task, tpChan chan<- []*config.Task, saverChan chan<- *data.PageData) error {
	defer repo.onTaskDone(task.Run)

	if task.Run.UseCacheFlag {
		cached, errCached := repo.getCachedPage(task)

		if errCached == nil {
			tpChan <- repo.createNewTasksFromLinks(task, cached.PageLinks)
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
	case <-time.After(repo.settings.Crawler.SaverSendTimeout.Std()):
		repo.logger.Warnw("Saver channel full, dropping page data", "url", task.URL)
	}

	errCache := repo.cachePages.Set(task.URL, pd, repo.settings.Cache.PageTTL.Std())
	if errCache != nil {
		repo.logger.Warnw("Failed to cache page", "url", task.URL, "depth", task.CurrentDepth, "err", errCache)
	}

	newTasks := repo.createNewTasksFromLinks(task, pd.PageLinks)

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

	if task.Run.ExtraFlags != nil {
		extra := repo.extraWorker.PerformExtraTask(task.URL, task.Run.ExtraFlags)

		if task.Run.ExtraFlags.ParseRenderedHTML {
			fetchRes.Body = extra.HTMLTask
		}
	}

	ctx := context.Background()

	parsed, errParse := repo.parser.Parse(ctx, &parser.ParseParams{
		Body:        fetchRes.Body,
		BaseURL:     task.URL,
		ContentType: fetchRes.ContentType,
	})
	if errParse != nil {
		repo.logger.Warnw("Failed to parse page", "url", task.URL, "err", errParse)

		parsed = &parser.ParseResult{}
	}

	repo.submitDocument(ctx, task, fetchRes.ContentType, parsed)

	pageData := &data.PageData{
		URL:           task.URL,
		Status:        fetchRes.Status,
		Title:         parsed.Title,
		Links:         parsed.AllURLs(),
		PageLinks:     parsed.URLsOfKind(parser.LinkPage),
		LastRunID:     task.Run.ID,
		LastUpdatedAt: time.Now(),
		FoundAt:       time.Now(),
		ContentType:   fetchRes.ContentType,
	}

	return pageData, nil
}

func (repo *CrawlerRepo) submitDocument(ctx context.Context, task *config.Task, contentType string, parsed *parser.ParseResult) {
	if strings.TrimSpace(parsed.Content) == "" {
		return
	}

	err := repo.documents.Submit(ctx, &documents.Document{
		URL:         task.URL,
		Title:       parsed.Title,
		Content:     parsed.Content,
		ContentType: contentType,
		RunID:       task.Run.ID,
		FetchedAt:   time.Now(),
	})
	if err != nil {
		repo.logger.Warnw("Failed to submit document", "url", task.URL, "err", err)
	}
}

func (repo *CrawlerRepo) onTaskDone(run *config.Run) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	left, err := repo.runStateManager.DecrementActiveTasks(ctx, run.ID)
	if err != nil {
		repo.logger.Errorw("Failed to decrement active tasks in Redis", "runID", run.ID, "error", err)
		return
	}

	if left == 0 {
		acquired, lockErr := repo.runStateManager.AcquireRunCompletionLock(ctx, run.ID, repo.settings.RunState.LockTTL.Std())
		if lockErr != nil {
			repo.logger.Errorw("Failed to acquire completion lock", "runID", run.ID, "error", lockErr)
			return
		}

		if acquired {
			select {
			case repo.cfg.CrawlCallbackChan <- struct{}{}:
				repo.logger.Infow("Run finished; released run slot", "runID", run.ID)
			default:
				repo.logger.Infow("Run finished, but the run slot was already empty for some reason", "runID", run.ID)
			}

			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cleanupCancel()

			if cleanupErr := repo.runStateManager.CleanupRun(cleanupCtx, run.ID); cleanupErr != nil {
				repo.logger.Warnw("Failed to cleanup run state", "runID", run.ID, "error", cleanupErr)
			}
		} else {
			repo.logger.Debugw("Another node is handling run completion", "runID", run.ID)
		}
	}
}

func (repo *CrawlerRepo) getCachedPage(task *config.Task) (*data.PageData, error) {
	cachedRaw, errCache := repo.cachePages.Get(task.URL)
	if errCache != nil {
		return nil, errCache
	}

	cached := new(data.PageData)
	if errUnmarshal := json.Unmarshal([]byte(cachedRaw), cached); errUnmarshal != nil {
		return nil, errUnmarshal
	}

	repo.logger.Infow("using cached page", "url", task.URL)

	return cached, nil
}

func (repo *CrawlerRepo) createNewTasksFromLinks(prevTask *config.Task, links []string) []*config.Task {
	newTasks := make([]*config.Task, len(links))
	newDepth := prevTask.CurrentDepth + 1

	for i, link := range links {
		newTask := &config.Task{
			URL:          link,
			CurrentDepth: newDepth,
			Run:          prevTask.Run,
		}

		newTasks[i] = newTask
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

	errSaveCache := repo.cacheRobots.Set(baseURL, string(responseData.Body), repo.settings.Cache.RobotsTTL.Std())
	if errSaveCache != nil {
		repo.logger.Warnw("failed to save cache", "url", baseURL, "err", errSaveCache)
	}

	return grobotstxt.AgentAllowed(robots, "project-arachne", urlToCheck)
}

func (repo *CrawlerRepo) Shutdown(ctx context.Context) error {
	go repo.networker.Stop()

	shutdowns := []func(context.Context) error{
		repo.extraWorker.Shutdown,
		repo.documents.Shutdown,
		repo.cachePages.Stop,
		repo.cacheRobots.Stop,
	}

	eg := &errgroup.Group{}
	for _, shutdown := range shutdowns {
		// shutdown := shutdown // птср после старых версий гошки
		eg.Go(func() error {
			subCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			return shutdown(subCtx)
		})
	}

	close(repo.cfg.CrawlCallbackChan)
	close(repo.cfg.TaskProducerChan)

	return eg.Wait()
}
