package indexing

import (
	"context"
	"errors"
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"
	"web-crawler/internal/documents"

	"go.uber.org/zap"
)

// After a shutdown deadline cancels in-flight work, how long to wait for workers
// to notice before giving up on them.
const abortGrace = 2 * time.Second

// AsyncSink moves indexing off the crawl workers. Embedding runs at a few chunks
// per second on CPU; done inline it stalls crawling, and a stalled consumer makes
// the Kafka queue drop tasks.
//
// Each URL always lands on the same shard, so two versions of a page are indexed
// in the order they arrived. A document dropped here was never hashed, so the
// next crawl of the page indexes it again.
// Progress hears about each document of a run as it enters the queue and as it
// leaves it, so a run can report how much of it is searchable. A document
// abandoned at shutdown leaves without a word: it is indexed on its next crawl.
type Progress interface {
	Queued(ctx context.Context, runID string)
	Done(ctx context.Context, runID string, failed bool)
	Rejected(ctx context.Context, runID string)
}

type AsyncSink struct {
	logger         *zap.SugaredLogger
	inner          documents.Sink
	progress       Progress
	shards         []chan *documents.Document
	enqueueTimeout time.Duration

	done          chan struct{}
	closeOnce     sync.Once
	processCtx    context.Context
	cancelProcess context.CancelFunc
	workers       sync.WaitGroup

	dropped   atomic.Int64
	failed    atomic.Int64
	abandoned atomic.Int64
}

// NewAsyncSink takes a nil progress when nobody needs to know.
func NewAsyncSink(logger *zap.SugaredLogger, inner documents.Sink, progress Progress, workers, queueSize int, enqueueTimeout time.Duration) *AsyncSink {
	workers = max(workers, 1)
	perShard := max((queueSize+workers-1)/workers, 1)

	processCtx, cancel := context.WithCancel(context.Background())

	s := &AsyncSink{
		logger:         logger,
		inner:          inner,
		progress:       progress,
		shards:         make([]chan *documents.Document, workers),
		enqueueTimeout: enqueueTimeout,
		done:           make(chan struct{}),
		processCtx:     processCtx,
		cancelProcess:  cancel,
	}

	for i := range s.shards {
		s.shards[i] = make(chan *documents.Document, perShard)
		s.workers.Add(1)

		go s.work(s.shards[i])
	}

	return s
}

func (s *AsyncSink) Submit(ctx context.Context, doc *documents.Document) error {
	if doc == nil {
		return ErrNilDocument
	}

	select {
	case <-s.done:
		return ErrSinkClosed
	default:
	}

	timer := time.NewTimer(s.enqueueTimeout)
	defer timer.Stop()

	select {
	case s.shards[shardOf(doc.URL, len(s.shards))] <- doc:
		if s.counted(doc) {
			s.progress.Queued(ctx, doc.RunID)
		}

		return nil
	case <-s.done:
		return ErrSinkClosed
	case <-timer.C:
		s.dropped.Add(1)
		return ErrQueueFull
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *AsyncSink) work(shard chan *documents.Document) {
	defer s.workers.Done()

	for {
		select {
		case doc := <-shard:
			s.process(doc)
		case <-s.done:
			for {
				select {
				case doc := <-shard:
					s.process(doc)
				default:
					return
				}
			}
		}
	}
}

func (s *AsyncSink) process(doc *documents.Document) {
	err := s.inner.Submit(s.processCtx, doc)

	switch {
	case err == nil:
	case errors.Is(err, ErrRejected):
		if s.counted(doc) {
			s.progress.Rejected(context.WithoutCancel(s.processCtx), doc.RunID)
		}

		return
	case s.processCtx.Err() != nil:
		s.abandoned.Add(1)
		return
	default:
		s.failed.Add(1)
		s.logger.Warnw("Indexing failed, page will be retried on its next crawl", "url", doc.URL, "err", err)
	}

	if s.counted(doc) {
		s.progress.Done(context.WithoutCancel(s.processCtx), doc.RunID, err != nil)
	}
}

// counted leaves out removals - a gone page, a redirect's old name - which make
// nothing searchable.
func (s *AsyncSink) counted(doc *documents.Document) bool {
	return s.progress != nil && doc.RunID != "" && doc.Content != ""
}

func (s *AsyncSink) Shutdown(ctx context.Context) error {
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
		// Running out of time is expected when the queue is long: whatever was not
		// indexed was never hashed, so its next crawl indexes it. Only workers that
		// ignore cancellation are an error.
		s.cancelProcess()

		select {
		case <-finished:
		case <-time.After(abortGrace):
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
		"dropped", s.dropped.Load(),
		"failed", s.failed.Load(),
		"abandoned", s.abandoned.Load(),
	)

	return errors.Join(drainErr, s.inner.Shutdown(closeCtx))
}

func shardOf(url string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(url))

	return int(h.Sum32() % uint32(n))
}
