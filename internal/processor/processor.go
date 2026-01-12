package processor

import (
	"web-crawler/internal/domain/config"
)

type Processor interface {
	GetRun() (*config.Run, error)
	QueueRun(run *config.Run)
	SendTask(task *config.Task) error
	StartTaskConsumer()
	GetTasksChan() <-chan *config.Task
	StartRunConsumer()
	GetRunsChan() <-chan *config.Run
}
