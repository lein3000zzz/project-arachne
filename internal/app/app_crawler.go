package app

import (
	"context"
	"errors"
	"time"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/pages"
	"web-crawler/internal/processor"
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
	taskProducerChan  chan []*config.Task
}

func NewCrawlerApp(logger *zap.SugaredLogger, crawler webcrawler.Crawler, pagesRepo pages.PageRepo, processorQueue processor.Processor, runStateManager runstates.RunStateManager, tp *tracesdk.TracerProvider) *CrawlerApp {
	return &CrawlerApp{
		logger:            logger,
		crawler:           crawler,
		processorQueue:    processorQueue,
		pageRepo:          pagesRepo,
		runStateManager:   runStateManager,
		maxConcurrentRuns: DefaultConcurrentRunsWorkers,
		taskProducerChan:  make(chan []*config.Task, 100),
		tracerProvider:    tp,
	}
}

func (app *CrawlerApp) StartApp(ctx context.Context) error {
	err := app.pageRepo.EnsureConnectivity()
	if err != nil {
		app.logger.Errorf("Error ensuring connectivity: %v", err)
		return err
	}

	crawlerCBChan := make(chan struct{}, 1)

	crawlerCfg := app.buildCrawlerConfig(crawlerCBChan)

	go app.processorQueue.StartRunConsumer()
	go app.processorQueue.StartTaskConsumer()
	go app.startTaskProducer()

	go app.startCrawlerCallbackListener(crawlerCBChan)
	go app.pageRepo.StartSaverWorkers(DefaultConcurrentTasksWorkers / 2)

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
		select {
		case <-ctx.Done():
			app.logger.Info("Context cancelled, dropping pending run and exiting")
			return
		default:
			app.processRun(ctx, tracer, run)
		}
	}

	span.AddEvent("Runs channel closed, exiting RunListener...")
	app.logger.Info("Runs channel closed, exiting RunListener...")
}

func (app *CrawlerApp) processRun(ctx context.Context, tracer trace.Tracer, run *config.Run) {
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	err := app.waitForRunSlot(runCtx, tracer, run)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			app.logger.Warnw("Timeout waiting for run slot, re-queueing", "runID", run.ID)
			app.processorQueue.QueueRun(run)
		} else if !errors.Is(err, context.Canceled) {
			app.logger.Errorw("Failed to acquire slot", "runID", run.ID, "error", err)
		}
		return
	}

	firstTask := &config.Task{
		URL:          utils.CorrectURLScheme(run.StartURL),
		Run:          run,
		CurrentDepth: 0,
	}

	if err := app.processorQueue.SendTask(firstTask); err != nil {
		app.handleDispatchError(ctx, tracer, run, err)
	}
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

		acquired, err := app.runStateManager.AcquireRunSlot(ctx, app.maxConcurrentRuns)
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

	if releaseErr := app.runStateManager.ReleaseRunSlot(releaseCtx); releaseErr != nil {
		app.logger.Errorw("Failed to release run slot after dispatch error", "error", releaseErr)
		span.RecordError(releaseErr)
	}
}

func (app *CrawlerApp) startCrawlerCallbackListener(sigChan <-chan struct{}) {
	for range sigChan {
		app.logger.Infof("Received crawler callback signal, run ended")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := app.runStateManager.ReleaseRunSlot(ctx)
		cancel()

		if err != nil {
			app.logger.Errorw("Failed to release run slot", "error", err)
		} else {
			app.logger.Infof("Run slot released via Redis semaphore")
		}
	}
}

func (app *CrawlerApp) buildCrawlerConfig(crawlerCBChan chan<- struct{}) *webcrawler.CrawlerConfig {
	crawlerCfg := &webcrawler.CrawlerConfig{
		TaskConsumerChan:  app.processorQueue.GetTasksChan(),
		SaverChan:         app.pageRepo.GetSaverChan(),
		TaskProducerChan:  app.taskProducerChan,
		CrawlCallbackChan: crawlerCBChan,
		WorkersNumber:     DefaultConcurrentTasksWorkers,
	}

	return crawlerCfg
}

func (app *CrawlerApp) startTaskProducer() {
	for tasks := range app.taskProducerChan {
		for _, task := range tasks {
			err := app.processorQueue.SendTask(task)
			if err != nil {
				return
			}
		}
	}
}

func (app *CrawlerApp) StopApp(ctx context.Context) error {
	shutdowns := []func(context.Context) error{
		app.crawler.Shutdown,
		app.processorQueue.StopProcessor,
		app.pageRepo.Shutdown,
		app.tracerProvider.Shutdown,
		//app.runStateManager.ReleaseRunSlot, // опционально. Если на других машинах все еще продолжается работа, то не надо.
		// Но надо будет добавить дополнительный счетчик активных машин вообще
		app.runStateManager.Stop,
	}

	eg := &errgroup.Group{}
	for _, shutdown := range shutdowns {
		// shutdown := shutdown // если я когда-то зачем-то решу пойти на более старую версию гошки
		eg.Go(func() error {
			subCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			return shutdown(subCtx)
		})
	}

	return eg.Wait()
}
