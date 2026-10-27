package webcrawler

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
)

func shutdownRepo() (*CrawlerRepo, *recordingSink) {
	sink := &recordingSink{}

	repo := (&CrawlerRepo{
		logger:      zap.NewNop().Sugar(),
		networker:   nopNetworker{},
		extraWorker: nopExtra{},
		cachePages:  nopCache{},
		cacheRobots: nopCache{},
		documents:   sink,
		cfg:         &CrawlerConfig{},
	}).withRobots()

	return repo, sink
}

func TestShutdownStopsTheSinkOnlyAfterWorkersExit(t *testing.T) {
	repo, sink := shutdownRepo()
	repo.workers.Add(1)

	done := make(chan error, 1)
	go func() { done <- repo.Shutdown(context.Background()) }()

	time.Sleep(30 * time.Millisecond)
	if sink.shutdownCount() != 0 {
		t.Fatal("the document sink was shut down while a worker could still submit to it")
	}

	repo.workers.Done()

	if err := <-done; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if sink.shutdownCount() != 1 {
		t.Error("the document sink must be shut down once the workers are gone")
	}
}

func TestShutdownReportsWorkersStillRunningAtTheDeadline(t *testing.T) {
	repo, sink := shutdownRepo()
	repo.workers.Add(1)
	defer repo.workers.Done()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	if err := repo.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the deadline reported", err)
	}

	if sink.shutdownCount() != 1 {
		t.Error("dependencies must still be shut down after the deadline")
	}
}
