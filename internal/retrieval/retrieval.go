// Package retrieval answers a query with pages, ranked by dense similarity, BM25,
// or both fused with reciprocal rank fusion. Each result carries the signals it
// was ranked by, so later stages - PageRank, a reranker - and the evaluation can
// see every score separately.
package retrieval

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"web-crawler/internal/embedding"
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
	Mode  Mode
}

// Signal is a page's place in one ranked list: its rank among pages, from 1, and
// the score of its best chunk there.
type Signal struct {
	Rank  int     `json:"rank"`
	Score float32 `json:"score"`
}

type Result struct {
	URL   string `json:"url"`
	Host  string `json:"host"`
	Title string `json:"title"`
	// The best-ranked chunk that matched, from whichever list ranked the page higher.
	Snippet string `json:"snippet"`
	// What the results are ordered by: the RRF score in hybrid mode, the single
	// list's score otherwise.
	Score float64 `json:"score"`
	Dense *Signal `json:"dense,omitempty"`
	BM25  *Signal `json:"bm25,omitempty"`
}

type Config struct {
	QueryPrefix string
	Candidates  int
	RRFK        int
}

type Retriever struct {
	embedder embedding.Embedder
	store    vectorstore.Searcher
	cfg      Config
}

func New(embedder embedding.Embedder, store vectorstore.Searcher, cfg Config) (*Retriever, error) {
	if embedder == nil || store == nil || cfg.Candidates <= 0 || cfg.RRFK <= 0 {
		return nil, ErrInvalidConfig
	}

	return &Retriever{embedder: embedder, store: store, cfg: cfg}, nil
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
	}

	filter := vectorstore.Filter{Host: strings.ToLower(strings.TrimSpace(req.Host))}

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

	results := fuse(dense, bm25, r.cfg.RRFK, req.Mode)
	if len(results) > req.Limit {
		results = results[:req.Limit]
	}

	return results, nil
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
		res.Dense = &Signal{Rank: p.rank, Score: p.hit.Score}
		res.Snippet = snippet(p.hit)
	}

	for _, p := range bm25 {
		res := get(p)
		res.BM25 = &Signal{Rank: p.rank, Score: p.hit.Score}

		if res.Dense == nil || p.rank < res.Dense.Rank {
			res.Snippet = snippet(p.hit)
		}
	}

	for _, res := range order {
		switch mode {
		case ModeDense:
			res.Score = float64(res.Dense.Score)
		case ModeBM25:
			res.Score = float64(res.BM25.Score)
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
