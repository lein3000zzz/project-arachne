package processor

import (
	"context"
	"web-crawler/internal/domain/config"
)

type Processor interface {
	GetRun() (*config.Run, error)
	QueueRun(run *config.Run)
	SendTask(task *config.Task) error
	StartTaskConsumer()
	GetTasksChan() <-chan *config.Task
	StartRunConsumer()
	StopProcessor(ctx context.Context) error
	GetRunsChan() <-chan *config.Run
}
