package tasksprocessor

import (
	"errors"
	"time"
)

// TODO

var (
	ErrNoTasks = errors.New("no tasks found")
)

const (
	singleRequestTimeout = 5 * time.Second
	queueTimeout         = 1 * time.Minute
	tickerTimeout        = 1 * time.Second
)

type Task struct {
	ID string

	URL          string
	CurrentDepth int
	MaxDepth     int
}

type TaskProcessor interface {
	SendTask(task *Task)
	GetTask() *Task
	StartTaskProducer()
	StartTaskConsumer()
}
