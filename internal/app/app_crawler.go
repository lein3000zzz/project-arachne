package app

import (
	"context"
	"time"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/pages"
	"web-crawler/internal/processor"
	"web-crawler/internal/utils"
	"web-crawler/internal/webcrawler"
	"web-crawler/internal/webcrawler/runstates"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/zap"
)

type CrawlerApp struct {
	logger          *zap.SugaredLogger
	crawler         webcrawler.Crawler
	processorQueue  processor.Processor
	pageRepo        pages.PageRepo
	runStateManager runstates.RunStateManager
	tracer          *trace.TracerProvider

	maxConcurrentRuns int
}

func NewCrawlerApp(logger *zap.SugaredLogger, crawler webcrawler.Crawler, pagesRepo pages.PageRepo, processorQueue processor.Processor, runStateManager runstates.RunStateManager, tp *trace.TracerProvider) *CrawlerApp {
	return &CrawlerApp{
		logger:            logger,
		crawler:           crawler,
		processorQueue:    processorQueue,
		pageRepo:          pagesRepo,
		runStateManager:   runStateManager,
		maxConcurrentRuns: DefaultConcurrentRunsWorkers,
		tracer:            tp,
	}
}

func (app *CrawlerApp) StartApp() error {
	err := app.pageRepo.EnsureConnectivity()
	if err != nil {
		app.logger.Errorf("Error ensuring connectivity: %v", err)
		return err
	}

	crawlerCBChan := make(chan struct{}, 1)
	taskProducerChan := make(chan []*config.Task, 100)

	crawlerCfg := app.buildCrawlerConfig(crawlerCBChan, taskProducerChan)

	go app.processorQueue.StartRunConsumer()
	go app.processorQueue.StartTaskConsumer()
	go app.startTaskProducer(taskProducerChan)

	go app.startCrawlerCallbackListener(crawlerCBChan)
	go app.pageRepo.StartSaverWorkers(DefaultConcurrentTasksWorkers / 2)

	go app.crawler.StartCrawler(&crawlerCfg)

	go app.startRunListener()

	return nil
}

func (app *CrawlerApp) startRunListener() {
	// TODO сделать нормальный flow с graceful shutdown'ами, блекджеком и тд
	// TODO сделать StopApp!!
	defer app.tracer.Shutdown(context.Background())

	tracer := otel.Tracer("app")
	_, span := tracer.Start(context.Background(), "RunListener")
	defer span.End()

	span.SetAttributes(attribute.Int("max_concurrent_runs", app.maxConcurrentRuns))

	runsChan := app.processorQueue.GetRunsChan()

	for run := range runsChan {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

		acquired := false
		for !acquired {
			var err error
			_, subSpan := tracer.Start(ctx, "AcquireSlot")
			subSpan.SetAttributes(
				attribute.String("run_id", run.ID),
				attribute.Int("max_concurrent_runs", app.maxConcurrentRuns),
			)
			acquired, err = app.runStateManager.AcquireRunSlot(ctx, app.maxConcurrentRuns)
			subSpan.SetAttributes(attribute.Bool("acquired", acquired))
			if err != nil {
				// мне не нравится, но пока сойдет, не забыть переделать TODO
				subSpan.RecordError(err)
				subSpan.SetStatus(codes.Error, "Slot acquisition failed")
				app.logger.Errorw("Error acquiring run slot", "error", err)
				subSpan.End()
				cancel()
				continue
			}
			subSpan.SetStatus(codes.Ok, "Slot checked")
			subSpan.End()

			if !acquired {
				select {
				case <-ctx.Done():
					app.logger.Warnw("Timeout waiting for run slot", "runID", run.ID)
					cancel()
					app.processorQueue.QueueRun(run)
					continue
				case <-time.After(1 * time.Second):
					// TODO do sth, mb log the warning that this would retry
				}
			}
		}
		cancel()

		firstTask := &config.Task{
			URL:          utils.CorrectURLScheme(run.StartURL),
			Run:          run,
			CurrentDepth: 0,
		}

		err := app.processorQueue.SendTask(firstTask)
		if err != nil {
			app.logger.Errorf("Error sending task: %v", err)

			span.RecordError(err)
			span.SetStatus(codes.Error, "Run's first task send failed")

			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if releaseErr := app.runStateManager.ReleaseRunSlot(releaseCtx); releaseErr != nil {
				app.logger.Errorw("Failed to release run slot", "error", releaseErr)
			}
			releaseCancel()

			continue
		}
	}

	span.SetStatus(codes.Ok, "Run listener exited")
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

func (app *CrawlerApp) buildCrawlerConfig(crawlerCBChan chan<- struct{}, tpChan chan<- []*config.Task) webcrawler.CrawlerConfig {
	crawlerCfg := webcrawler.CrawlerConfig{
		TaskConsumerChan:  app.processorQueue.GetTasksChan(),
		SaverChan:         app.pageRepo.GetSaverChan(),
		TaskProducerChan:  tpChan,
		CrawlCallbackChan: crawlerCBChan,
		WorkersNumber:     DefaultConcurrentTasksWorkers,
	}

	return crawlerCfg
}

func (app *CrawlerApp) startTaskProducer(tpChan <-chan []*config.Task) {
	for tasks := range tpChan {
		for _, task := range tasks {
			err := app.processorQueue.SendTask(task)
			if err != nil {
				return
			}
		}
	}
}
