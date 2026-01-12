package processor

import "errors"

var (
	ErrNoTasks          = errors.New("no tasks found")
	ErrNoRuns           = errors.New("no runs found")
	ErrRunLimitExceeded = errors.New("run limit exceeded")
)
