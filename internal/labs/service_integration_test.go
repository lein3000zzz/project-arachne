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
	"web-crawler/internal/uploads"

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

func TestReferencesOfALabsDocuments(t *testing.T) {
	store, pool, lein, other := newStore(t)
	ctx := context.Background()

	producer := &fakeProducer{}
	service := NewService(store, runs.NewService(runs.NewStore(nil, store.pool, time.Minute), producer,
		runs.Limits{DefaultDepth: 2, MaxDepth: 5, DefaultLinks: 10, MaxLinks: 100}), nil, Limits{MaxSources: 5})

	lab, err := service.Create(ctx, lein, "resnets", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	const paper, spam = "https://arxiv.org/pdf/1512.03385", "https://spam.example/paper.pdf"

	for _, u := range []string{paper, spam, "https://arxiv.org/abs/1409.1556"} {
		src, err := store.AddSource(ctx, lein, lab.ID, Source{URL: u, Scope: ScopePage, MaxDepth: 1, MaxLinks: 1})
		if err != nil {
			t.Fatal(err)
		}

		crawled(t, pool, src.ID, u, 200)
	}

	refer := func(doc string, refs ...string) {
		for i, u := range refs {
			exec(t, pool, `INSERT INTO page_references (page_id, position, url, label) SELECT id, $2, $3, $4 FROM pages WHERE url = $1`,
				doc, i+1, u, fmt.Sprintf("[%d] cited %s", i+1, u))
		}
	}

	refer(paper, "https://arxiv.org/abs/1409.1556", "https://arxiv.org/abs/1502.03167", "https://doi.org/10.1109/5.726791")
	refer(spam, "https://evil.example/")
	exec(t, pool, `INSERT INTO page_reviews (url, content_hash, decision, reasons, summary, model, tool_calls, seconds)
		VALUES ($1, 'h', 'skip', '{spam}', '', 'm', 1, 1)`, spam)

	docs, err := service.References(ctx, lein, lab.ID)
	if err != nil || len(docs) != 1 || docs[0].URL != paper || len(docs[0].References) != 3 {
		t.Fatalf("documents: %+v, %v; want the paper alone, the spam set aside", docs, err)
	}

	if refs := docs[0].References; !refs[0].InLab || refs[1].InLab || refs[1].URL != "https://arxiv.org/abs/1502.03167" ||
		refs[1].Label != "[2] cited https://arxiv.org/abs/1502.03167" {
		t.Errorf("references: %+v", refs)
	}

	if _, err := service.References(ctx, other, lab.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user's lab: %v", err)
	}

	for _, picks := range [][]Cited{
		nil,
		{{Document: paper, URL: "https://evil.example/"}},
		{{Document: spam, URL: "https://evil.example/"}},
		{{Document: paper, URL: "https://unrelated.example/"}},
	} {
		if _, err := service.AddReferences(ctx, lein, lab.ID, picks); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v, want only the lab's documents' references", picks, err)
		}
	}

	if _, err := service.AddReferences(ctx, lein, lab.ID, []Cited{{Document: paper, URL: "https://arxiv.org/abs/1409.1556"}}); !errors.Is(err, ErrSourceExists) {
		t.Errorf("a reference the lab has as a source: %v", err)
	}

	runsBefore := len(producer.runs)

	detail, err := service.AddReferences(ctx, lein, lab.ID, []Cited{
		{Document: paper, URL: "https://arxiv.org/abs/1502.03167"},
		{Document: paper, URL: "https://doi.org/10.1109/5.726791"},
	})
	if err != nil || len(detail.Sources) != 5 || len(producer.runs) != runsBefore+2 {
		t.Fatalf("add: %d sources, %d runs, %v", len(detail.Sources), len(producer.runs)-runsBefore, err)
	}

	added := detail.Sources[3]
	if added.Kind != KindReference || added.Scope != ScopePage || added.CitedBy != paper ||
		added.Citation != "[2] cited https://arxiv.org/abs/1502.03167" || added.URL != "https://arxiv.org/abs/1502.03167" {
		t.Errorf("reference source: %+v", added)
	}

	if last := producer.runs[len(producer.runs)-1]; last.MaxLinks != 1 || last.MaxDepth != 1 || !last.ReadDocuments {
		t.Errorf("a reference is crawled alone, and read if it is a document: %+v", last)
	}

	if _, err := service.AddReferences(ctx, other, lab.ID, []Cited{{Document: paper, URL: "https://arxiv.org/abs/1502.03167"}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("into another user's lab: %v", err)
	}
}

func TestUploadsAreSourcesTheirLabsAlone(t *testing.T) {
	store, pool, lein, other := newStore(t)
	ctx := context.Background()

	var purged []string
	files := uploads.NewStore(pool, func(_ context.Context, url string) error {
		purged = append(purged, url)
		return nil
	})

	producer := &fakeProducer{}
	runService := runs.NewService(runs.NewStore(nil, store.pool, time.Minute), producer,
		runs.Limits{DefaultDepth: 2, MaxDepth: 5, DefaultLinks: 10, MaxLinks: 100})
	service := NewService(store, runService, nil, Limits{MaxSources: 3, Documents: true}).KeepsUploads(files)

	first, _ := service.Create(ctx, lein, "first", "", nil, nil)
	second, _ := service.Create(ctx, lein, "second", "", nil, nil)

	pdf := File{Name: "resnet.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.5\n%\xe2\xe3 resnet")}
	notes := File{Name: "notes.md", ContentType: "text/markdown", Data: []byte("# Notes\n\nResidual connections.")}

	detail, err := service.AddUploads(ctx, lein, first.ID, []File{pdf, notes})
	if err != nil || len(detail.Sources) != 2 {
		t.Fatalf("add: %+v, %v", detail.Sources, err)
	}

	src := detail.Sources[0]
	if src.Kind != KindUpload || !uploads.Is(src.URL) || src.Filename != "resnet.pdf" || src.Scope != ScopePage {
		t.Errorf("upload source: %+v", src)
	}

	if run := producer.runs[0]; run.StartURL != src.URL || !run.ReadDocuments || run.MaxLinks != 1 || run.MaxDepth != 1 {
		t.Errorf("an upload is crawled alone, and read: %+v", run)
	}

	if _, err := service.AddUploads(ctx, lein, first.ID, []File{pdf}); !errors.Is(err, ErrSourceExists) {
		t.Errorf("the same file twice in a lab: %v", err)
	}

	if again, err := service.AddUploads(ctx, lein, second.ID, []File{pdf}); err != nil || again.Sources[0].URL != src.URL {
		t.Errorf("the same file in another lab is the same upload: %+v, %v", again.Sources, err)
	}

	if _, err := service.AddUploads(ctx, other, first.ID, []File{notes}); !errors.Is(err, ErrNotFound) {
		t.Errorf("into another user's lab: %v", err)
	}

	if _, err := service.AddUploads(ctx, lein, second.ID, []File{notes, {Name: "a.txt", Data: []byte("a")}, {Name: "b.txt", Data: []byte("b")}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("past the lab's sources: %v", err)
	}

	textOnly := NewService(store, runService, nil, Limits{MaxSources: 3}).KeepsUploads(files)
	if _, err := textOnly.AddUploads(ctx, lein, second.ID, []File{{Name: "scan.pdf", Data: pdf.Data}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a document while documents are off: %v", err)
	}

	if _, err := service.AddSources(ctx, lein, second.ID, []SourceRequest{{URL: src.URL}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("an upload's address pasted as a link: %v", err)
	}

	if err := service.RemoveSource(ctx, lein, first.ID, src.ID); err != nil || len(purged) != 0 {
		t.Errorf("removed from one lab of two: %v, purged %v", err, purged)
	}

	if err := service.Delete(ctx, lein, second.ID); err != nil || len(purged) != 1 || purged[0] != src.URL {
		t.Errorf("the last lab holding it deleted: %v, purged %v", err, purged)
	}

	id, _ := uploads.ID(src.URL)
	if _, err := files.Owned(ctx, lein, id); !errors.Is(err, uploads.ErrNotFound) {
		t.Errorf("the file outlived its last lab: %v", err)
	}
}
