// Package rank computes PageRank over a link graph held in memory. It does no
// I/O: callers load the graph and store the scores.
package rank

import (
	"fmt"
	"math"
	"slices"
)

// Scores are a page's two ranks, both summing to 1 over their graph: Page over
// pages, Site over sites, so a page shares its site's score with its siblings.
type Scores struct {
	Page float64 `json:"page"`
	Site float64 `json:"site"`
}

type Edge struct {
	From, To int32
}

// Graph is a directed graph in compressed sparse row form: the targets of node i
// are targets[offsets[i]:offsets[i+1]].
type Graph struct {
	offsets []int64
	targets []int32
}

// NewGraph builds a graph of n nodes. Self-loops and repeated edges are dropped:
// a page voting for itself, or twice for the same page, is not a vote.
func NewGraph(n int, edges []Edge) (*Graph, error) {
	if n <= 0 {
		return nil, fmt.Errorf("%w: a graph needs at least one node", ErrInvalidGraph)
	}

	sorted := make([]Edge, 0, len(edges))

	for _, e := range edges {
		if e.From < 0 || int(e.From) >= n || e.To < 0 || int(e.To) >= n {
			return nil, fmt.Errorf("%w: edge %d->%d outside %d nodes", ErrInvalidGraph, e.From, e.To, n)
		}

		if e.From != e.To {
			sorted = append(sorted, e)
		}
	}

	slices.SortFunc(sorted, func(a, b Edge) int {
		if a.From != b.From {
			return int(a.From - b.From)
		}

		return int(a.To - b.To)
	})

	sorted = slices.Compact(sorted)

	g := &Graph{offsets: make([]int64, n+1), targets: make([]int32, len(sorted))}

	for i, e := range sorted {
		g.offsets[e.From+1]++
		g.targets[i] = e.To
	}

	for i := 1; i <= n; i++ {
		g.offsets[i] += g.offsets[i-1]
	}

	return g, nil
}

func (g *Graph) Nodes() int {
	return len(g.offsets) - 1
}

func (g *Graph) Edges() int {
	return len(g.targets)
}

type Options struct {
	// Probability of following a link rather than jumping to a random page.
	Damping float64
	// Iteration stops once the scores move less than this in total (L1).
	Tolerance     float64
	MaxIterations int
}

type Result struct {
	// Scores sum to 1; a page with no links in or out gets (1-d)/n plus its share
	// of the dangling mass.
	Scores     []float64
	Iterations int
	Residual   float64
	Converged  bool
}

// PageRank runs the power iteration. Rank held by pages without outgoing links -
// dead ends and pages never fetched - is spread evenly over all pages, as if the
// surfer jumped at random, so no rank leaks out of the graph.
func PageRank(g *Graph, opts Options) (Result, error) {
	if opts.Damping <= 0 || opts.Damping >= 1 || opts.Tolerance <= 0 || opts.MaxIterations <= 0 {
		return Result{}, fmt.Errorf("%w: %+v", ErrInvalidOptions, opts)
	}

	n := g.Nodes()
	d := opts.Damping
	uniform := 1 / float64(n)

	current := make([]float64, n)
	next := make([]float64, n)

	for i := range current {
		current[i] = uniform
	}

	result := Result{Residual: math.Inf(1)}

	for result.Iterations < opts.MaxIterations && !result.Converged {
		result.Iterations++

		var dangling float64

		for i := 0; i < n; i++ {
			if g.offsets[i] == g.offsets[i+1] {
				dangling += current[i]
			}
		}

		base := (1-d)*uniform + d*dangling*uniform
		for i := range next {
			next[i] = base
		}

		for i := 0; i < n; i++ {
			from, to := g.offsets[i], g.offsets[i+1]
			if from == to {
				continue
			}

			share := d * current[i] / float64(to-from)
			for _, target := range g.targets[from:to] {
				next[target] += share
			}
		}

		result.Residual = 0
		for i := range next {
			result.Residual += math.Abs(next[i] - current[i])
		}

		current, next = next, current
		result.Converged = result.Residual < opts.Tolerance
	}

	result.Scores = current

	return result, nil
}

// Collapse maps a graph onto groups of its nodes - pages onto their sites - with
// one edge from group A to group B when any node of A links to any node of B.
// Edges within a group are dropped, and so is the count: a link repeated on every
// page of a site, like a footer, is still one edge.
func Collapse(edges []Edge, groupOf []int32) []Edge {
	seen := make(map[Edge]struct{})
	out := make([]Edge, 0)

	for _, e := range edges {
		group := Edge{From: groupOf[e.From], To: groupOf[e.To]}
		if group.From == group.To {
			continue
		}

		if _, dup := seen[group]; dup {
			continue
		}

		seen[group] = struct{}{}
		out = append(out, group)
	}

	return out
}
