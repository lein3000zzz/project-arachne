package processor

import (
	"errors"
	"sync"
)

// TODO RUN LOGIC IMPLEMENTATION. THE RUNPROCESSOR HAS TASKPROCESSOR AND EACH TASK IS REFERENCING ITS RUN

var (
	ErrNoTasks = errors.New("no tasks found")
	ErrNoRuns  = errors.New("no runs found")
)

type Run struct {
	ID string

	StartURL string

	MaxDepth int
	MaxLinks int

	CurrentLinks int
	*sync.RWMutex
}

type Task struct {
	URL string

	CurrentDepth int
	Run          *Run
}

type Processor interface {
	StartRun(run *Run)
	SendTask(task *Task)
	GetTask() *Task
	StartTaskProducer()
	StartTaskConsumer()
}
