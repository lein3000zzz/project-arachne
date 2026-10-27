package webcrawler

import (
	"context"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/domain/data"
)

type TaskSender interface {
	SendTask(task *config.Task) error
}

type CrawlerConfig struct {
	TaskConsumerChan <-chan *config.Task
	SaverChan        chan<- *data.PageData
	TaskSender       TaskSender

	WorkersNumber int
}

type Crawler interface {
	StartCrawler(cfg *CrawlerConfig)
	Shutdown(ctx context.Context) error
}
