package tasksprocessor

// TODO

type Task struct {
	ID string

	URL          string
	CurrentDepth int
	MaxDepth     int
}

type TaskProcessor interface {
	AddTask()
	GetTask()
}
