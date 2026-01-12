package config

import (
	"sync"
	"web-crawler/internal/utils"
)

// TODO юзер-специфик конфиги и прочие чудеса

type Run struct {
	ID string

	UseCacheFlag bool
	MaxDepth     int
	MaxLinks     int
	ExtraFlags   *ExtraTaskFlags

	StartURL string

	CurrentLinks int
	ActiveTasks  int

	sync.Once

	sync.RWMutex
}

func NewRun(URL string, maxDepth, maxLinks int, flags *ExtraTaskFlags) *Run {
	id, _ := utils.GenerateID()

	return &Run{
		ID:           id,
		UseCacheFlag: true,
		MaxDepth:     maxDepth,
		MaxLinks:     maxLinks,
		ExtraFlags:   flags,
		StartURL:     URL,
		CurrentLinks: 0,
		ActiveTasks:  0,
	}
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

func (run *Run) IncrementActiveAndCurrentWithMutex() {
	run.Lock()
	defer run.Unlock()

	run.ActiveTasks++
	run.CurrentLinks++
}
