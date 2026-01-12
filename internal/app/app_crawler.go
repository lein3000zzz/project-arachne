package app

import (
	"web-crawler/internal/pages"
	"web-crawler/internal/processor"
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
		runLimiter: make(chan struct{}, ConcurrentRunsWorkers),
	}
}

func (app *CrawlerApp) StartApp() error {
	err := app.pageRepo.EnsureConnectivity()
	if err != nil {
		app.logger.Errorf("Error ensuring connectivity: %v", err)
		return err
	}

	tChan := app.processorQueue.GetTasksChan()
	app.crawler.StartCrawler(tChan, ConcurrentTasksWorkers)
}
