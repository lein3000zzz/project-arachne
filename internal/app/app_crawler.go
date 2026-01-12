package app

import (
	"web-crawler/internal/domain/config"
	"web-crawler/internal/pages"
	"web-crawler/internal/processor"
	"web-crawler/internal/utils"
	"web-crawler/internal/webcrawler"

	"go.uber.org/zap"
)

type CrawlerApp struct {
	logger         *zap.SugaredLogger
	crawler        webcrawler.Crawler
	processorQueue processor.Processor
	pageRepo       pages.PageRepo

	runLimiter chan struct{}
}

func NewCrawlerApp(logger *zap.SugaredLogger, crawler webcrawler.Crawler, pagesRepo pages.PageRepo, processorQueue processor.Processor) *CrawlerApp {
	return &CrawlerApp{
		logger:         logger,
		crawler:        crawler,
		processorQueue: processorQueue,
		pageRepo:       pagesRepo,

		// Вот эта сказка гарантирует последовательность ранов
		runLimiter: make(chan struct{}, DefaultConcurrentRunsWorkers),
	}
}

func (app *CrawlerApp) StartApp() error {
	err := app.pageRepo.EnsureConnectivity()
	if err != nil {
		app.logger.Errorf("Error ensuring connectivity: %v", err)
		return err
	}

	callbackChan := app.crawler.GetCallbackChan()

	go app.processorQueue.StartRunConsumer()
	go app.processorQueue.StartTaskConsumer()

	go app.crawler.StartCrawler()

	go app.startCrawlerCallbackListener(callbackChan)
	go app.startRunListener()

	return nil
}

func (app *CrawlerApp) startRunListener() {
	runsChan := app.processorQueue.GetRunsChan()

	for run := range runsChan {
		app.runLimiter <- struct{}{}

		firstTask := &config.Task{
			URL:          utils.CorrectURLScheme(run.StartURL),
			Run:          run,
			CurrentDepth: 0,
		}

		err := app.processorQueue.SendTask(firstTask)
		if err != nil {
			app.logger.Errorf("Error sending task: %v", err)
			<-app.runLimiter
			continue
		}
	}
}

func (app *CrawlerApp) startCrawlerCallbackListener(sigChan <-chan struct{}) {
	for range sigChan {
		app.logger.Infof("Received crawler callback signal, run ended")
		
		select {
		case <-app.runLimiter:
			app.logger.Infof("semaphore released")
		default:
			app.logger.Warnw("tried to release runs semaphore yet it had already been released")
		}
	}
}
