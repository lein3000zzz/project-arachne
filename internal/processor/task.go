package processor

import (
	"errors"
	"sync"
)

// TODO RUN LOGIC IMPLEMENTATION. THE RUNPROCESSOR HAS TASKPROCESSOR AND EACH TASK IS REFERENCING ITS RUN

var (
	ErrNoTasks = errors.New("no tasks found")
)

type Task struct {
	ID string

	URL string

	CurrentDepth int
	MaxDepth     int

	CurrentLinks int
	MaxLinks     int

	*sync.RWMutex
}

type Processor interface {
	SendTask(task *Task)
	GetTask() *Task
	StartTaskProducer()
	StartTaskConsumer()
}
