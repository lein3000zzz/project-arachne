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

// RunRecorder keeps the record of runs that progress reports are read from. It is
// told only: a failure to record never affects the crawl.
type RunRecorder interface {
	Finished(ctx context.Context, run *config.Run, note string) error
}
