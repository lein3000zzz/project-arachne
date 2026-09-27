package app

import (
	"context"
	"errors"
	"time"
	"web-crawler/internal/appconfig"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/pages"
	"web-crawler/internal/processor"
	"web-crawler/internal/processor/queue"
	"web-crawler/internal/utils"
	"web-crawler/internal/webcrawler"
	"web-crawler/internal/webcrawler/runstates"

	"github.com/lein3000zzz/vault-config-manager/pkg/manager"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type CrawlerApp struct {
	logger          *zap.SugaredLogger
	crawler         webcrawler.Crawler
	processorQueue  processor.Processor
	pageRepo        pages.PageRepo
	runStateManager runstates.RunStateManager
	tracerProvider  *tracesdk.TracerProvider
	secretManager   manager.SecretManager

	maxConcurrentRuns int
	cfg               appconfig.Config
}

func NewCrawlerApp(logger *zap.SugaredLogger, crawler webcrawler.Crawler, pagesRepo pages.PageRepo, processorQueue processor.Processor, runStateManager runstates.RunStateManager, tp *tracesdk.TracerProvider, cfg appconfig.Config) *CrawlerApp {
	return &CrawlerApp{
		logger:            logger,
		crawler:           crawler,
		processorQueue:    processorQueue,
		pageRepo:          pagesRepo,
		runStateManager:   runStateManager,
		maxConcurrentRuns: cfg.Crawler.RunWorkers,
		tracerProvider:    tp,
		cfg:               cfg,
	}
}

func (app *CrawlerApp) StartApp(ctx context.Context) error {
	err := app.pageRepo.EnsureConnectivity()
	if err != nil {
		app.logger.Errorf("Error ensuring connectivity: %v", err)
		return err
	}

	crawlerCfg := app.buildCrawlerConfig()

	go app.processorQueue.StartRunConsumer()
	go app.processorQueue.StartTaskConsumer()

	go app.pageRepo.StartSaverWorkers(app.cfg.Crawler.SaverWorkers)

	go app.crawler.StartCrawler(crawlerCfg)

	go app.startRunListener(ctx)

	return nil
}

func (app *CrawlerApp) startRunListener(ctx context.Context) {
	tracer := app.tracerProvider.Tracer("CrawlerApp.RunListener")

	ctx, span := tracer.Start(ctx, "RunListener")
	defer span.End()

	span.SetAttributes(attribute.Int("max_concurrent_runs", app.maxConcurrentRuns))
	app.logger.Info("Run listener started")

	runsChan := app.processorQueue.GetRunsChan()

	for run := range runsChan {
		if ctx.Err() != nil {
			app.logger.Info("Shutting down; pending runs are redelivered after restart")
			return
		}

		if app.processRun(ctx, tracer, run) {
			run.Ack()
		}
	}

	span.AddEvent("Runs channel closed, exiting RunListener...")
	app.logger.Info("Runs channel closed, exiting RunListener...")
}

// processRun reports whether the run message is finished with. It is not when
// shutdown interrupts it: left unacked, the run is redelivered after the restart.
func (app *CrawlerApp) processRun(ctx context.Context, tracer trace.Tracer, run *config.Run) bool {
	if run.MaxDepth < 1 || run.MaxLinks < 1 {
		app.logger.Warnw("Dropping run that can never crawl anything", "runID", run.ID, "maxDepth", run.MaxDepth, "maxLinks", run.MaxLinks)
		return true
	}

	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := app.waitForRunSlot(runCtx, tracer, run); err != nil {
		if ctx.Err() != nil {
			return false
		}

		app.logger.Warnw("No run slot yet, re-queueing", "runID", run.ID, "error", err)
		app.processorQueue.QueueRun(run)

		return true
	}

	firstTask := &config.Task{
		URL:          utils.CorrectURLScheme(run.StartURL),
		Run:          run,
		CurrentDepth: 0,
	}

	if err := app.processorQueue.SendTask(firstTask); err != nil {
		app.handleDispatchError(ctx, tracer, run, err)

		if ctx.Err() != nil || errors.Is(err, queue.ErrQueueClosed) {
			return false
		}

		app.processorQueue.QueueRun(run)
	}

	return true
}

func (app *CrawlerApp) waitForRunSlot(ctx context.Context, tracer trace.Tracer, run *config.Run) error {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		_, span := tracer.Start(ctx, "AcquireSlot")
		span.SetAttributes(
			attribute.String("run_id", run.ID),
			attribute.Int("max_concurrent_runs", app.maxConcurrentRuns),
		)

		acquired, _, err := app.runStateManager.AcquireRunSlot(ctx, run.ID, app.maxConcurrentRuns, app.cfg.RunState.SlotLease.Std())
		span.SetAttributes(attribute.Bool("acquired", acquired))

		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "Slot acquisition error")
			span.End()
			return err
		}

		if acquired {
			span.SetStatus(codes.Ok, "Slot acquired")
			span.End()
			return nil
		}

		span.End()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			continue
		}
	}
}

func (app *CrawlerApp) handleDispatchError(ctx context.Context, tracerProvider trace.Tracer, run *config.Run, err error) {
	app.logger.Errorf("Error sending first task for run %s: %v", run.ID, err)

	_, span := tracerProvider.Start(ctx, "HandleDispatchError")
	defer span.End()

	span.RecordError(err)
	span.SetStatus(codes.Error, "First task send failed")

	releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if releaseErr := app.runStateManager.ReleaseRunSlot(releaseCtx, run.ID); releaseErr != nil {
		app.logger.Errorw("Failed to release run slot after dispatch error", "error", releaseErr)
		span.RecordError(releaseErr)
	}
}

func (app *CrawlerApp) buildCrawlerConfig() *webcrawler.CrawlerConfig {
	return &webcrawler.CrawlerConfig{
		TaskConsumerChan: app.processorQueue.GetTasksChan(),
		SaverChan:        app.pageRepo.GetSaverChan(),
		TaskSender:       app.processorQueue,
		WorkersNumber:    app.cfg.Crawler.TaskWorkers,
	}
}

func (app *CrawlerApp) ShutdownTimeout() time.Duration {
	return app.cfg.Shutdown.Timeout.Std()
}

// StopApp shuts down in stages because each stage closes channels the next one's
// goroutines write to. Stopping the queues closes the crawl workers' input; the
// crawler waits for them before closing its own outputs; the page savers' input is
// closed only after that.
func (app *CrawlerApp) StopApp(ctx context.Context) error {
	stages := [][]func(context.Context) error{
		{app.processorQueue.StopProcessor},
		{app.crawler.Shutdown},
		{
			app.pageRepo.Shutdown,
			app.tracerProvider.Shutdown,
			// app.runStateManager.ReleaseRunSlot, // Optional for now. Vital for distributed setups, but more work will
			// be needed here for sure
			app.runStateManager.Stop,
		},
	}

	var errs []error

	for _, stage := range stages {
		eg := &errgroup.Group{}
		for _, shutdown := range stage {
			eg.Go(func() error {
				subCtx, cancel := context.WithTimeout(ctx, app.cfg.Shutdown.ComponentTimeout.Std())
				defer cancel()

				return shutdown(subCtx)
			})
		}

		errs = append(errs, eg.Wait())
	}

	return errors.Join(errs...)
}
