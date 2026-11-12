package indexing

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	failures  map[string]int
	calls     int
	sawCancel bool
	shutdowns int
}

func (f *fakeInner) Submit(ctx context.Context, doc *documents.Document) error {
	f.mu.Lock()
	f.calls++
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

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failures[doc.URL] != 0 {
		f.failures[doc.URL]--
		return errors.New("the embedder is down")
	}

	if doc.Content == "rejected" {
		return ErrRejected
	}

	d := *doc
	f.docs = append(f.docs, &d)

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

type fakeProgress struct {
	mu                               sync.Mutex
	queued, indexed, fails, rejected map[string]int
}

func newProgress() *fakeProgress {
	return &fakeProgress{queued: map[string]int{}, indexed: map[string]int{}, fails: map[string]int{}, rejected: map[string]int{}}
}

func (f *fakeProgress) Rejected(_ context.Context, runID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected[runID]++
}

func (f *fakeProgress) Queued(_ context.Context, runID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queued[runID]++
}

func (f *fakeProgress) Done(_ context.Context, runID string, failed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if failed {
		f.fails[runID]++
	} else {
		f.indexed[runID]++
	}
}

// memQueue keeps the table's rules in memory: the oldest ready page first,
// none while an older version of its URL waits, held pages left alone until
// their lease lapses.
type memQueue struct {
	mu   sync.Mutex
	next int
	jobs []*memJob
}

type memJob struct {
	Job
	readyAt, heldUntil time.Time
	owner              string
	dead               bool
	why                string
}

func (q *memQueue) Push(_ context.Context, doc *documents.Document) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.next++
	q.jobs = append(q.jobs, &memJob{Job: Job{ID: fmt.Sprint(q.next), Doc: *doc}})

	return nil
}

func (q *memQueue) Waiting(context.Context) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	n := 0

	for _, j := range q.jobs {
		if !j.dead {
			n++
		}
	}

	return n, nil
}

func (q *memQueue) Claim(_ context.Context, owner string, lease time.Duration) (*Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := time.Now()
	blocked := map[string]bool{}

	for _, j := range q.jobs {
		if j.dead {
			continue
		}

		if !blocked[j.Doc.URL] && !now.Before(j.readyAt) && now.After(j.heldUntil) {
			j.owner, j.heldUntil = owner, now.Add(lease)
			job := j.Job

			return &job, nil
		}

		blocked[j.Doc.URL] = true
	}

	return nil, nil
}

func (q *memQueue) find(id string) *memJob {
	for _, j := range q.jobs {
		if j.ID == id {
			return j
		}
	}

	return nil
}

func (q *memQueue) Extend(_ context.Context, id, owner string, lease time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if j := q.find(id); j != nil && j.owner == owner {
		j.heldUntil = time.Now().Add(lease)
	}

	return nil
}

func (q *memQueue) Done(_ context.Context, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.jobs = slices.DeleteFunc(q.jobs, func(j *memJob) bool { return j.ID == id })

	return nil
}

func (q *memQueue) Retry(_ context.Context, id string, after time.Duration, why string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	j := q.find(id)
	j.Attempts++
	j.readyAt, j.heldUntil, j.owner, j.why = time.Now().Add(after), time.Time{}, "", why

	return nil
}

func (q *memQueue) Release(_ context.Context, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	j := q.find(id)
	j.heldUntil, j.owner = time.Time{}, ""

	return nil
}

func (q *memQueue) Bury(_ context.Context, id, why string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	j := q.find(id)
	j.Attempts++
	j.dead, j.why, j.heldUntil, j.owner = true, why, time.Time{}, ""

	return nil
}

func (q *memQueue) Prune(context.Context, time.Duration) (int64, error) { return 0, nil }

func (q *memQueue) snapshot() []memJob {
	q.mu.Lock()
	defer q.mu.Unlock()

	out := make([]memJob, len(q.jobs))
	for i, j := range q.jobs {
		out[i] = *j
	}

	return out
}

func newQueued(inner documents.Sink, queue Queue, progress Progress, workers, capacity int) *QueuedSink {
	return NewQueuedSink(zap.NewNop().Sugar(), inner, queue, progress, QueuedConfig{
		Workers: workers, Capacity: capacity, EnqueueTimeout: time.Second, Owner: "test",
		FirstRetry: time.Millisecond, Poll: 5 * time.Millisecond,
	})
}

// eventually waits for cond, failing the test after a while.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}

	t.Fatalf("timed out waiting: %s", what)
}

func TestVersionsOfOnePageAreIndexedInOrder(t *testing.T) {
	inner, queue := &fakeInner{}, &memQueue{}
	sink := newQueued(inner, queue, nil, 4, 100)

	for i := range 20 {
		for _, url := range []string{"https://a.example/", "https://b.example/"} {
			if err := sink.Submit(context.Background(), &documents.Document{URL: url, Content: fmt.Sprint(i)}); err != nil {
				t.Fatalf("Submit: %v", err)
			}
		}
	}

	eventually(t, "everything indexed", func() bool { return len(inner.delivered()) == 40 })
	_ = sink.Shutdown(context.Background())

	next := map[string]int{}
	for _, doc := range inner.delivered() {
		if doc.Content != fmt.Sprint(next[doc.URL]) {
			t.Fatalf("%s: got version %s, want %d: versions of one page reordered", doc.URL, doc.Content, next[doc.URL])
		}
		next[doc.URL]++
	}
}

func TestAFullQueueMakesTheCrawlWaitThenSaysSo(t *testing.T) {
	inner := &fakeInner{block: make(chan struct{})}
	sink := NewQueuedSink(zap.NewNop().Sugar(), inner, &memQueue{}, nil,
		QueuedConfig{Workers: 1, Capacity: 2, EnqueueTimeout: 30 * time.Millisecond, Owner: "test"})

	var err error
	for i := 0; i < 5 && err == nil; i++ {
		err = sink.Submit(context.Background(), &documents.Document{URL: fmt.Sprintf("https://example.com/%d", i)})
	}

	if !errors.Is(err, ErrQueueFull) {
		t.Errorf("err = %v, want ErrQueueFull once the worker is stuck and the queue is full", err)
	}

	close(inner.block)
	_ = sink.Shutdown(context.Background())
}

// The point of the queue: what waits when the crawler stops is indexed when
// it starts again, the page being indexed included.
func TestPagesWaitingAtAStopAreIndexedOnTheNextStart(t *testing.T) {
	queue := &memQueue{}
	stuck := &fakeInner{block: make(chan struct{})}
	first := newQueued(stuck, queue, nil, 1, 100)

	for i := range 3 {
		if err := first.Submit(context.Background(), &documents.Document{URL: fmt.Sprintf("https://example.com/%d", i), Content: "text"}); err != nil {
			t.Fatal(err)
		}
	}

	eventually(t, "a page taken", func() bool { stuck.mu.Lock(); defer stuck.mu.Unlock(); return stuck.calls == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	if err := first.Shutdown(ctx); err != nil {
		t.Fatalf("a deadline with work in hand is expected, not an error: %v", err)
	}

	if !stuck.sawCancel || stuck.shutdowns != 1 {
		t.Errorf("the page in hand sees its work cancelled (%v), the inner sink is shut down (%d)", stuck.sawCancel, stuck.shutdowns)
	}

	if jobs := queue.snapshot(); len(jobs) != 3 || jobs[0].Attempts != 0 || jobs[0].owner != "" {
		t.Fatalf("queue after the stop: %+v; the page in hand given back untried", jobs)
	}

	inner := &fakeInner{}
	second := newQueued(inner, queue, nil, 2, 100)

	eventually(t, "the waiting pages indexed", func() bool { return len(inner.delivered()) == 3 })
	_ = second.Shutdown(context.Background())

	if len(queue.snapshot()) != 0 {
		t.Errorf("indexed pages leave the queue: %+v", queue.snapshot())
	}
}

// A crawler that died never gave its page back: once the hold lapses another
// takes it.
func TestAPageHeldByADeadCrawlerIsTakenWhenItsHoldLapses(t *testing.T) {
	queue := &memQueue{}
	_ = queue.Push(context.Background(), &documents.Document{URL: "https://example.com/", Content: "text"})

	if job, _ := queue.Claim(context.Background(), "dead crawler", 50*time.Millisecond); job == nil {
		t.Fatal("nothing to claim")
	}

	inner := &fakeInner{}
	sink := newQueued(inner, queue, nil, 1, 100)

	time.Sleep(20 * time.Millisecond)

	if len(inner.delivered()) != 0 {
		t.Fatal("a held page was taken before its hold lapsed")
	}

	eventually(t, "the lapsed page indexed", func() bool { return len(inner.delivered()) == 1 })
	_ = sink.Shutdown(context.Background())
}

func TestAFailingPageIsTriedAgainThenKeptDead(t *testing.T) {
	inner := &fakeInner{failures: map[string]int{"https://example.com/flaky": 2, "https://example.com/broken": 100}}
	queue, progress := &memQueue{}, newProgress()
	sink := newQueued(inner, queue, progress, 2, 100)

	for _, url := range []string{"https://example.com/flaky", "https://example.com/broken", "https://example.com/fine"} {
		_ = sink.Submit(context.Background(), &documents.Document{URL: url, RunID: "r", Content: "text"})
	}

	eventually(t, "the broken page given up on", func() bool {
		jobs := queue.snapshot()
		return len(jobs) == 1 && jobs[0].dead
	})
	_ = sink.Shutdown(context.Background())

	dead := queue.snapshot()[0]
	if dead.Doc.URL != "https://example.com/broken" || dead.Attempts != maxAttempts || dead.why != "the embedder is down" {
		t.Errorf("dead page %+v: kept after %d tries, with why", dead, maxAttempts)
	}

	if len(inner.delivered()) != 2 || progress.queued["r"] != 3 || progress.indexed["r"] != 2 || progress.fails["r"] != 1 {
		t.Errorf("delivered %d, progress queued %v indexed %v failed %v: a retry is not a failure, giving up is",
			len(inner.delivered()), progress.queued, progress.indexed, progress.fails)
	}
}

func TestProgressCountsRunsPagesOnly(t *testing.T) {
	inner, progress := &fakeInner{}, newProgress()
	sink := newQueued(inner, &memQueue{}, progress, 1, 100)

	for _, doc := range []*documents.Document{
		{URL: "https://example.com/1", RunID: "r", Content: "text"},
		{URL: "https://example.com/2", RunID: "r", Content: "rejected"},
		{URL: "https://example.com/startup-removal", Content: "text"},
		{URL: "https://example.com/gone", RunID: "r"},
	} {
		_ = sink.Submit(context.Background(), doc)
	}

	eventually(t, "all done", func() bool { return len(inner.delivered()) == 3 })
	_ = sink.Shutdown(context.Background())

	if progress.queued["r"] != 2 || progress.indexed["r"] != 1 || progress.rejected["r"] != 1 || len(progress.queued) != 1 {
		t.Errorf("queued %v indexed %v rejected %v: documents without a run, and removals, are not counted",
			progress.queued, progress.indexed, progress.rejected)
	}
}

func TestAStoppedSinkTakesNothing(t *testing.T) {
	sink := newQueued(&fakeInner{}, &memQueue{}, nil, 1, 1)

	_ = sink.Shutdown(context.Background())
	_ = sink.Shutdown(context.Background())

	if err := sink.Submit(context.Background(), &documents.Document{URL: "https://example.com/"}); !errors.Is(err, ErrSinkClosed) {
		t.Errorf("err = %v, want ErrSinkClosed", err)
	}

	if err := sink.Submit(context.Background(), nil); !errors.Is(err, ErrNilDocument) {
		t.Errorf("err = %v, want ErrNilDocument", err)
	}
}
