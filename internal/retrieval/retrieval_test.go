package retrieval

import (
	"context"
	"errors"
	"math"
	"strings"
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

	r, err := New(embedder, store, nil, nil, Config{QueryPrefix: "Query: ", Candidates: 50, RRFK: 60})
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

	if got := urls(dense); got[0] != "a" || dense[0].Score != 0.9 || dense[0].BM25 != nil {
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
		Request{Query: "  goroutines  ", Limit: 5, Host: " Go.Dev ", URLs: []string{"https://go.dev/doc/"}, Mode: ModeHybrid}); err != nil {
		t.Fatal(err)
	}

	if len(embedder.texts) != 1 || embedder.texts[0] != "Query: goroutines" {
		t.Errorf("embedded %q", embedder.texts)
	}

	if len(store.textQueries) != 1 || store.textQueries[0] != "goroutines" {
		t.Errorf("BM25 got %q: the instruction would become search terms", store.textQueries)
	}

	for _, f := range store.filters {
		if f.Host != "go.dev" || len(f.URLs) != 1 || f.URLs[0] != "https://go.dev/doc/" {
			t.Errorf("filter %+v, want host go.dev and the one URL", f)
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

	r, err := New(&fakeEmbedder{}, store, ranks, nil, Config{Candidates: 10, RRFK: 60})
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

	failing, _ := New(&fakeEmbedder{}, store, fakeRanks{err: errors.New("postgres down")}, nil, Config{Candidates: 10, RRFK: 60})
	if _, err := failing.Search(context.Background(), Request{Query: "q", Limit: 10, Mode: ModeDense}); err == nil {
		t.Error("a failed rank lookup must fail the search, not drop the scores silently")
	}
}

type fakeReranker struct {
	scores map[string]float64
	// Scores a document containing the key, over scores.
	byText map[string]float64
	docs   []string
	err    error
}

func (f *fakeReranker) Model() string { return "fake-reranker" }

func (f *fakeReranker) Rerank(_ context.Context, _ string, docs []string) ([]float64, error) {
	f.docs = docs
	if f.err != nil {
		return nil, f.err
	}

	out := make([]float64, len(docs))
	for i, d := range docs {
		for key, score := range f.scores {
			if strings.HasPrefix(d, "T "+key+"\n") {
				out[i] = score
			}
		}

		for key, score := range f.byText {
			if strings.Contains(d, key) {
				out[i] = score
			}
		}
	}

	return out, nil
}

func TestPageRankJoinsTheFusionOnlyWithAWeight(t *testing.T) {
	store := &fakeStore{
		dense: []vectorstore.Hit{hit("a", 0.9, ""), hit("b", 0.8, ""), hit("c", 0.7, "")},
		text:  []vectorstore.Hit{hit("a", 9, ""), hit("b", 8, ""), hit("c", 7, "")},
	}
	ranks := fakeRanks{scores: map[string]rank.Scores{
		"a": {Page: 0.1, Site: 0.5}, "b": {Page: 0.2, Site: 0.5}, "c": {Page: 0.9, Site: 0.5},
	}}

	r, err := New(&fakeEmbedder{}, store, ranks, nil, Config{Candidates: 10, RRFK: 1})
	if err != nil {
		t.Fatal(err)
	}

	plain, _ := r.Search(context.Background(), Request{Query: "q", Limit: 10, Mode: ModeHybrid})
	if got := urls(plain); got[0] != "a" {
		t.Errorf("without a weight the order is retrieval's: %v", got)
	}

	heavy := 10.0
	weighted, err := r.Search(context.Background(), Request{Query: "q", Limit: 10, Mode: ModeHybrid, PageRankWeight: &heavy})
	if err != nil {
		t.Fatal(err)
	}

	if got := urls(weighted); got[0] != "c" {
		t.Errorf("a heavy page-rank weight puts the most linked page first: %v", got)
	}

	// Equal site scores share rank 1: the site list adds the same to everyone.
	site := 10.0
	siteOnly, _ := r.Search(context.Background(), Request{Query: "q", Limit: 10, Mode: ModeHybrid, SiteRankWeight: &site})
	if got := urls(siteOnly); got[0] != "a" || got[2] != "c" {
		t.Errorf("one site, one rank: order must not change: %v", got)
	}

	bm25, _ := r.Search(context.Background(), Request{Query: "q", Limit: 10, Mode: ModeBM25, PageRankWeight: &heavy})
	// c is third by BM25 and first by page rank: 1/(1+3) + 10/(1+1), not BM25's 7.
	if got := urls(bm25); got[0] != "c" || math.Abs(bm25[0].Score-(1.0/4+10.0/2)) > 1e-12 {
		t.Errorf("a single list joins the fusion as its RRF term, not raw BM25: %v %v", got, bm25[0].Score)
	}

	negative := -1.0
	if _, err := r.Search(context.Background(), Request{Query: "q", Limit: 10, Mode: ModeHybrid, PageRankWeight: &negative}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("negative weight: %v", err)
	}
}

func TestRerankReordersTheTopCandidates(t *testing.T) {
	store := &fakeStore{dense: []vectorstore.Hit{
		hit("a", 0.9, "alpha"), hit("b", 0.8, "beta"), hit("c", 0.7, "gamma"), hit("d", 0.6, "delta"),
	}}
	reranker := &fakeReranker{scores: map[string]float64{"a": 0.1, "b": 0.3, "c": 0.9}}

	r, err := New(&fakeEmbedder{}, store, nil, reranker, Config{Candidates: 10, RRFK: 60, RerankByDefault: true, RerankCandidates: 3})
	if err != nil {
		t.Fatal(err)
	}

	results, err := r.Search(context.Background(), Request{Query: "q", Limit: 10, Mode: ModeDense})
	if err != nil {
		t.Fatal(err)
	}

	if got := urls(results); strings.Join(got, "") != "cbad" {
		t.Errorf("order = %v: the top 3 by reranker, then the rest in fusion order", got)
	}

	if results[0].Rerank == nil || results[0].Rerank.Rank != 1 || results[0].Rerank.Score != 0.9 || results[3].Rerank != nil {
		t.Errorf("rerank signals: %+v / %+v", results[0].Rerank, results[3].Rerank)
	}

	if results[0].Dense.Rank != 3 {
		t.Errorf("the dense signal keeps its own rank: %+v", results[0].Dense)
	}

	if len(reranker.docs) != 3 || reranker.docs[0] != "T a\n\nalpha" {
		t.Errorf("reranker saw %q: title and best chunk, without the duplicated title", reranker.docs)
	}

	off := false
	plain, _ := r.Search(context.Background(), Request{Query: "q", Limit: 10, Mode: ModeDense, Rerank: &off})
	if got := urls(plain); strings.Join(got, "") != "abcd" || plain[0].Rerank != nil {
		t.Errorf("rerank=false: %v", got)
	}

	reranker.err = errors.New("reranker down")
	if _, err := r.Search(context.Background(), Request{Query: "q", Limit: 10, Mode: ModeDense}); err == nil {
		t.Error("a failed rerank must fail the search, not quietly return another order")
	}

	none, _ := New(&fakeEmbedder{}, store, nil, nil, Config{Candidates: 10, RRFK: 60})
	on := true
	if _, err := none.Search(context.Background(), Request{Query: "q", Limit: 10, Mode: ModeDense, Rerank: &on}); !errors.Is(err, ErrNoReranker) {
		t.Errorf("rerank requested without a reranker: %v", err)
	}
}

// Inside a lab's few pages a page's keyword chunk can outrank its meaning
// chunk; the reranker must still see the better one.
func TestRerankReadsEveryChunkAPageMatched(t *testing.T) {
	store := &fakeStore{
		dense: []vectorstore.Hit{hit("a", 0.9, "goroutines are lightweight threads"), hit("b", 0.8, "b dense")},
		text:  []vectorstore.Hit{hit("a", 9, "goroutine goroutine goroutine"), hit("b", 8, "b dense")},
	}
	reranker := &fakeReranker{byText: map[string]float64{"lightweight": 3, "goroutine goroutine": -5, "b dense": 1}}

	r, err := New(&fakeEmbedder{}, store, nil, reranker, Config{Candidates: 10, RRFK: 60, RerankByDefault: true, RerankCandidates: 5})
	if err != nil {
		t.Fatal(err)
	}

	results, err := r.Search(context.Background(), Request{Query: "q", Limit: 10, Mode: ModeHybrid})
	if err != nil {
		t.Fatal(err)
	}

	if len(reranker.docs) != 3 {
		t.Errorf("reranked %d documents, want 3: both chunks of a, the one chunk of b once", len(reranker.docs))
	}

	if results[0].URL != "a" || results[0].Rerank.Score != 3 || results[0].Snippet != "goroutines are lightweight threads" {
		t.Errorf("a = %+v: its best chunk's score and text", results[0])
	}
}

func TestPagesWithTheSameExcerptCountOnce(t *testing.T) {
	menu := "Why Go  Common problems companies solve with Go"
	store := &fakeStore{dense: []vectorstore.Hit{
		hit("tab1", 0.9, menu), hit("tab2", 0.8, menu+"\n"), hit("doc", 0.7, "errgroup provides synchronization"), hit("tab3", 0.6, menu),
	}}

	results, err := newRetriever(t, store, &fakeEmbedder{}).Search(context.Background(), Request{Query: "q", Limit: 10, Mode: ModeDense})
	if err != nil {
		t.Fatal(err)
	}

	if got := urls(results); strings.Join(got, " ") != "tab1 doc" {
		t.Errorf("results = %v: one page per excerpt, the best-ranked kept", got)
	}
}
