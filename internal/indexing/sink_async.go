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
type AsyncSink struct {
	logger         *zap.SugaredLogger
	inner          documents.Sink
	shards         []chan *documents.Document
	enqueueTimeout time.Duration

	done          chan struct{}
	closeOnce     sync.Once
	processCtx    context.Context
	cancelProcess context.CancelFunc
	workers       sync.WaitGroup

	dropped atomic.Int64
	failed  atomic.Int64
}

func NewAsyncSink(logger *zap.SugaredLogger, inner documents.Sink, workers, queueSize int, enqueueTimeout time.Duration) *AsyncSink {
	workers = max(workers, 1)
	perShard := max((queueSize+workers-1)/workers, 1)

	processCtx, cancel := context.WithCancel(context.Background())

	s := &AsyncSink{
		logger:         logger,
		inner:          inner,
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
	if err := s.inner.Submit(s.processCtx, doc); err != nil {
		s.failed.Add(1)
		s.logger.Warnw("Indexing failed, page will be retried on its next crawl", "url", doc.URL, "err", err)
	}
}

func (s *AsyncSink) Shutdown(ctx context.Context) error {
	s.closeOnce.Do(func() { close(s.done) })

	finished := make(chan struct{})
	go func() {
		s.workers.Wait()
		close(finished)
	}()

	var drainErr error

	select {
	case <-finished:
	case <-ctx.Done():
		s.cancelProcess()

		select {
		case <-finished:
		case <-time.After(abortGrace):
			drainErr = errors.New("indexing workers did not stop after cancellation")
		}

		drainErr = errors.Join(drainErr, ctx.Err())
	}

	s.cancelProcess()

	s.logger.Infow("Indexing queue stopped", "dropped", s.dropped.Load(), "failed", s.failed.Load())

	return errors.Join(drainErr, s.inner.Shutdown(ctx))
}

func shardOf(url string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(url))

	return int(h.Sum32() % uint32(n))
}
