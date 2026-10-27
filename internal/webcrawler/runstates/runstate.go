package runstates

import (
	"context"
	"time"
)

type Admission int

const (
	Admitted Admission = iota
	// Duplicate: this task was already admitted for the run, by this node or another.
	Duplicate
	LimitReached
	// RunInactive: the run holds no live slot - it finished, or its lease expired.
	RunInactive
)

type TaskState struct {
	RunActive bool
	Done      bool
}

// RunStateManager coordinates runs across nodes. Every method is a single atomic
// Redis operation, and every task-level method is idempotent, because Kafka
// delivers a task at least once.
type RunStateManager interface {
	// AcquireRunSlot is idempotent per run. It first frees slots whose lease has
	// expired and reports those runs as reaped.
	AcquireRunSlot(ctx context.Context, runID string, maxConcurrent int, lease time.Duration) (acquired bool, reaped []string, err error)
	ReleaseRunSlot(ctx context.Context, runID string) error
	AdmitTask(ctx context.Context, runID, taskID string, maxLinks int) (Admission, error)
	// UnadmitTask undoes an admission whose task never reached the queue.
	UnadmitTask(ctx context.Context, runID, taskID string) error
	// TaskState also renews the run's lease: a task starting is proof of progress.
	TaskState(ctx context.Context, runID, taskID string, lease time.Duration) (TaskState, error)
	// CompleteTask counts a task as finished once. counted is false for a task that
	// was already completed or never admitted; left is meaningful only when counted.
	CompleteTask(ctx context.Context, runID, taskID string, lease time.Duration) (left int64, counted bool, err error)
	CleanupRun(ctx context.Context, runID string) error
	Stop(ctx context.Context) error
}
