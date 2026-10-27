package webcrawler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"web-crawler/internal/appconfig"
	"web-crawler/internal/documents"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/domain/data"
	"web-crawler/internal/networker"
	"web-crawler/internal/networker/sugaredworker"
	"web-crawler/internal/parser"
	"web-crawler/internal/processor"
	"web-crawler/internal/robots"
	"web-crawler/internal/urlnorm"
	"web-crawler/internal/webcrawler/cache"
	"web-crawler/internal/webcrawler/runstates"

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
	robots          *robots.Checker
	runStateManager runstates.RunStateManager
	documents       documents.Sink
	settings        appconfig.Config

	cfg     *CrawlerConfig
	workers sync.WaitGroup
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
		robots:          robots.NewChecker(logger, networker, cacheRobots, settings.Crawler.RobotsAgent(), settings.Cache.RobotsTTL.Std()),
		runStateManager: runStateManager,
		documents:       documentSink,
		settings:        settings,
	}
}

func (repo *CrawlerRepo) StartCrawler(cfg *CrawlerConfig) {
	repo.cfg = cfg

	for range repo.cfg.WorkersNumber {
		repo.workers.Add(1)
		go repo.crawlWorker(repo.cfg.TaskConsumerChan, repo.cfg.SaverChan)
	}
}

func (repo *CrawlerRepo) crawlWorker(tcChan <-chan *config.Task, saverChan chan<- *data.PageData) {
	defer repo.workers.Done()

	repo.logger.Infof("Started crawlWorker")

	for task := range tcChan {
		repo.handleTask(task, saverChan)
	}
}

// handleTask completes a task only once its work is fully done, including queueing
// every link it found: completion counts it down and acks it, and an acked task is
// never redelivered.
func (repo *CrawlerRepo) handleTask(task *config.Task, saverChan chan<- *data.PageData) {
	task.EnsureID()

	if repo.alreadyHandled(task) {
		task.Ack()
		return
	}

	err := repo.processTask(task, saverChan)

	switch {
	case errors.Is(err, ErrChildrenNotSent):
		repo.logger.Warnw("Links of a page were not queued; leaving the task for redelivery", "url", task.URL, "error", err)
		return
	case err != nil && !errors.Is(err, ErrCacheHit):
		repo.logger.Warnw("Error processing task", "url", task.URL, "error", err)
	}

	repo.completeTask(task)
}

// alreadyHandled skips redelivered tasks that were already completed, and tasks of
// runs that finished or lost their slot. On a Redis error it processes anyway:
// every step after this is idempotent.
func (repo *CrawlerRepo) alreadyHandled(task *config.Task) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	state, err := repo.runStateManager.TaskState(ctx, task.Run.ID, task.ID, repo.settings.RunState.SlotLease.Std())
	if err != nil {
		repo.logger.Warnw("Failed to read task state, processing anyway", "url", task.URL, "error", err)
		return false
	}

	return state.Done || !state.RunActive
}

func (repo *CrawlerRepo) processTask(task *config.Task, saverChan chan<- *data.PageData) error {
	if task.Run.UseCacheFlag {
		cached, errCached := repo.getCachedPage(task)

		if errCached == nil {
			if err := repo.sendChildren(task, cached.PageLinks); err != nil {
				return err
			}

			return ErrCacheHit
		}
	}

	pd, err := repo.processCrawlTask(task)
	if err != nil {
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

	return repo.sendChildren(task, pd.PageLinks)
}

// sendChildren queues the page's links before the page itself is completed. The
// other order lets the run's count reach zero while its children are still
// uncounted, and the run is declared finished mid-crawl.
func (repo *CrawlerRepo) sendChildren(parent *config.Task, links []string) error {
	if parent.CurrentDepth+1 >= parent.Run.MaxDepth {
		return nil
	}

	var failed error

	for _, link := range links {
		if !parent.Run.InScope(link) {
			continue
		}

		child := &config.Task{URL: link, CurrentDepth: parent.CurrentDepth + 1, Run: parent.Run}

		err := repo.cfg.TaskSender.SendTask(child)

		switch {
		case err == nil, errors.Is(err, processor.ErrRunLimitExceeded), errors.Is(err, processor.ErrInvalidURL):
		case errors.Is(err, processor.ErrRunNotActive):
			return nil
		default:
			failed = errors.Join(failed, err)
		}
	}

	if failed != nil {
		return fmt.Errorf("%w: %w", ErrChildrenNotSent, failed)
	}

	return nil
}

// completeTask counts the task down and acks it. Only the completion that takes the
// run to zero gets past the counted check - across all nodes - and it frees the
// slot directly. A failed count is not acked, so the task is redelivered; counting
// is idempotent, so the redelivery cannot count it twice.
func (repo *CrawlerRepo) completeTask(task *config.Task) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	left, counted, err := repo.runStateManager.CompleteTask(ctx, task.Run.ID, task.ID, repo.settings.RunState.SlotLease.Std())
	if err != nil {
		repo.logger.Errorw("Failed to complete task, leaving it for redelivery", "url", task.URL, "runID", task.Run.ID, "error", err)
		return
	}

	task.Ack()

	if !counted || left > 0 {
		return
	}

	if err := repo.runStateManager.ReleaseRunSlot(ctx, task.Run.ID); err != nil {
		repo.logger.Errorw("Failed to release run slot; it frees itself when the lease expires", "runID", task.Run.ID, "error", err)
	}

	if err := repo.runStateManager.CleanupRun(ctx, task.Run.ID); err != nil {
		repo.logger.Warnw("Failed to clean up run state", "runID", task.Run.ID, "error", err)
	}

	repo.logger.Infow("Run finished", "runID", task.Run.ID)
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

	pageURL, alias := pageIdentity(task.URL, fetchRes)
	if alias != "" && !repo.isAllowedByRobots(pageURL) {
		repo.logger.Warnw("Redirected to a page robots.txt disallows", "url", task.URL, "final", pageURL)
		return nil, ErrNotAllowedByRobots
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
		BaseURL:     linkBase(task.URL, fetchRes),
		ContentType: fetchRes.ContentType,
	})
	if errParse != nil {
		repo.logger.Warnw("Failed to parse page", "url", task.URL, "err", errParse)

		parsed = &parser.ParseResult{}
	}

	// After a temporary redirect elsewhere the body is another page's - a login
	// form, a mirror - and indexing it under pageURL would make pageURL findable by
	// text it does not have. That page is indexed when it is crawled itself.
	if final, ok := urlnorm.Canonical(fetchRes.FinalURL, nil); !ok || final == pageURL {
		repo.submitDocument(ctx, pageURL, task.Run.ID, fetchRes, parsed)
	}

	pageData := &data.PageData{
		URL:           pageURL,
		Status:        fetchRes.Status,
		Title:         parsed.Title,
		Links:         parsed.AllURLs(),
		PageLinks:     parsed.URLsOfKind(parser.LinkPage),
		LastRunID:     task.Run.ID,
		LastUpdatedAt: time.Now(),
		FoundAt:       time.Now(),
		ContentType:   fetchRes.ContentType,
	}

	if alias != "" {
		pageData.Aliases = []string{alias}
		repo.removeDocument(ctx, alias, task.Run.ID)
	}

	return pageData, nil
}

// pageIdentity names the page a fetch returned. When the requested URL turned out
// to be another name for a different one, the page is stored under that one and
// the requested URL comes back as its alias.
func pageIdentity(requested string, fetchRes *networker.FetchResult) (pageURL, alias string) {
	if canonical, ok := urlnorm.Canonical(requested, nil); ok {
		requested = canonical
	}

	identity, ok := urlnorm.Canonical(fetchRes.IdentityURL, nil)
	if !ok || identity == requested {
		return requested, ""
	}

	return identity, requested
}

// linkBase is where the document really came from, whatever it is stored under:
// relative links resolve against it.
func linkBase(requested string, fetchRes *networker.FetchResult) string {
	if fetchRes.FinalURL == "" {
		return requested
	}

	return fetchRes.FinalURL
}

func (repo *CrawlerRepo) submitDocument(ctx context.Context, pageURL, runID string, fetchRes *networker.FetchResult, parsed *parser.ParseResult) {
	doc := &documents.Document{
		URL:         pageURL,
		Title:       parsed.Title,
		Content:     parsed.Content,
		ContentType: fetchRes.ContentType,
		RunID:       runID,
		FetchedAt:   time.Now(),
	}

	switch {
	case fetchRes.Status == http.StatusNotFound || fetchRes.Status == http.StatusGone:
		// The page is gone: an empty document removes whatever was indexed for it.
		doc.Title, doc.Content = "", ""
	case fetchRes.Status < 200 || fetchRes.Status >= 300:
		// Other errors are usually transient (5xx, 429, 403 from a rate limiter), and
		// an error body is not page content. Leave the indexed version alone.
		return
	case strings.TrimSpace(parsed.Content) == "":
		// Empty text on a live page more likely means extraction failed than that the
		// page is empty - and the legacy parser extracts no text at all. Treating it
		// as a removal would wipe the index.
		return
	}

	if err := repo.documents.Submit(ctx, doc); err != nil {
		repo.logger.Warnw("Failed to submit document", "url", pageURL, "err", err)
	}
}

// removeDocument drops whatever was indexed under a URL that is now only an alias.
func (repo *CrawlerRepo) removeDocument(ctx context.Context, pageURL, runID string) {
	doc := &documents.Document{URL: pageURL, RunID: runID, FetchedAt: time.Now()}

	if err := repo.documents.Submit(ctx, doc); err != nil {
		repo.logger.Warnw("Failed to remove alias from the index", "url", pageURL, "err", err)
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

func (repo *CrawlerRepo) isAllowedByRobots(urlToCheck string) bool {
	return repo.robots.Allowed(urlToCheck)
}

// Shutdown expects the task input to be closed already: workers only exit when it
// is. The document sink is shut down after them, so no worker submits into a
// closed sink.
func (repo *CrawlerRepo) Shutdown(ctx context.Context) error {
	workersDone := make(chan struct{})
	go func() {
		repo.workers.Wait()
		close(workersDone)
	}()

	var errWait error

	select {
	case <-workersDone:
	case <-ctx.Done():
		errWait = fmt.Errorf("crawl workers still running at shutdown deadline: %w", ctx.Err())
	}

	go repo.networker.Stop()

	shutdowns := []func(context.Context) error{
		repo.extraWorker.Shutdown,
		repo.documents.Shutdown,
		repo.cachePages.Stop,
		repo.cacheRobots.Stop,
	}

	eg := &errgroup.Group{}
	for _, shutdown := range shutdowns {
		eg.Go(func() error {
			return shutdown(ctx)
		})
	}

	return errors.Join(errWait, eg.Wait())
}
