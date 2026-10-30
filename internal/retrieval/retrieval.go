// Package retrieval answers a query with pages. Dense similarity and BM25 are
// fused with reciprocal rank fusion, the link graph can join the fusion as two
// more ranked lists, and a cross-encoder can reorder the top. Each result carries
// every signal it was ranked by, so each stage can be judged on its own.
package retrieval

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"web-crawler/internal/embedding"
	"web-crawler/internal/rank"
	"web-crawler/internal/vectorstore"

	"golang.org/x/sync/errgroup"
)

type Mode string

const (
	ModeHybrid Mode = "hybrid"
	ModeDense  Mode = "dense"
	ModeBM25   Mode = "bm25"
)

var Modes = []Mode{ModeHybrid, ModeDense, ModeBM25}

type Request struct {
	Query string
	Limit int
	Host  string
	// Non-nil searches only these pages; empty finds nothing.
	URLs []string
	Mode Mode
	// Nil takes the configured default.
	PageRankWeight *float64
	SiteRankWeight *float64
	Rerank         *bool
}

// Signal is a page's place in one ranked list: its rank among pages, from 1, and
// its score there - of its best chunk, for dense and BM25.
type Signal struct {
	Rank  int     `json:"rank"`
	Score float64 `json:"score"`
}

type Result struct {
	URL   string `json:"url"`
	Host  string `json:"host"`
	Title string `json:"title"`
	// The best-ranked chunk that matched, from whichever list ranked the page
	// higher; when reranked, whichever of its chunks the reranker scored highest.
	Snippet string `json:"snippet"`
	// The fusion score: RRF over the lists in play, or the single list's own score
	// when one list ranks alone. Results are in this order unless reranked.
	Score float64 `json:"score"`
	Dense *Signal `json:"dense,omitempty"`
	BM25  *Signal `json:"bm25,omitempty"`
	// Link-graph scores from the last ranking run; absent for a page not ranked
	// yet. They change the order only with a non-zero weight.
	PageRank *rank.Scores `json:"pagerank,omitempty"`
	// Set on the top candidates when the reranker ran: they are then ordered by
	// it, ahead of the rest.
	Rerank *Signal `json:"rerank,omitempty"`

	// Each distinct chunk the lists matched for the page. Which one ranks higher
	// depends on the other pages searched, a lab's few or the whole index, so
	// the reranker reads them all rather than let that decide.
	chunks []string
}

type RankSource interface {
	LookupRanks(ctx context.Context, urls []string) (map[string]rank.Scores, error)
}

type Reranker interface {
	Rerank(ctx context.Context, query string, documents []string) ([]float64, error)
	Model() string
}

type Config struct {
	QueryPrefix string
	Candidates  int
	RRFK        int

	PageRankWeight float64
	SiteRankWeight float64

	RerankByDefault  bool
	RerankCandidates int
}

type Retriever struct {
	embedder embedding.Embedder
	store    vectorstore.Searcher
	ranks    RankSource
	reranker Reranker
	cfg      Config
}

// New builds a retriever. ranks and reranker may be nil: results then carry no
// PageRank, and are never reranked.
func New(embedder embedding.Embedder, store vectorstore.Searcher, ranks RankSource, reranker Reranker, cfg Config) (*Retriever, error) {
	if embedder == nil || store == nil || cfg.Candidates <= 0 || cfg.RRFK <= 0 ||
		cfg.PageRankWeight < 0 || cfg.SiteRankWeight < 0 || reranker != nil && cfg.RerankCandidates <= 0 {
		return nil, ErrInvalidConfig
	}

	return &Retriever{embedder: embedder, store: store, ranks: ranks, reranker: reranker, cfg: cfg}, nil
}

func (r *Retriever) Search(ctx context.Context, req Request) ([]Result, error) {
	query := strings.TrimSpace(req.Query)

	switch {
	case query == "":
		return nil, ErrEmptyQuery
	case req.Limit <= 0:
		return nil, fmt.Errorf("%w: limit must be > 0", ErrInvalidRequest)
	case req.Mode != ModeHybrid && req.Mode != ModeDense && req.Mode != ModeBM25:
		return nil, fmt.Errorf("%w: %q", ErrUnknownMode, req.Mode)
	case req.PageRankWeight != nil && *req.PageRankWeight < 0, req.SiteRankWeight != nil && *req.SiteRankWeight < 0:
		return nil, fmt.Errorf("%w: rank weights must be >= 0", ErrInvalidRequest)
	case req.Rerank != nil && *req.Rerank && r.reranker == nil:
		return nil, ErrNoReranker
	}

	filter := vectorstore.Filter{Host: strings.ToLower(strings.TrimSpace(req.Host)), URLs: req.URLs}

	var dense, bm25 []page

	group, groupCtx := errgroup.WithContext(ctx)

	if req.Mode == ModeHybrid || req.Mode == ModeDense {
		group.Go(func() (err error) {
			dense, err = r.searchDense(groupCtx, query, filter)
			return err
		})
	}

	if req.Mode == ModeHybrid || req.Mode == ModeBM25 {
		group.Go(func() (err error) {
			bm25, err = r.searchBM25(groupCtx, query, filter)
			return err
		})
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	results := distinct(fuse(dense, bm25, r.cfg.RRFK, req.Mode))

	if err := r.attachRanks(ctx, results); err != nil {
		return nil, err
	}

	pageWeight, siteWeight := orDefault(req.PageRankWeight, r.cfg.PageRankWeight), orDefault(req.SiteRankWeight, r.cfg.SiteRankWeight)
	if pageWeight > 0 || siteWeight > 0 {
		fuseGraph(results, r.cfg.RRFK, req.Mode, pageWeight, siteWeight)
	}

	if r.reranker != nil && orDefault(req.Rerank, r.cfg.RerankByDefault) {
		if err := r.rerank(ctx, query, results); err != nil {
			return nil, err
		}
	}

	if len(results) > req.Limit {
		results = results[:req.Limit]
	}

	return results, nil
}

// widen keeps a float32 score's shortest decimal form: a plain float64(f) turns
// 0.6 into 0.6000000238418579 in every response.
func widen(f float32) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(float64(f), 'g', -1, 32), 64)
	return v
}

func orDefault[T any](v *T, fallback T) T {
	if v == nil {
		return fallback
	}

	return *v
}

func (r *Retriever) attachRanks(ctx context.Context, results []Result) error {
	if r.ranks == nil || len(results) == 0 {
		return nil
	}

	urls := make([]string, len(results))
	for i, res := range results {
		urls[i] = res.URL
	}

	ranks, err := r.ranks.LookupRanks(ctx, urls)
	if err != nil {
		return fmt.Errorf("looking up page ranks: %w", err)
	}

	for i := range results {
		if scores, ok := ranks[results[i].URL]; ok {
			results[i].PageRank = &scores
		}
	}

	return nil
}

func (r *Retriever) searchDense(ctx context.Context, query string, filter vectorstore.Filter) ([]page, error) {
	vectors, err := r.embedder.Embed(ctx, []string{r.cfg.QueryPrefix + query})
	if err != nil {
		return nil, fmt.Errorf("embedding the query: %w", err)
	}

	if len(vectors) != 1 {
		return nil, fmt.Errorf("embedding the query: got %d vectors for one text", len(vectors))
	}

	hits, err := r.store.SearchDense(ctx, vectors[0], r.cfg.Candidates, filter)
	if err != nil {
		return nil, err
	}

	return pages(hits), nil
}

func (r *Retriever) searchBM25(ctx context.Context, query string, filter vectorstore.Filter) ([]page, error) {
	hits, err := r.store.SearchText(ctx, query, r.cfg.Candidates, filter)
	if err != nil {
		return nil, err
	}

	return pages(hits), nil
}

// page is a URL's best chunk in one list.
type page struct {
	hit  vectorstore.Hit
	rank int
}

// pages folds a chunk list into a page list: a page ranks where its best chunk
// does, and its other chunks drop out. Ranking chunks instead would let one long
// page fill the results, and RRF would count it once per chunk.
func pages(hits []vectorstore.Hit) []page {
	seen := make(map[string]struct{}, len(hits))
	out := make([]page, 0, len(hits))

	for _, hit := range hits {
		if _, dup := seen[hit.URL]; dup {
			continue
		}

		seen[hit.URL] = struct{}{}
		out = append(out, page{hit: hit, rank: len(out) + 1})
	}

	return out
}

func fuse(dense, bm25 []page, k int, mode Mode) []Result {
	byURL := make(map[string]*Result)
	order := make([]*Result, 0, len(dense)+len(bm25))

	get := func(p page) *Result {
		if res, ok := byURL[p.hit.URL]; ok {
			return res
		}

		res := &Result{URL: p.hit.URL, Host: p.hit.Host, Title: p.hit.Title}
		byURL[p.hit.URL] = res
		order = append(order, res)

		return res
	}

	for _, p := range dense {
		res := get(p)
		res.Dense = &Signal{Rank: p.rank, Score: widen(p.hit.Score)}
		res.Snippet = snippet(p.hit)
		res.chunks = []string{res.Snippet}
	}

	for _, p := range bm25 {
		res := get(p)
		res.BM25 = &Signal{Rank: p.rank, Score: widen(p.hit.Score)}

		text := snippet(p.hit)
		if res.Dense == nil || p.rank < res.Dense.Rank {
			res.Snippet = text
		}

		if !slices.Contains(res.chunks, text) {
			res.chunks = append(res.chunks, text)
		}
	}

	for _, res := range order {
		switch mode {
		case ModeDense:
			res.Score = res.Dense.Score
		case ModeBM25:
			res.Score = res.BM25.Score
		default:
			for _, signal := range []*Signal{res.Dense, res.BM25} {
				if signal != nil {
					res.Score += 1 / float64(k+signal.Rank)
				}
			}
		}
	}

	sort.SliceStable(order, func(i, j int) bool {
		if order[i].Score != order[j].Score {
			return order[i].Score > order[j].Score
		}

		return bestRank(order[i]) < bestRank(order[j])
	})

	results := make([]Result, len(order))
	for i, res := range order {
		results[i] = *res
	}

	return results
}

// distinct keeps the best-ranked of results whose excerpts are the same text.
// A site's menu leaking into every page, or one document mirrored under many
// addresses, would otherwise take every place - and every rerank slot.
func distinct(results []Result) []Result {
	seen := make(map[string]bool, len(results))
	out := results[:0]

	for _, res := range results {
		key := strings.Join(strings.Fields(res.Snippet), " ")
		if key != "" && seen[key] {
			continue
		}

		seen[key] = true
		out = append(out, res)
	}

	return out
}

func bestRank(res *Result) int {
	best := int(^uint(0) >> 1)

	for _, signal := range []*Signal{res.Dense, res.BM25} {
		if signal != nil && signal.Rank < best {
			best = signal.Rank
		}
	}

	return best
}

// snippet drops the title the indexer put in front of every chunk.
func snippet(hit vectorstore.Hit) string {
	return strings.TrimPrefix(hit.Text, hit.Title+"\n\n")
}
