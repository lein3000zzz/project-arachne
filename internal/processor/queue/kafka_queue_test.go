package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type flushRecorder struct {
	mu      sync.Mutex
	batches [][][]byte
}

func (f *flushRecorder) flush(items [][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()

	copied := make([][]byte, len(items))
	copy(copied, items)
	f.batches = append(f.batches, copied)
}

func (f *flushRecorder) all() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([][]byte, 0)
	for _, b := range f.batches {
		out = append(out, b...)
	}

	return out
}

func startProducer(t *testing.T, batchSize int, interval time.Duration) (chan []byte, chan struct{}, *flushRecorder, chan struct{}) {
	t.Helper()

	in := make(chan []byte, 100)
	stop := make(chan struct{})
	done := make(chan struct{})
	rec := &flushRecorder{}

	go func() {
		runProducer(stop, in, batchSize, interval, func() {}, rec.flush)
		close(done)
	}()

	return in, stop, rec, done
}

func TestProducerFlushesFullBatches(t *testing.T) {
	in, stop, rec, done := startProducer(t, 3, time.Hour)

	for i := range 7 {
		in <- []byte(fmt.Sprint(i))
	}

	deadline := time.Now().Add(time.Second)
	for len(rec.all()) < 6 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	rec.mu.Lock()
	if len(rec.batches) != 2 || len(rec.batches[0]) != 3 {
		t.Errorf("want two full batches of 3 before any tick, got %v", rec.batches)
	}
	rec.mu.Unlock()

	close(stop)
	<-done
}

func TestProducerFlushesOnTick(t *testing.T) {
	in, stop, rec, done := startProducer(t, 100, 10*time.Millisecond)

	in <- []byte("lonely")

	deadline := time.Now().Add(time.Second)
	for len(rec.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if got := rec.all(); len(got) != 1 || string(got[0]) != "lonely" {
		t.Errorf("a partial batch must go out on the tick, got %q", got)
	}

	close(stop)
	<-done
}

func TestProducerFlushesBufferedItemsOnStopInsteadOfDroppingThem(t *testing.T) {
	in, stop, rec, done := startProducer(t, 1000, time.Hour)

	for i := range 25 {
		in <- []byte(fmt.Sprint(i))
	}

	close(stop)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("producer did not return after stop")
	}

	got := rec.all()
	if len(got) != 25 {
		t.Fatalf("every queued item must be flushed at shutdown, got %d of 25", len(got))
	}

	for i, item := range got {
		if len(item) == 0 {
			t.Fatalf("item %d is empty: the producer is reading a closed channel", i)
		}
	}
}

func TestProducerStopWithNothingQueuedSendsNothing(t *testing.T) {
	_, stop, rec, done := startProducer(t, 10, time.Hour)

	close(stop)
	<-done

	if n := len(rec.all()); n != 0 {
		t.Errorf("stopping an idle producer must not produce anything, got %d records", n)
	}
}

func newTestQueue(buffer int) *KafkaQueue {
	stop, cancel := context.WithCancel(context.Background())

	return &KafkaQueue{
		producerChan: make(chan []byte, buffer),
		consumerChan: make(chan Message, buffer),
		cfg:          &KafkaConfig{ChannelBuffer: buffer},
		stop:         stop,
		cancelStop:   cancel,
	}
}

func TestProduceAfterSealIsRejected(t *testing.T) {
	q := newTestQueue(4)
	q.seal()

	if err := q.Produce(context.Background(), []byte("late")); !errors.Is(err, ErrQueueClosed) {
		t.Errorf("err = %v, want ErrQueueClosed", err)
	}

	if len(q.producerChan) != 0 {
		t.Error("a sealed queue must not accept items")
	}
}

func TestBlockedProduceIsReleasedByStop(t *testing.T) {
	q := newTestQueue(1)
	if err := q.Produce(context.Background(), []byte("fills the buffer")); err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		result <- q.Produce(context.Background(), []byte("blocks"))
	}()

	time.Sleep(20 * time.Millisecond)
	q.cancelStop()

	select {
	case err := <-result:
		if !errors.Is(err, ErrQueueClosed) {
			t.Errorf("err = %v, want ErrQueueClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a Produce blocked on a full buffer must return once the queue stops, or sealing deadlocks")
	}

	sealed := make(chan struct{})
	go func() {
		q.seal()
		close(sealed)
	}()

	select {
	case <-sealed:
	case <-time.After(time.Second):
		t.Fatal("seal blocked: a Produce is still holding the lock")
	}
}

func TestProduceHonoursItsContext(t *testing.T) {
	q := newTestQueue(1)
	_ = q.Produce(context.Background(), []byte("fills the buffer"))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if err := q.Produce(ctx, []byte("x")); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}
