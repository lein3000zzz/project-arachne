package retrieval

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"web-crawler/internal/rank"
	"web-crawler/internal/vectorstore"
)

type fakeEmbedder struct {
	mu    sync.Mutex
	texts []string
	err   error
}

func (f *fakeEmbedder) Model() string { return "fake" }

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.texts = append(f.texts, texts...)
	if f.err != nil {
		return nil, f.err
	}

	return [][]float32{{1, 0}}, nil
}

type fakeStore struct {
	dense, text []vectorstore.Hit
	filters     []vectorstore.Filter
	textQueries []string
	mu          sync.Mutex
}

func (f *fakeStore) SearchDense(_ context.Context, _ []float32, limit int, filter vectorstore.Filter) ([]vectorstore.Hit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.filters = append(f.filters, filter)

	return f.dense[:min(limit, len(f.dense))], nil
}

func (f *fakeStore) SearchText(_ context.Context, query string, limit int, filter vectorstore.Filter) ([]vectorstore.Hit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.filters = append(f.filters, filter)
	f.textQueries = append(f.textQueries, query)

	return f.text[:min(limit, len(f.text))], nil
}

func hit(url string, score float32, text string) vectorstore.Hit {
	return vectorstore.Hit{URL: url, Host: "example.com", Title: "T " + url, Text: "T " + url + "\n\n" + text, Score: score}
}

func newRetriever(t *testing.T, store *fakeStore, embedder *fakeEmbedder) *Retriever {
	t.Helper()

	r, err := New(embedder, store, nil, Config{QueryPrefix: "Query: ", Candidates: 50, RRFK: 60})
	if err != nil {
		t.Fatal(err)
	}

	return r
}

func urls(results []Result) []string {
	out := make([]string, len(results))
	for i, res := range results {
		out[i] = res.URL
	}

	return out
}

func TestHybridFusesPagesByReciprocalRank(t *testing.T) {
	store := &fakeStore{
		dense: []vectorstore.Hit{
			hit("a", 0.9, "a1"), hit("a", 0.8, "a2"), hit("b", 0.7, "b1"), hit("c", 0.6, "c1"),
		},
		text: []vectorstore.Hit{
			hit("c", 12, "c-bm25"), hit("b", 9, "b-bm25"), hit("d", 5, "d1"),
		},
	}

	results, err := newRetriever(t, store, &fakeEmbedder{}).Search(context.Background(), Request{Query: "q", Limit: 10, Mode: ModeHybrid})
	if err != nil {
		t.Fatal(err)
	}

	// Page ranks: dense a=1 b=2 c=3, bm25 c=1 b=2 d=3. a's second chunk must not
	// push b down to 3.
	want := map[string]float64{
		"a": 1.0 / 61,
		"b": 1.0/62 + 1.0/62,
		"c": 1.0/63 + 1.0/61,
		"d": 1.0 / 63,
	}

	if got := urls(results); len(got) != 4 || got[0] != "c" || got[1] != "b" || got[2] != "a" || got[3] != "d" {
		t.Fatalf("order = %v, want [c b a d]", got)
	}

	for _, res := range results {
		if math.Abs(res.Score-want[res.URL]) > 1e-12 {
			t.Errorf("%s: score %v, want %v", res.URL, res.Score, want[res.URL])
		}
	}

	c := results[0]
	if c.Dense == nil || c.Dense.Rank != 3 || c.Dense.Score != 0.6 || c.BM25 == nil || c.BM25.Rank != 1 || c.BM25.Score != 12 {
		t.Errorf("c signals = dense %+v bm25 %+v", c.Dense, c.BM25)
	}

	if c.Snippet != "c-bm25" {
		t.Errorf("snippet = %q: it comes from the list that ranked the page higher, without the title", c.Snippet)
	}

	if a := results[2]; a.BM25 != nil || a.Snippet != "a1" {
		t.Errorf("a: bm25 %+v snippet %q; a page missing from a list has no signal there", a.BM25, a.Snippet)
	}
}

func TestSingleModesRankByTheirOwnScore(t *testing.T) {
	store := &fakeStore{
		dense: []vectorstore.Hit{hit("a", 0.9, ""), hit("b", 0.5, "")},
		text:  []vectorstore.Hit{hit("b", 7, ""), hit("a", 3, "")},
	}
	embedder := &fakeEmbedder{}
	r := newRetriever(t, store, embedder)

	dense, err := r.Search(context.Background(), Request{Query: "q", Limit: 10, Mode: ModeDense})
	if err != nil {
		t.Fatal(err)
	}

	if got := urls(dense); got[0] != "a" || dense[0].Score != float64(float32(0.9)) || dense[0].BM25 != nil {
		t.Errorf("dense = %+v", dense)
	}

	if len(store.textQueries) != 0 {
		t.Error("dense mode must not run a BM25 search")
	}

	bm25, err := r.Search(context.Background(), Request{Query: "q", Limit: 10, Mode: ModeBM25})
	if err != nil {
		t.Fatal(err)
	}

	if got := urls(bm25); got[0] != "b" || bm25[0].Score != 7 || bm25[0].Dense != nil {
		t.Errorf("bm25 = %+v", bm25)
	}

	if len(embedder.texts) != 1 {
		t.Errorf("bm25 mode must not embed: %d embeddings", len(embedder.texts))
	}
}

func TestSearchPrefixesOnlyTheEmbeddedQuery(t *testing.T) {
	store := &fakeStore{}
	embedder := &fakeEmbedder{}

	if _, err := newRetriever(t, store, embedder).Search(context.Background(),
		Request{Query: "  goroutines  ", Limit: 5, Host: " Go.Dev ", Mode: ModeHybrid}); err != nil {
		t.Fatal(err)
	}

	if len(embedder.texts) != 1 || embedder.texts[0] != "Query: goroutines" {
		t.Errorf("embedded %q", embedder.texts)
	}

	if len(store.textQueries) != 1 || store.textQueries[0] != "goroutines" {
		t.Errorf("BM25 got %q: the instruction would become search terms", store.textQueries)
	}

	for _, f := range store.filters {
		if f.Host != "go.dev" {
			t.Errorf("filter host %q, want go.dev", f.Host)
		}
	}
}

func TestSearchLimitsAndValidates(t *testing.T) {
	store := &fakeStore{dense: []vectorstore.Hit{hit("a", 3, ""), hit("b", 2, ""), hit("c", 1, "")}}
	r := newRetriever(t, store, &fakeEmbedder{})

	results, err := r.Search(context.Background(), Request{Query: "q", Limit: 2, Mode: ModeDense})
	if err != nil || len(results) != 2 {
		t.Errorf("limit 2: %d results, %v", len(results), err)
	}

	for _, req := range []Request{
		{Query: " ", Limit: 1, Mode: ModeDense},
		{Query: "q", Limit: 0, Mode: ModeDense},
		{Query: "q", Limit: 1, Mode: "magic"},
	} {
		if _, err := r.Search(context.Background(), req); err == nil {
			t.Errorf("%+v must be rejected", req)
		}
	}

	failing := newRetriever(t, store, &fakeEmbedder{err: errors.New("down")})
	if _, err := failing.Search(context.Background(), Request{Query: "q", Limit: 1, Mode: ModeHybrid}); err == nil {
		t.Error("an embedder failure must fail the hybrid search, not silently degrade it to BM25")
	}
}

type fakeRanks struct {
	scores map[string]rank.Scores
	err    error
}

func (f fakeRanks) LookupRanks(context.Context, []string) (map[string]rank.Scores, error) {
	return f.scores, f.err
}

func TestSearchAttachesRanksWithoutReordering(t *testing.T) {
	store := &fakeStore{dense: []vectorstore.Hit{hit("a", 0.9, ""), hit("b", 0.5, "")}}
	ranks := fakeRanks{scores: map[string]rank.Scores{"b": {Page: 0.9, Site: 0.8}}}

	r, err := New(&fakeEmbedder{}, store, ranks, Config{Candidates: 10, RRFK: 60})
	if err != nil {
		t.Fatal(err)
	}

	results, err := r.Search(context.Background(), Request{Query: "q", Limit: 10, Mode: ModeDense})
	if err != nil {
		t.Fatal(err)
	}

	if got := urls(results); got[0] != "a" || got[1] != "b" {
		t.Errorf("order = %v: ranks are reported, not used for ordering", got)
	}

	if results[0].PageRank != nil || results[1].PageRank == nil || results[1].PageRank.Site != 0.8 {
		t.Errorf("ranks = %+v, %+v", results[0].PageRank, results[1].PageRank)
	}

	failing, _ := New(&fakeEmbedder{}, store, fakeRanks{err: errors.New("postgres down")}, Config{Candidates: 10, RRFK: 60})
	if _, err := failing.Search(context.Background(), Request{Query: "q", Limit: 10, Mode: ModeDense}); err == nil {
		t.Error("a failed rank lookup must fail the search, not drop the scores silently")
	}
}
