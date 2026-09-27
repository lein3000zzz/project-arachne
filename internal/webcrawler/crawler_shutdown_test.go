package webcrawler

import (
	"context"
	"errors"
	"testing"
	"time"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/networker"
	"web-crawler/internal/networker/sugaredworker"

	"go.uber.org/zap"
)

type nopNetworker struct{}

func (nopNetworker) Stop()                                        {}
func (nopNetworker) Fetch(string) (*networker.FetchResult, error) { return nil, errors.New("unused") }

type nopExtra struct{}

func (nopExtra) RestartBrowserAndLauncher() error { return nil }
func (nopExtra) PerformExtraTask(string, *config.ExtraTaskFlags) *sugaredworker.ExtraTaskRes {
	return nil
}
func (nopExtra) Shutdown(context.Context) error { return nil }

type nopCache struct{}

func (nopCache) Set(string, any, time.Duration) error { return nil }
func (nopCache) Get(string) (string, error)           { return "", errors.New("miss") }
func (nopCache) Stop(context.Context) error           { return nil }

func shutdownRepo() (*CrawlerRepo, chan struct{}, chan []*config.Task) {
	callbacks := make(chan struct{}, 1)
	produced := make(chan []*config.Task, 1)

	repo := &CrawlerRepo{
		logger:      zap.NewNop().Sugar(),
		networker:   nopNetworker{},
		extraWorker: nopExtra{},
		cachePages:  nopCache{},
		cacheRobots: nopCache{},
		documents:   &recordingSink{},
		cfg:         &CrawlerConfig{CrawlCallbackChan: callbacks, TaskProducerChan: produced},
	}

	return repo, callbacks, produced
}

func isClosed[T any](ch chan T) bool {
	select {
	case _, ok := <-ch:
		return !ok
	default:
		return false
	}
}

func TestShutdownLeavesOutputsOpenWhileAWorkerRuns(t *testing.T) {
	repo, callbacks, produced := shutdownRepo()
	repo.workers.Add(1)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	if err := repo.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the deadline reported", err)
	}

	if isClosed(callbacks) || isClosed(produced) {
		t.Error("closing a channel a live worker may still send on would panic that worker")
	}

	repo.workers.Done()
}

func TestShutdownClosesOutputsOnceWorkersExit(t *testing.T) {
	repo, callbacks, produced := shutdownRepo()
	repo.workers.Add(1)

	go func() {
		time.Sleep(20 * time.Millisecond)
		repo.workers.Done()
	}()

	if err := repo.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if !isClosed(callbacks) || !isClosed(produced) {
		t.Error("with every worker gone, the output channels must be closed so their readers exit")
	}
}
