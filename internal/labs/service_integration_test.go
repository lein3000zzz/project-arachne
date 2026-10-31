//go:build integration

package labs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/domain/data"
	"web-crawler/internal/pages"
	"web-crawler/internal/runs"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.uber.org/zap"
)

type fakeProducer struct {
	runs []config.Run
}

func (p *fakeProducer) ProduceSync(_ context.Context, rs ...*kgo.Record) kgo.ProduceResults {
	results := make(kgo.ProduceResults, len(rs))

	for i, r := range rs {
		var run config.Run
		results[i] = kgo.ProduceResult{Record: r, Err: json.Unmarshal(r.Value, &run)}
		p.runs = append(p.runs, run)
	}

	return results
}

// A lab's source is crawled through the real runs service; the crawler saving
// a page of that run is what puts the page in the lab.
func TestASourcesCrawlFillsTheLab(t *testing.T) {
	store, pool, lein, other := newStore(t)
	ctx := context.Background()

	producer := &fakeProducer{}
	runService := runs.NewService(runs.NewStore(nil, pool, 10*time.Minute), producer,
		runs.Limits{DefaultDepth: 2, MaxDepth: 5, DefaultLinks: 10, MaxLinks: 100})
	service := NewService(store, runService, nil, Limits{MaxSources: 3})

	detail, err := service.Create(ctx, lein, "  Go concurrency ", "goroutines", []SourceRequest{{URL: "go.dev/doc/", Scope: "section", Links: 5}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if detail.Name != "Go concurrency" || len(detail.Sources) != 1 || detail.Sources[0].Run == nil ||
		detail.Sources[0].URL != "https://go.dev/doc/" || detail.Sources[0].MaxDepth != 2 || detail.Sources[0].MaxLinks != 5 {
		t.Fatalf("created: %+v", detail)
	}

	if len(producer.runs) != 1 || producer.runs[0].UseCacheFlag || producer.runs[0].Scope != "https://go.dev/doc/" || !producer.runs[0].ReadDocuments {
		t.Fatalf("produced %+v: want one run, past the page cache, in the section, reading a document at its start", producer.runs)
	}

	run := producer.runs[0]

	var sourceID, submittedBy int64
	if err := pool.QueryRow(ctx, `SELECT source_id, submitted_by FROM runs WHERE id = $1`, run.ID).Scan(&sourceID, &submittedBy); err != nil ||
		sourceID != detail.Sources[0].ID || submittedBy != lein {
		t.Fatalf("run row: source %d, by %d, %v", sourceID, submittedBy, err)
	}

	repo := pages.NewPostgresRepo(zap.NewNop().Sugar(), pool)
	save := func(url, runID string) {
		t.Helper()

		now := time.Now()
		if err := repo.SavePage(&data.PageData{URL: url, Status: 200, Title: "T", LastRunID: runID, FoundAt: now, LastUpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}

	save("https://go.dev/doc/", run.ID)
	save("https://go.dev/doc/effective_go", run.ID)
	save("https://go.dev/blog/", "someone-elses-run")

	urls, err := service.PageURLs(ctx, lein, []int64{detail.ID, detail.ID})
	if err != nil || fmt.Sprint(urls) != "[https://go.dev/doc/ https://go.dev/doc/effective_go]" {
		t.Errorf("lab pages: %v, %v", urls, err)
	}

	refreshed, err := service.Refresh(ctx, lein, detail.ID, detail.Sources[0].ID)
	if err != nil || len(producer.runs) != 2 || refreshed.LastRunID != producer.runs[1].ID || refreshed.Run.SourceID != refreshed.ID {
		t.Errorf("refresh: %+v, %v", refreshed, err)
	}

	if _, err := service.Refresh(ctx, other, detail.ID, detail.Sources[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user refreshing: %v", err)
	}

	if _, err := service.AddSources(ctx, lein, detail.ID, []SourceRequest{{URL: "https://go.dev/doc/"}}); !errors.Is(err, ErrSourceExists) {
		t.Errorf("a source the lab has: %v", err)
	}

	if _, err := service.AddSources(ctx, lein, detail.ID, []SourceRequest{{URL: "a.example"}, {URL: "b.example"}, {URL: "c.example"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("past the source limit: %v", err)
	}

	grown, err := service.AddSources(ctx, lein, detail.ID, []SourceRequest{{URL: "https://gobyexample.com/"}})
	if err != nil || len(grown.Sources) != 2 || grown.Sources[1].Scope != runs.ScopeSite {
		t.Errorf("grown: %+v, %v", grown, err)
	}

	if _, err := runService.Submit(ctx, runs.Request{URL: "https://example.com/"}, runs.Origin{UserID: lein}); err != nil ||
		producer.runs[len(producer.runs)-1].ReadDocuments {
		t.Errorf("a plain crawl reads no documents: %v", err)
	}
}

func TestABadSourceLeavesNoLab(t *testing.T) {
	store, pool, lein, _ := newStore(t)
	ctx := context.Background()

	producer := &fakeProducer{}
	service := NewService(store, runs.NewService(runs.NewStore(nil, pool, time.Minute), producer,
		runs.Limits{DefaultDepth: 2, MaxDepth: 5, DefaultLinks: 10, MaxLinks: 100}), nil, Limits{MaxSources: 5})

	for _, sources := range [][]SourceRequest{
		{{URL: "https://go.dev/"}, {URL: "mailto:someone@example.com"}},
		{{URL: "https://go.dev/", Depth: 50}},
		{{URL: "https://go.dev/"}, {URL: "go.dev"}},
	} {
		if _, err := service.Create(ctx, lein, "go", "", sources, nil); err == nil {
			t.Errorf("%+v accepted", sources)
		}
	}

	if labs, _ := service.List(ctx, lein); len(labs) != 0 || len(producer.runs) != 0 {
		t.Errorf("%d labs and %d runs left after refused requests", len(labs), len(producer.runs))
	}

	for _, name := range []string{"", "   ", "two\nlines", strings.Repeat("a", maxNameRunes+1)} {
		if _, err := service.Create(ctx, lein, name, "", nil, nil); !errors.Is(err, ErrInvalid) {
			t.Errorf("name %q: %v", name, err)
		}
	}

	off := NewService(store, nil, nil, Limits{MaxSources: 5})

	if _, err := off.Create(ctx, lein, "go", "", []SourceRequest{{URL: "https://go.dev/"}}, nil); !errors.Is(err, ErrCrawlingDisabled) {
		t.Errorf("sources without crawling: %v", err)
	}

	if _, err := off.Create(ctx, lein, "go", "", nil, nil); err != nil {
		t.Errorf("a lab without sources needs no crawler: %v", err)
	}
}

func TestEnginesAndSourcesFoundBySearch(t *testing.T) {
	store, _, lein, other := newStore(t)
	ctx := context.Background()

	producer := &fakeProducer{}
	service := NewService(store, runs.NewService(runs.NewStore(nil, store.pool, time.Minute), producer,
		runs.Limits{DefaultDepth: 2, MaxDepth: 5, DefaultLinks: 10, MaxLinks: 100}), nil,
		Limits{MaxSources: 3, DefaultEngines: []string{"brave", "bing"}})

	lab, err := service.Create(ctx, lein, "go", "", nil, nil)
	if err != nil || strings.Join(lab.Engines, ",") != "brave,bing" {
		t.Fatalf("defaults: %v, %v", lab.Engines, err)
	}

	if none, err := service.Create(ctx, lein, "offline", "", nil, []string{}); err != nil || len(none.Engines) != 0 {
		t.Errorf("no engines asked for: %v, %v", none.Engines, err)
	}

	if got, err := service.SetEngines(ctx, lein, lab.ID, []string{" wikipedia ", "wikipedia", "google cse"}); err != nil ||
		strings.Join(got.Engines, "|") != "wikipedia|google cse" {
		t.Errorf("set: %v, %v", got.Engines, err)
	}

	if _, err := service.SetEngines(ctx, other, lab.ID, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user's lab: %v", err)
	}

	if _, err := service.SetEngines(ctx, lein, lab.ID, []string{"a,b"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a comma would split into two engines at SearXNG: %v", err)
	}

	src, err := service.AddFound(ctx, lein, lab.ID, Found{URL: "go.dev/blog/pipelines", Query: "go pipelines", Engines: []string{"brave"}, Reason: "the official post"})
	if err != nil || src.Kind != KindSearch || src.Scope != ScopePage || src.Query != "go pipelines" || src.Reason != "the official post" ||
		src.Engines[0] != "brave" || src.URL != "https://go.dev/blog/pipelines" {
		t.Fatalf("found source: %+v, %v", src, err)
	}

	if last := producer.runs[len(producer.runs)-1]; last.MaxLinks != 1 || last.MaxDepth != 1 || last.UseCacheFlag {
		t.Errorf("a found page is crawled alone: %+v", last)
	}

	if _, err := service.AddFound(ctx, lein, lab.ID, Found{URL: "https://go.dev/blog/pipelines"}); !errors.Is(err, ErrSourceExists) {
		t.Errorf("found twice: %v", err)
	}

	if _, err := service.AddFound(ctx, other, lab.ID, Found{URL: "https://evil.example/"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("into another user's lab: %v", err)
	}
}
