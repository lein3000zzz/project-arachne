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

	CurrentLinks int
	ActiveTasks  int
	*sync.RWMutex
}

func (run *Run) IncrementActiveWithMutex() int {
	run.Lock()
	defer run.Unlock()

	run.ActiveTasks++
	return run.ActiveTasks
}

func (run *Run) DecrementActiveWithMutex() int {
	run.Lock()
	defer run.Unlock()

	run.ActiveTasks--
	return run.ActiveTasks
}

func (run *Run) EnsureMutex() {
	if run.RWMutex == nil {
		run.RWMutex = &sync.RWMutex{}
	}
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
