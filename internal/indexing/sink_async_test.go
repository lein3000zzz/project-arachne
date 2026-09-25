package indexing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
	"web-crawler/internal/documents"

	"go.uber.org/zap"
)

type fakeInner struct {
	mu        sync.Mutex
	docs      []*documents.Document
	block     chan struct{}
	failFirst bool
	calls     int
	sawCancel bool
	shutdowns int
}

func (f *fakeInner) Submit(ctx context.Context, doc *documents.Document) error {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()

	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			f.mu.Lock()
			f.sawCancel = true
			f.mu.Unlock()

			return ctx.Err()
		}
	}

	if f.failFirst && call == 1 {
		return errors.New("first one fails")
	}

	f.mu.Lock()
	f.docs = append(f.docs, doc)
	f.mu.Unlock()

	return nil
}

func (f *fakeInner) Shutdown(context.Context) error {
	f.mu.Lock()
	f.shutdowns++
	f.mu.Unlock()

	return nil
}

func (f *fakeInner) delivered() []*documents.Document {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]*documents.Document(nil), f.docs...)
}

func newAsync(inner documents.Sink, workers, queue int, timeout time.Duration) *AsyncSink {
	return NewAsyncSink(zap.NewNop().Sugar(), inner, workers, queue, timeout)
}

func TestAsyncSinkKeepsPerURLOrder(t *testing.T) {
	inner := &fakeInner{}
	sink := newAsync(inner, 4, 64, time.Second)

	for i := range 20 {
		for _, url := range []string{"https://a.example/", "https://b.example/"} {
			if err := sink.Submit(context.Background(), &documents.Document{URL: url, Content: fmt.Sprint(i)}); err != nil {
				t.Fatalf("Submit: %v", err)
			}
		}
	}

	if err := sink.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	next := map[string]int{}
	for _, doc := range inner.delivered() {
		if doc.Content != fmt.Sprint(next[doc.URL]) {
			t.Fatalf("%s: got version %s, want %d: versions of one page reordered", doc.URL, doc.Content, next[doc.URL])
		}
		next[doc.URL]++
	}

	if next["https://a.example/"] != 20 || next["https://b.example/"] != 20 {
		t.Errorf("want every document delivered, got %v", next)
	}
}

func TestAsyncSinkReportsFullQueue(t *testing.T) {
	inner := &fakeInner{block: make(chan struct{})}
	sink := newAsync(inner, 1, 1, 20*time.Millisecond)
	ctx := context.Background()

	var err error
	for i := 0; i < 5 && err == nil; i++ {
		err = sink.Submit(ctx, &documents.Document{URL: "https://example.com/"})
	}

	if !errors.Is(err, ErrQueueFull) {
		t.Errorf("err = %v, want ErrQueueFull once the worker is stuck and the queue is full", err)
	}

	close(inner.block)
	_ = sink.Shutdown(ctx)
}

func TestAsyncSinkDrainsQueueOnShutdown(t *testing.T) {
	inner := &fakeInner{}
	sink := newAsync(inner, 2, 100, time.Second)

	for i := range 30 {
		if err := sink.Submit(context.Background(), &documents.Document{URL: fmt.Sprintf("https://example.com/%d", i)}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}

	if err := sink.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if n := len(inner.delivered()); n != 30 {
		t.Errorf("everything queued before shutdown must be indexed, got %d of 30", n)
	}

	if inner.shutdowns != 1 {
		t.Errorf("inner sink must be shut down once, got %d", inner.shutdowns)
	}
}

func TestAsyncSinkShutdownDeadlineCancelsInFlightWork(t *testing.T) {
	inner := &fakeInner{block: make(chan struct{})}
	sink := newAsync(inner, 1, 4, time.Second)

	if err := sink.Submit(context.Background(), &documents.Document{URL: "https://example.com/"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := sink.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the deadline reported", err)
	}

	if elapsed := time.Since(start); elapsed > abortGrace {
		t.Errorf("shutdown took %s: in-flight work was not cancelled", elapsed)
	}

	inner.mu.Lock()
	defer inner.mu.Unlock()

	if !inner.sawCancel {
		t.Error("the in-flight document must see its context cancelled")
	}
}

func TestAsyncSinkSurvivesFailedDocuments(t *testing.T) {
	inner := &fakeInner{failFirst: true}
	sink := newAsync(inner, 1, 8, time.Second)

	for i := range 3 {
		_ = sink.Submit(context.Background(), &documents.Document{URL: fmt.Sprintf("https://example.com/%d", i)})
	}

	_ = sink.Shutdown(context.Background())

	if n := len(inner.delivered()); n != 2 {
		t.Errorf("a failure must not stop the worker: want 2 delivered, got %d", n)
	}

	if sink.failed.Load() != 1 {
		t.Errorf("failed = %d, want 1", sink.failed.Load())
	}
}

func TestAsyncSinkRejectsAfterShutdown(t *testing.T) {
	sink := newAsync(&fakeInner{}, 1, 1, time.Second)

	_ = sink.Shutdown(context.Background())
	_ = sink.Shutdown(context.Background())

	if err := sink.Submit(context.Background(), &documents.Document{URL: "https://example.com/"}); !errors.Is(err, ErrSinkClosed) {
		t.Errorf("err = %v, want ErrSinkClosed", err)
	}

	if err := sink.Submit(context.Background(), nil); !errors.Is(err, ErrNilDocument) {
		t.Errorf("err = %v, want ErrNilDocument", err)
	}
}
