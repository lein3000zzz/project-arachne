package processor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/processor/queue"
	"web-crawler/internal/webcrawler/runstates"

	"go.uber.org/zap"
)

type fakeQueue struct {
	queue.Queue
	produced [][]byte
	err      error
	consumer chan queue.Message
}

func (f *fakeQueue) Produce(_ context.Context, payload []byte) error {
	if f.err != nil {
		return f.err
	}

	f.produced = append(f.produced, payload)

	return nil
}

func (f *fakeQueue) GetConsumerChan() <-chan queue.Message { return f.consumer }
func (f *fakeQueue) StartQueueConsumer()                   {}
func (f *fakeQueue) StartQueueProducer()                   {}

type fakeRunState struct {
	runstates.RunStateManager
	admission  runstates.Admission
	admitted   []string
	unadmitted []string
}

func (f *fakeRunState) AdmitTask(_ context.Context, _, taskID string, _ int) (runstates.Admission, error) {
	f.admitted = append(f.admitted, taskID)
	return f.admission, nil
}

func (f *fakeRunState) UnadmitTask(_ context.Context, _, taskID string) error {
	f.unadmitted = append(f.unadmitted, taskID)
	return nil
}

func task(depth int) *config.Task {
	return &config.Task{
		URL:          "https://example.com/",
		CurrentDepth: depth,
		Run:          &config.Run{ID: "run", MaxDepth: 3, MaxLinks: 10},
	}
}

func newProcessor(tasks *fakeQueue, state *fakeRunState) *QueueProcessor {
	return NewTaskProcessorKafka(zap.NewNop().Sugar(), tasks, &fakeQueue{}, state)
}

func TestSendTaskByAdmission(t *testing.T) {
	cases := []struct {
		admission runstates.Admission
		err       error
		produced  int
	}{
		{runstates.Admitted, nil, 1},
		{runstates.Duplicate, nil, 0},
		{runstates.LimitReached, ErrRunLimitExceeded, 0},
		{runstates.RunInactive, ErrRunNotActive, 0},
	}

	for _, tc := range cases {
		tasks, state := &fakeQueue{}, &fakeRunState{admission: tc.admission}

		err := newProcessor(tasks, state).SendTask(task(0))
		if !errors.Is(err, tc.err) {
			t.Errorf("admission %v: err = %v, want %v", tc.admission, err, tc.err)
		}

		if len(tasks.produced) != tc.produced || len(state.unadmitted) != 0 {
			t.Errorf("admission %v: produced %d (want %d), unadmitted %v",
				tc.admission, len(tasks.produced), tc.produced, state.unadmitted)
		}
	}
}

func TestSendTaskCarriesItsID(t *testing.T) {
	tasks, state := &fakeQueue{}, &fakeRunState{}

	if err := newProcessor(tasks, state).SendTask(task(0)); err != nil {
		t.Fatal(err)
	}

	var sent config.Task
	_ = json.Unmarshal(tasks.produced[0], &sent)

	want := config.TaskID("run", "https://example.com/")
	if sent.ID != want || state.admitted[0] != want {
		t.Errorf("sent ID %q, admitted %q, want %q", sent.ID, state.admitted[0], want)
	}
}

func TestSendTaskAtMaxDepthNeverReachesRedis(t *testing.T) {
	tasks, state := &fakeQueue{}, &fakeRunState{}

	if err := newProcessor(tasks, state).SendTask(task(3)); !errors.Is(err, ErrRunLimitExceeded) {
		t.Fatalf("err = %v, want ErrRunLimitExceeded", err)
	}

	if len(state.admitted) != 0 {
		t.Error("the depth limit needs no Redis round trip")
	}
}

func TestSendTaskUndoesAdmissionWhenTheQueueRefuses(t *testing.T) {
	tasks, state := &fakeQueue{err: queue.ErrQueueClosed}, &fakeRunState{}

	if err := newProcessor(tasks, state).SendTask(task(0)); !errors.Is(err, queue.ErrQueueClosed) {
		t.Fatalf("err = %v, want ErrQueueClosed", err)
	}

	if len(state.unadmitted) != 1 || state.unadmitted[0] != state.admitted[0] {
		t.Errorf("admitted %v, unadmitted %v: a task that never reached Kafka must not stay counted",
			state.admitted, state.unadmitted)
	}
}

func TestTaskConsumerAcksMalformedMessagesAndAttachesAcks(t *testing.T) {
	consumer := make(chan queue.Message, 4)
	tasks := &fakeQueue{consumer: consumer}
	p := newProcessor(tasks, &fakeRunState{})

	acked := map[string]int{}
	send := func(name string, payload string) {
		consumer <- queue.NewMessage([]byte(payload), func() { acked[name]++ })
	}

	valid, _ := json.Marshal(task(0))
	send("empty", "")
	send("garbage", "not json")
	send("no run", `{"url":"https://example.com/"}`)
	send("valid", string(valid))
	close(consumer)

	go p.StartTaskConsumer()

	var delivered []*config.Task
	for tk := range p.GetTasksChan() {
		delivered = append(delivered, tk)
	}

	for _, name := range []string{"empty", "garbage", "no run"} {
		if acked[name] != 1 {
			t.Errorf("malformed %q must be acked immediately, acked %d times", name, acked[name])
		}
	}

	if len(delivered) != 1 || acked["valid"] != 0 {
		t.Fatalf("want the valid task delivered and not yet acked; delivered %d, acked %d", len(delivered), acked["valid"])
	}

	delivered[0].Ack()
	if acked["valid"] != 1 {
		t.Error("acking the task must ack its Kafka message")
	}
}

func TestRunConsumerAttachesAcks(t *testing.T) {
	consumer := make(chan queue.Message, 2)
	runs := &fakeQueue{consumer: consumer}
	p := NewTaskProcessorKafka(zap.NewNop().Sugar(), &fakeQueue{}, runs, &fakeRunState{})

	acked := 0
	payload, _ := json.Marshal(&config.Run{ID: "r", StartURL: "https://example.com/"})
	consumer <- queue.NewMessage(payload, func() { acked++ })
	close(consumer)

	go p.StartRunConsumer()

	select {
	case run := <-p.GetRunsChan():
		if acked != 0 {
			t.Error("a run must not be acked before it is handled")
		}

		run.Ack()
		if acked != 1 {
			t.Error("acking the run must ack its Kafka message")
		}
	case <-time.After(time.Second):
		t.Fatal("run not delivered")
	}
}
