package tasksprocessor

import "errors"

// TODO

var (
	ErrNoTasks = errors.New("no tasks found")
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
