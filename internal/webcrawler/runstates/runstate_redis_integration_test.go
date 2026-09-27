//go:build integration

package runstates

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// DB 15 keeps these tests away from the crawler's run state in DB 2.
const testDB = 15

func newManager(t *testing.T) (*RedisRunStateManager, *redis.Client) {
	t.Helper()

	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	client := redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("REDIS_PASSWORD"), DB: testDB})
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis at %s: %v (set REDIS_PASSWORD)", addr, err)
	}

	client.FlushDB(context.Background())
	t.Cleanup(func() {
		client.FlushDB(context.Background())
		_ = client.Close()
	})

	return NewRedisRunStateManager(client, zap.NewNop().Sugar(), "test-node", time.Hour), client
}

const lease = time.Minute

func TestSlotsAreOwnedAndIdempotent(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()

	if ok, _, _ := m.AcquireRunSlot(ctx, "a", 1, lease); !ok {
		t.Fatal("first run must get the only slot")
	}

	if ok, _, _ := m.AcquireRunSlot(ctx, "b", 1, lease); ok {
		t.Fatal("a second run must not exceed the limit")
	}

	if ok, _, _ := m.AcquireRunSlot(ctx, "a", 1, lease); !ok {
		t.Fatal("a redelivered run must keep its own slot")
	}

	_ = m.ReleaseRunSlot(ctx, "b")
	if ok, _, _ := m.AcquireRunSlot(ctx, "b", 1, lease); ok {
		t.Fatal("releasing a slot the run never held must not free another run's slot")
	}

	_ = m.ReleaseRunSlot(ctx, "a")
	_ = m.ReleaseRunSlot(ctx, "a")
	if ok, _, _ := m.AcquireRunSlot(ctx, "b", 1, lease); !ok {
		t.Fatal("after a's release, b must get the slot")
	}
}

func TestExpiredLeaseIsReaped(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()

	if ok, _, _ := m.AcquireRunSlot(ctx, "stuck", 1, 300*time.Millisecond); !ok {
		t.Fatal("acquire")
	}

	time.Sleep(450 * time.Millisecond)

	ok, reaped, err := m.AcquireRunSlot(ctx, "next", 1, lease)
	if err != nil || !ok {
		t.Fatalf("a run without progress must lose its slot: ok=%v err=%v", ok, err)
	}

	if !slices.Equal(reaped, []string{"stuck"}) {
		t.Errorf("reaped = %v, want [stuck]", reaped)
	}

	if admission, _ := m.AdmitTask(ctx, "stuck", "t", 10); admission != RunInactive {
		t.Errorf("a reaped run must not admit new tasks, got %v", admission)
	}
}

func TestProgressRenewsTheLease(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	short := 400 * time.Millisecond

	_, _, _ = m.AcquireRunSlot(ctx, "busy", 1, short)
	_, _ = m.AdmitTask(ctx, "busy", "t1", 10)
	_, _ = m.AdmitTask(ctx, "busy", "t2", 10)

	time.Sleep(250 * time.Millisecond)
	_, _ = m.TaskState(ctx, "busy", "t1", short)

	time.Sleep(250 * time.Millisecond)
	_, _, _ = m.CompleteTask(ctx, "busy", "t1", short)

	time.Sleep(250 * time.Millisecond)

	if ok, _, _ := m.AcquireRunSlot(ctx, "other", 1, lease); ok {
		t.Error("750ms after acquiring, a run that kept making progress must still hold its 400ms lease")
	}
}

func TestAdmission(t *testing.T) {
	m, client := newManager(t)
	ctx := context.Background()

	if a, _ := m.AdmitTask(ctx, "r", "t1", 10); a != RunInactive {
		t.Fatalf("no slot: got %v, want RunInactive", a)
	}

	_, _, _ = m.AcquireRunSlot(ctx, "r", 1, lease)

	steps := []struct {
		task string
		want Admission
	}{
		{"t1", Admitted},
		{"t1", Duplicate},
		{"t2", Admitted},
		{"t3", LimitReached},
	}

	for _, s := range steps {
		if got, err := m.AdmitTask(ctx, "r", s.task, 2); err != nil || got != s.want {
			t.Fatalf("admit %s: got %v (err %v), want %v", s.task, got, err, s.want)
		}
	}

	if n, _ := client.Get(ctx, activeTasksKeyPrefix+"r").Int(); n != 2 {
		t.Errorf("active = %d, want 2: duplicates and rejections must not be counted", n)
	}

	if ttl := client.TTL(ctx, tasksKeyPrefix+"r").Val(); ttl <= 0 {
		t.Errorf("run state must expire, ttl = %v", ttl)
	}
}

func TestUnadmitMakesTheTaskAdmissibleAgain(t *testing.T) {
	m, client := newManager(t)
	ctx := context.Background()

	_, _, _ = m.AcquireRunSlot(ctx, "r", 1, lease)
	_, _ = m.AdmitTask(ctx, "r", "t1", 1)

	if err := m.UnadmitTask(ctx, "r", "t1"); err != nil {
		t.Fatal(err)
	}

	if n, _ := client.Get(ctx, activeTasksKeyPrefix+"r").Int(); n != 0 {
		t.Errorf("active = %d after unadmit, want 0", n)
	}

	if a, _ := m.AdmitTask(ctx, "r", "t1", 1); a != Admitted {
		t.Errorf("a task that never reached the queue must be admissible again, got %v", a)
	}
}

func TestCompletionCountsOnce(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()

	_, _, _ = m.AcquireRunSlot(ctx, "r", 1, lease)
	_, _ = m.AdmitTask(ctx, "r", "t1", 10)
	_, _ = m.AdmitTask(ctx, "r", "t2", 10)

	if left, counted, _ := m.CompleteTask(ctx, "r", "t1", lease); !counted || left != 1 {
		t.Fatalf("complete t1: left=%d counted=%v", left, counted)
	}

	if _, counted, _ := m.CompleteTask(ctx, "r", "t1", lease); counted {
		t.Fatal("a redelivered task must not be counted twice")
	}

	if state, _ := m.TaskState(ctx, "r", "t1", lease); !state.Done || !state.RunActive {
		t.Errorf("state = %+v, want done and active", state)
	}

	if _, counted, _ := m.CompleteTask(ctx, "r", "never-admitted", lease); counted {
		t.Fatal("a task that was never admitted must not be counted")
	}

	if left, counted, _ := m.CompleteTask(ctx, "r", "t2", lease); !counted || left != 0 {
		t.Fatalf("complete t2: left=%d counted=%v", left, counted)
	}
}

func TestExactlyOneWorkerSeesTheRunFinish(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	const tasks = 50

	_, _, _ = m.AcquireRunSlot(ctx, "r", 1, lease)
	for i := range tasks {
		_, _ = m.AdmitTask(ctx, "r", fmt.Sprint(i), tasks)
	}

	var finishers atomic.Int32
	var wg sync.WaitGroup

	for i := range tasks {
		for range 2 {
			wg.Go(func() {
				if left, counted, err := m.CompleteTask(ctx, "r", fmt.Sprint(i), lease); err == nil && counted && left == 0 {
					finishers.Add(1)
				}
			})
		}
	}

	wg.Wait()

	if n := finishers.Load(); n != 1 {
		t.Errorf("%d completions saw the run reach zero, want exactly 1", n)
	}
}

func TestCleanupRemovesRunState(t *testing.T) {
	m, client := newManager(t)
	ctx := context.Background()

	_, _, _ = m.AcquireRunSlot(ctx, "r", 1, lease)
	_, _ = m.AdmitTask(ctx, "r", "t1", 10)

	if err := m.CleanupRun(ctx, "r"); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{activeTasksKeyPrefix + "r", currentLinksKeyPrefix + "r", tasksKeyPrefix + "r"} {
		if client.Exists(ctx, key).Val() != 0 {
			t.Errorf("%s survived cleanup", key)
		}
	}
}
