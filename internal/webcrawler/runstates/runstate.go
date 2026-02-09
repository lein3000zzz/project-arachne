package runstates

import (
	"context"
	"time"
)

// RunStateManager работает с ранами в распределенной системе
// инкременты и декременты атомарны
type RunStateManager interface {
	IncrementActiveTasks(ctx context.Context, runID string) (int64, error)
	DecrementActiveTasks(ctx context.Context, runID string) (int64, error)
	IncrementCurrentLinks(ctx context.Context, runID string) (int64, error)
	GetCurrentLinks(ctx context.Context, runID string) (int64, error)
	GetActiveTasks(ctx context.Context, runID string) (int64, error)
	IncrementActiveAndCurrentLinks(ctx context.Context, runID string) error
	AcquireRunCompletionLock(ctx context.Context, runID string, ttl time.Duration) (bool, error)
	ReleaseRunCompletionLock(ctx context.Context, runID string) error
	AcquireRunSlot(ctx context.Context, maxConcurrent int) (bool, error)
	ReleaseRunSlot(ctx context.Context) error
	CleanupRun(ctx context.Context, runID string) error
	SetRunTTL(ctx context.Context, runID string, ttl time.Duration) error
	Stop(ctx context.Context) error
}
