package processor

import (
	"errors"
	"web-crawler/internal/domain/config"
)

// TODO RUN LOGIC IMPLEMENTATION. THE RUNPROCESSOR HAS TASKPROCESSOR AND EACH TASK IS REFERENCING ITS RUN

var (
	ErrNoTasks          = errors.New("no tasks found")
	ErrNoRuns           = errors.New("no runs found")
	ErrRunLimitExceeded = errors.New("run limit exceeded")
)

type Processor interface {
	GetRun() (*config.Run, error)
	QueueRun(run *config.Run)
	SendTask(task *config.Task) error
	StartTaskConsumer()
	GetTasksChan() <-chan *config.Task
}
