package processor

import (
	"errors"
	"sync"
)

// TODO RUN LOGIC IMPLEMENTATION. THE RUNPROCESSOR HAS TASKPROCESSOR AND EACH TASK IS REFERENCING ITS RUN

var (
	ErrNoTasks          = errors.New("no tasks found")
	ErrNoRuns           = errors.New("no runs found")
	ErrRunLimitExceeded = errors.New("run limit exceeded")
)

type Run struct {
	ID string

	UseCacheFlag bool
	MaxDepth     int
	MaxLinks     int
	ExtraFlags   *ExtraTaskFlags

	StartURL string

	currentLinks int
	*sync.RWMutex
}

func (run *Run) IncrementLinks() {
	run.Lock()
	defer run.Unlock()

	run.currentLinks++
}

type Task struct {
	URL          string
	CurrentDepth int

	Run *Run
}

type ExtraTaskFlags struct {
	ShouldScreenshot  bool
	ParseRenderedHTML bool
}

type Processor interface {
	GetRun() (*Run, error)
	QueueRun(run *Run)
	SendTask(task *Task) error
	GetTask() (*Task, error)
}
