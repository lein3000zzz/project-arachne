package webcrawler

import (
	"web-crawler/internal/domain/config"
	"web-crawler/internal/domain/data"
)

type CrawlerConfig struct {
	TaskConsumerChan <-chan *config.Task
	SaverChan        chan<- *data.PageData
	TaskProducerChan chan<- []*config.Task

	WorkersNumber int
}

type Crawler interface {
	StartCrawler(crawlerConfig *CrawlerConfig)
}
