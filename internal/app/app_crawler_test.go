package app

import (
	"context"
	"sync"
	"testing"
	"time"
	"web-crawler/internal/appconfig"
	"web-crawler/internal/pages"
	"web-crawler/internal/processor"
	"web-crawler/internal/webcrawler"
	"web-crawler/internal/webcrawler/runstates"

	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/zap"
)

type shutdownLog struct {
	mu     sync.Mutex
	events []string
}

func (l *shutdownLog) record(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.events = append(l.events, event)
}

func (l *shutdownLog) index(event string) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	for i, e := range l.events {
		if e == event {
			return i
		}
	}

	return -1
}

// Each stage records when it starts and when it ends, and is slow enough that a
// concurrent StopApp would interleave them.
func slowStage(log *shutdownLog, name string) func(context.Context) error {
	return func(context.Context) error {
		log.record(name + " start")
		time.Sleep(20 * time.Millisecond)
		log.record(name + " end")

		return nil
	}
}

type fakeProcessor struct {
	processor.Processor
	stop func(context.Context) error
}

func (f *fakeProcessor) StopProcessor(ctx context.Context) error { return f.stop(ctx) }

type fakeCrawler struct {
	webcrawler.Crawler
	stop func(context.Context) error
}

func (f *fakeCrawler) Shutdown(ctx context.Context) error { return f.stop(ctx) }

type fakePages struct {
	pages.PageRepo
	stop func(context.Context) error
}

func (f *fakePages) Shutdown(ctx context.Context) error { return f.stop(ctx) }

type fakeRunState struct {
	runstates.RunStateManager
	stop func(context.Context) error
}

func (f *fakeRunState) Stop(ctx context.Context) error { return f.stop(ctx) }

func TestStopAppShutsDownInStages(t *testing.T) {
	log := &shutdownLog{}

	app := NewCrawlerApp(
		zap.NewNop().Sugar(),
		&fakeCrawler{stop: slowStage(log, "crawler")},
		&fakePages{stop: slowStage(log, "pages")},
		&fakeProcessor{stop: slowStage(log, "queues")},
		&fakeRunState{stop: slowStage(log, "runstate")},
		tracesdk.NewTracerProvider(),
		appconfig.Default(),
	)

	if err := app.StopApp(context.Background()); err != nil {
		t.Fatalf("StopApp: %v", err)
	}

	for _, order := range [][2]string{
		{"queues end", "crawler start"},
		{"crawler end", "pages start"},
		{"crawler end", "runstate start"},
	} {
		before, after := log.index(order[0]), log.index(order[1])
		if before < 0 || after < 0 || before > after {
			t.Errorf("%q must happen before %q; got %v", order[0], order[1], log.events)
		}
	}
}
