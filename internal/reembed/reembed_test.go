package reembed

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"web-crawler/internal/vectorstore"

	"go.uber.org/zap"
)

type store struct {
	mu    sync.Mutex
	pages map[string][]vectorstore.Row
}

func (s *store) URLs(context.Context) ([]string, error) {
	var out []string
	for u := range s.pages {
		out = append(out, u)
	}

	slices.Sort(out)

	return out, nil
}

func (s *store) Chunks(_ context.Context, url string) ([]vectorstore.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.pages[url]), nil
}

func (s *store) Replace(_ context.Context, url string, rows []vectorstore.Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pages[url] = slices.Clone(rows)

	return nil
}

type embedder struct {
	calls [][]string
	fail  bool
}

func (e *embedder) Model() string { return "new-model" }

func (e *embedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls = append(e.calls, texts)
	if e.fail {
		return nil, errors.New("rate limited")
	}

	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = []float32{float32(len(t)), 1}
	}

	return out, nil
}

func chunk(url, id, hash, text string, ordinal int32) vectorstore.Row {
	return vectorstore.Row{ID: id, URL: url, Host: "example.com", Ordinal: ordinal, Title: "T", Text: text, ContentHash: hash,
		RunID: "run", IndexedAt: 42, Vector: []float32{9, 9, 9}}
}

func oldIndex() *store {
	return &store{pages: map[string][]vectorstore.Row{
		"https://a": {chunk("https://a", "a1", "ha", "T\n\nfirst part of a", 0), chunk("https://a", "a2", "ha", "T\n\nsecond part of a", 1)},
		"https://b": {chunk("https://b", "b1", "hb", "T\n\nall of b", 0)},
		"https://c": {chunk("https://c", "c1", "hc", "T\n\nall of c", 0)},
	}}
}

func TestEveryPageIsCopiedWithNewVectors(t *testing.T) {
	from, to, emb := oldIndex(), &store{pages: map[string][]vectorstore.Row{}}, &embedder{}

	stats, err := Run(context.Background(), zap.NewNop().Sugar(), from, to, emb, Config{Batch: 2})
	if err != nil || stats.Copied != 3 || stats.Chunks != 4 || stats.Pages != 3 {
		t.Fatalf("stats %+v, %v", stats, err)
	}

	// a's two chunks fill a group; b and c go together.
	if len(emb.calls) != 2 || len(emb.calls[0]) != 2 || emb.calls[0][0] != "T\n\nfirst part of a" {
		t.Errorf("embedded %q", emb.calls)
	}

	got := to.pages["https://a"][1]
	if got.ID != "a2" || got.Text != "T\n\nsecond part of a" || got.ContentHash != "ha" || got.IndexedAt != 42 || got.RunID != "run" ||
		!slices.Equal(got.Vector, []float32{float32(len("T\n\nsecond part of a")), 1}) {
		t.Errorf("copied row %+v", got)
	}

	emb.calls = nil

	if stats, err := Run(context.Background(), zap.NewNop().Sugar(), from, to, emb, Config{Batch: 2}); err != nil || stats.Current != 3 || len(emb.calls) != 0 {
		t.Errorf("a second run must embed nothing: %+v, %v, %d calls", stats, err, len(emb.calls))
	}
}

func TestWhatTheTargetHasIsLeftAloneOrFinished(t *testing.T) {
	from, emb := oldIndex(), &embedder{}
	to := &store{pages: map[string][]vectorstore.Row{
		// The indexer re-read b since the switch: newer content, its own hash.
		"https://b": {chunk("https://b", "b9", "hb-new", "T\n\nb as it is now", 0)},
		// A copy of a cut short: one chunk of two.
		"https://a": {chunk("https://a", "a1", "ha", "T\n\nfirst part of a", 0)},
	}}

	stats, err := Run(context.Background(), zap.NewNop().Sugar(), from, to, emb, Config{Batch: 10})
	if err != nil || stats.Newer != 1 || stats.Copied != 2 {
		t.Fatalf("stats %+v, %v", stats, err)
	}

	if b := to.pages["https://b"]; len(b) != 1 || b[0].ID != "b9" {
		t.Errorf("the indexer's newer page was overwritten: %+v", b)
	}

	if a := to.pages["https://a"]; len(a) != 2 {
		t.Errorf("a copy cut short must be finished: %+v", a)
	}
}

func TestFailuresAreLeftForTheNextRun(t *testing.T) {
	from, to := oldIndex(), &store{pages: map[string][]vectorstore.Row{}}

	stats, err := Run(context.Background(), zap.NewNop().Sugar(), from, to, &embedder{fail: true}, Config{Batch: 10})
	if err == nil || !strings.Contains(err.Error(), "run again") || stats.Failed != 3 || stats.Copied != 0 || len(to.pages) != 0 {
		t.Fatalf("stats %+v, %v", stats, err)
	}

	if stats, err := Run(context.Background(), zap.NewNop().Sugar(), from, to, &embedder{}, Config{Batch: 10}); err != nil || stats.Copied != 3 {
		t.Errorf("the next run: %+v, %v", stats, err)
	}
}

func TestADryRunChangesNothing(t *testing.T) {
	from, to, emb := oldIndex(), &store{pages: map[string][]vectorstore.Row{}}, &embedder{}

	var progress []Stats

	stats, err := Run(context.Background(), zap.NewNop().Sugar(), from, to, emb,
		Config{Batch: 2, DryRun: true, Progress: func(s Stats) { progress = append(progress, s) }})
	if err != nil || stats.Copied != 3 || stats.Chunks != 4 || len(emb.calls) != 0 || len(to.pages) != 0 || len(progress) == 0 {
		t.Errorf("dry run: %+v, %v, %d calls, %d pages written", stats, err, len(emb.calls), len(to.pages))
	}
}
