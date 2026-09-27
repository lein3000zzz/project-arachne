package processor

import (
	"context"
	"errors"
	"testing"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/processor/queue"
	"web-crawler/internal/webcrawler/runstates"

	"go.uber.org/zap"
)

type fakeQueue struct {
	queue.Queue
	produced [][]byte
	err      error
}

func (f *fakeQueue) Produce(_ context.Context, payload []byte) error {
	if f.err != nil {
		return f.err
	}

	f.produced = append(f.produced, payload)

	return nil
}

type fakeRunState struct {
	runstates.RunStateManager
	links       int64
	incremented int
	decremented int
}

func (f *fakeRunState) GetCurrentLinks(context.Context, string) (int64, error) { return f.links, nil }

func (f *fakeRunState) IncrementActiveAndCurrentLinks(context.Context, string) error {
	f.incremented++
	return nil
}

func (f *fakeRunState) DecrementActiveTasks(context.Context, string) (int64, error) {
	f.decremented++
	return 0, nil
}

func task(depth int) *config.Task {
	return &config.Task{
		URL:          "https://example.com/",
		CurrentDepth: depth,
		Run:          &config.Run{ID: "run", MaxDepth: 3, MaxLinks: 10},
	}
}

func TestSendTaskCountsAndProduces(t *testing.T) {
	tasks, state := &fakeQueue{}, &fakeRunState{}
	p := NewTaskProcessorKafka(zap.NewNop().Sugar(), tasks, &fakeQueue{}, state)

	if err := p.SendTask(task(0)); err != nil {
		t.Fatalf("SendTask: %v", err)
	}

	if len(tasks.produced) != 1 || state.incremented != 1 || state.decremented != 0 {
		t.Errorf("produced=%d incremented=%d decremented=%d, want 1/1/0",
			len(tasks.produced), state.incremented, state.decremented)
	}
}

func TestSendTaskReleasesTheCountWhenTheQueueRefuses(t *testing.T) {
	tasks, state := &fakeQueue{err: queue.ErrQueueClosed}, &fakeRunState{}
	p := NewTaskProcessorKafka(zap.NewNop().Sugar(), tasks, &fakeQueue{}, state)

	err := p.SendTask(task(0))
	if !errors.Is(err, queue.ErrQueueClosed) {
		t.Fatalf("err = %v, want ErrQueueClosed", err)
	}

	if state.incremented != 1 || state.decremented != 1 {
		t.Errorf("incremented=%d decremented=%d: a task that never ran must not stay counted",
			state.incremented, state.decremented)
	}
}

func TestSendTaskOverTheLimitIsNeverCounted(t *testing.T) {
	cases := map[string]struct {
		task  *config.Task
		links int64
	}{
		"depth": {task(3), 0},
		"links": {task(0), 10},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tasks, state := &fakeQueue{}, &fakeRunState{links: tc.links}
			p := NewTaskProcessorKafka(zap.NewNop().Sugar(), tasks, &fakeQueue{}, state)

			if err := p.SendTask(tc.task); !errors.Is(err, ErrRunLimitExceeded) {
				t.Fatalf("err = %v, want ErrRunLimitExceeded", err)
			}

			if state.incremented != 0 || state.decremented != 0 || len(tasks.produced) != 0 {
				t.Errorf("a rejected task must touch nothing: incremented=%d decremented=%d produced=%d",
					state.incremented, state.decremented, len(tasks.produced))
			}
		})
	}
}
