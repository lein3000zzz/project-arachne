package webcrawler

import (
	"context"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/domain/data"
)

type CrawlerConfig struct {
	TaskConsumerChan <-chan *config.Task
	SaverChan        chan<- *data.PageData
	TaskProducerChan chan<- []*config.Task

	CrawlCallbackChan chan<- struct{}

	WorkersNumber int
}

type Crawler interface {
	StartCrawler(cfg *CrawlerConfig)
	Shutdown(ctx context.Context) error
}
