package indexing

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"web-crawler/internal/documents"

	"go.uber.org/zap"
)

// After a shutdown deadline cancels in-flight work, how long to wait for workers
// to notice before giving up on them.
const abortGrace = 2 * time.Second

const (
	// A worker renews its hold on a page at a third of this; a page whose
	// crawler died is taken again once it lapses.
	lease = 2 * time.Minute
	// How often idle workers look for work another process queued.
	idlePoll = 2 * time.Second
	// How often a submit waiting for room looks again.
	fullPoll = 500 * time.Millisecond
	// A page fails this many times, waiting longer each time - the embedder
	// or the reviewer's model down for a while - before it is given up on.
	maxAttempts = 6
	firstRetry  = 30 * time.Second
	lastRetry   = 30 * time.Minute
	// Dead pages are kept this long for a person to look at.
	deadKept = 14 * 24 * time.Hour
	// Queue writes after the work is done get their own time: the work's
	// context may be cancelled by then.
	queueWriteTimeout = 10 * time.Second
)

// Progress hears about each document of a run as it enters the queue and as it
// leaves it, so a run can report how much of it is searchable.
type Progress interface {
	Queued(ctx context.Context, runID string)
	Done(ctx context.Context, runID string, failed bool)
	Rejected(ctx context.Context, runID string)
}

type QueuedConfig struct {
	Workers int
	// Pages waiting before a submit waits for room.
	Capacity       int
	EnqueueTimeout time.Duration
	// Names this process's hold on the pages it indexes.
	Owner string
	// Zero for the defaults: 30 s before a failed page is tried again,
	// doubling up to 30 min, and a look for work every 2 s when idle.
	FirstRetry time.Duration
	Poll       time.Duration
}

// QueuedSink moves indexing off the crawl workers through a queue that
// outlives the process. Embedding runs at a few chunks per second on CPU;
// done inline it stalls crawling, and what waits in memory is lost when the
// crawler dies. Pages wait in the queue until they are indexed or turned down;
// one that keeps failing is kept, dead, with its error.
type QueuedSink struct {
	logger   *zap.SugaredLogger
	inner    documents.Sink
	queue    Queue
	progress Progress
	cfg      QueuedConfig

	wake          chan struct{}
	done          chan struct{}
	closeOnce     sync.Once
	processCtx    context.Context
	cancelProcess context.CancelFunc
	workers       sync.WaitGroup

	indexed, rejected, retried, buried, released atomic.Int64
}

// NewQueuedSink takes a nil progress when nobody needs to know.
func NewQueuedSink(logger *zap.SugaredLogger, inner documents.Sink, queue Queue, progress Progress, cfg QueuedConfig) *QueuedSink {
	cfg.Workers = max(cfg.Workers, 1)
	cfg.Capacity = max(cfg.Capacity, 1)
	cfg.FirstRetry = cmp.Or(cfg.FirstRetry, firstRetry)
	cfg.Poll = cmp.Or(cfg.Poll, idlePoll)

	processCtx, cancel := context.WithCancel(context.Background())

	s := &QueuedSink{
		logger:        logger,
		inner:         inner,
		queue:         queue,
		progress:      progress,
		cfg:           cfg,
		wake:          make(chan struct{}, cfg.Workers),
		done:          make(chan struct{}),
		processCtx:    processCtx,
		cancelProcess: cancel,
	}

	if n, err := queue.Prune(processCtx, deadKept); err != nil {
		logger.Warnw("Failed to prune dead pages from the indexing queue", "error", err)
	} else if n > 0 {
		logger.Infow("Pruned dead pages from the indexing queue", "pages", n)
	}

	if waiting, err := queue.Waiting(processCtx); err == nil && waiting > 0 {
		logger.Infow("Indexing pages queued before the last stop", "pages", waiting)
	}

	for range cfg.Workers {
		s.workers.Add(1)

		go s.work()
	}

	return s
}

func (s *QueuedSink) Submit(ctx context.Context, doc *documents.Document) error {
	if doc == nil {
		return ErrNilDocument
	}

	if s.closed() {
		return ErrSinkClosed
	}

	deadline := time.Now().Add(s.cfg.EnqueueTimeout)

	for {
		waiting, err := s.queue.Waiting(ctx)
		if err != nil {
			return fmt.Errorf("counting queued pages: %w", err)
		}

		if waiting < s.cfg.Capacity {
			break
		}

		if !time.Now().Before(deadline) {
			return ErrQueueFull
		}

		select {
		case <-time.After(fullPoll):
		case <-s.done:
			return ErrSinkClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if err := s.queue.Push(ctx, doc); err != nil {
		return fmt.Errorf("queueing %s: %w", doc.URL, err)
	}

	if s.counted(doc) {
		s.progress.Queued(ctx, doc.RunID)
	}

	select {
	case s.wake <- struct{}{}:
	default:
	}

	return nil
}

func (s *QueuedSink) closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *QueuedSink) work() {
	defer s.workers.Done()

	for !s.closed() {
		job, err := s.queue.Claim(s.processCtx, s.cfg.Owner, lease)

		switch {
		case err != nil && s.processCtx.Err() != nil:
			return
		case err != nil:
			s.logger.Warnw("Failed to take a page from the indexing queue", "error", err)
		case job != nil:
			s.process(job)
			continue
		}

		select {
		case <-s.wake:
		case <-time.After(s.cfg.Poll):
		case <-s.done:
			return
		}
	}
}

func (s *QueuedSink) process(job *Job) {
	ctx, stop := context.WithCancel(s.processCtx)
	held := make(chan struct{})

	go func() {
		defer close(held)
		s.hold(ctx, job.ID)
	}()

	err := s.inner.Submit(ctx, &job.Doc)

	stop()
	<-held

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(s.processCtx), queueWriteTimeout)
	defer cancel()

	doc := &job.Doc

	var queueErr error

	switch {
	case err == nil:
		queueErr = s.queue.Done(writeCtx, job.ID)
		s.indexed.Add(1)

		if s.counted(doc) {
			s.progress.Done(writeCtx, doc.RunID, false)
		}
	case errors.Is(err, ErrRejected):
		queueErr = s.queue.Done(writeCtx, job.ID)
		s.rejected.Add(1)

		if s.counted(doc) {
			s.progress.Rejected(writeCtx, doc.RunID)
		}
	case s.processCtx.Err() != nil:
		// Stopping: the next start indexes it.
		queueErr = s.queue.Release(writeCtx, job.ID)
		s.released.Add(1)
	case job.Attempts+1 >= maxAttempts:
		queueErr = s.queue.Bury(writeCtx, job.ID, err.Error())
		s.buried.Add(1)
		s.logger.Warnw("Indexing failed every try, page kept as dead", "url", doc.URL, "attempts", job.Attempts+1, "error", err)

		if s.counted(doc) {
			s.progress.Done(writeCtx, doc.RunID, true)
		}
	default:
		after := s.retryAfter(job.Attempts)
		queueErr = s.queue.Retry(writeCtx, job.ID, after, err.Error())
		s.retried.Add(1)
		s.logger.Infow("Indexing failed, page will be tried again", "url", doc.URL, "attempt", job.Attempts+1, "in", after, "error", err)
	}

	// The hold lapses and the page is taken again: indexed twice at worst,
	// which the document hash makes cheap.
	if queueErr != nil {
		s.logger.Warnw("Failed to record a page's indexing in the queue", "url", doc.URL, "error", queueErr)
	}
}

// hold renews the lease on a page until ctx ends.
func (s *QueuedSink) hold(ctx context.Context, id string) {
	ticker := time.NewTicker(lease / 3)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.queue.Extend(ctx, id, s.cfg.Owner, lease); err != nil && ctx.Err() == nil {
				s.logger.Warnw("Failed to renew the hold on a page being indexed", "id", id, "error", err)
			}
		}
	}
}

func (s *QueuedSink) retryAfter(attempts int) time.Duration {
	return min(s.cfg.FirstRetry<<min(attempts, 16), lastRetry)
}

// counted leaves out removals - a gone page, a redirect's old name - which make
// nothing searchable.
func (s *QueuedSink) counted(doc *documents.Document) bool {
	return s.progress != nil && doc.RunID != "" && doc.Content != ""
}

// Shutdown stops taking pages; the one each worker holds is finished, or given
// back when ctx ends first. Pages still waiting stay queued for the next start.
func (s *QueuedSink) Shutdown(ctx context.Context) error {
	s.closeOnce.Do(func() { close(s.done) })

	finished := make(chan struct{})
	go func() {
		s.workers.Wait()
		close(finished)
	}()

	var drainErr error

	closeCtx := ctx

	select {
	case <-finished:
	case <-ctx.Done():
		s.cancelProcess()

		select {
		case <-finished:
		case <-time.After(abortGrace + queueWriteTimeout):
			drainErr = errors.New("indexing workers did not stop after cancellation")
		}

		// The deadline is spent, but closing the store and hash connections still
		// has to happen and takes moments.
		var cancel context.CancelFunc
		closeCtx, cancel = context.WithTimeout(context.Background(), abortGrace)
		defer cancel()
	}

	s.cancelProcess()

	s.logger.Infow("Indexing queue stopped",
		"indexed", s.indexed.Load(),
		"rejected", s.rejected.Load(),
		"retried", s.retried.Load(),
		"dead", s.buried.Load(),
		"givenBack", s.released.Load(),
	)

	return errors.Join(drainErr, s.inner.Shutdown(closeCtx))
}
