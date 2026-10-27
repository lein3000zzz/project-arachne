package retrieval

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// A cross-encoder reads the whole pair; past a few hundred tokens it only slows
// down and the model truncates anyway.
const maxRerankRunes = 2000

// fuseGraph adds the link graph to the fusion as two more ranked lists: the
// candidates ordered by page PageRank, and by site PageRank. Ranks rather than
// raw scores keep them on the same footing as the retrieval lists, whose scores
// are not comparable either. A single retrieval list is turned into its RRF term
// first, so the sum has one scale.
func fuseGraph(results []Result, k int, mode Mode, pageWeight, siteWeight float64) {
	if mode != ModeHybrid {
		for i := range results {
			results[i].Score = 1 / float64(k+i+1)
		}
	}

	addRankList(results, k, pageWeight, func(r Result) (float64, bool) {
		if r.PageRank == nil {
			return 0, false
		}

		return r.PageRank.Page, true
	})

	addRankList(results, k, siteWeight, func(r Result) (float64, bool) {
		if r.PageRank == nil {
			return 0, false
		}

		return r.PageRank.Site, true
	})

	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
}

// addRankList adds weight/(k+rank) for each result's rank by value; results
// without a value take no part. Equal values share a rank, as siblings on one
// site share a site score.
func addRankList(results []Result, k int, weight float64, value func(Result) (float64, bool)) {
	if weight == 0 {
		return
	}

	order := make([]int, 0, len(results))
	for i, r := range results {
		if _, ok := value(r); ok {
			order = append(order, i)
		}
	}

	sort.SliceStable(order, func(a, b int) bool {
		va, _ := value(results[order[a]])
		vb, _ := value(results[order[b]])

		return va > vb
	})

	rank := 0
	prev := -1.0

	for pos, i := range order {
		v, _ := value(results[i])
		if pos == 0 || v != prev {
			rank = pos + 1
			prev = v
		}

		results[i].Score += weight / float64(k+rank)
	}
}

// rerank reorders the top candidates by the cross-encoder; the rest follow in
// fusion order.
func (r *Retriever) rerank(ctx context.Context, query string, results []Result) error {
	top := results[:min(r.cfg.RerankCandidates, len(results))]
	if len(top) == 0 {
		return nil
	}

	documents := make([]string, len(top))
	for i, res := range top {
		documents[i] = rerankDocument(res)
	}

	scores, err := r.reranker.Rerank(ctx, query, documents)
	if err != nil {
		return fmt.Errorf("reranking: %w", err)
	}

	for i := range top {
		top[i].Rerank = &Signal{Score: scores[i]}
	}

	sort.SliceStable(top, func(i, j int) bool { return top[i].Rerank.Score > top[j].Rerank.Score })

	for i := range top {
		top[i].Rerank.Rank = i + 1
	}

	return nil
}

func rerankDocument(res Result) string {
	text := strings.TrimSpace(res.Title + "\n\n" + res.Snippet)

	if runes := []rune(text); len(runes) > maxRerankRunes {
		text = string(runes[:maxRerankRunes])
	}

	return text
}
