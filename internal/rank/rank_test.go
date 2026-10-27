package rank

import (
	"errors"
	"math"
	"math/rand"
	"testing"
)

var defaults = Options{Damping: 0.85, Tolerance: 1e-12, MaxIterations: 1000}

func run(t *testing.T, n int, edges []Edge) []float64 {
	t.Helper()

	g, err := NewGraph(n, edges)
	if err != nil {
		t.Fatal(err)
	}

	res, err := PageRank(g, defaults)
	if err != nil {
		t.Fatal(err)
	}

	if !res.Converged {
		t.Fatalf("no convergence after %d iterations, residual %g", res.Iterations, res.Residual)
	}

	return res.Scores
}

func assertScores(t *testing.T, got, want []float64, tol float64) {
	t.Helper()

	for i := range want {
		if math.Abs(got[i]-want[i]) > tol {
			t.Errorf("score[%d] = %.10f, want %.10f", i, got[i], want[i])
		}
	}
}

func TestPageRankMatchesPublishedValues(t *testing.T) {
	// A->B, A->C, B->C, C->A: the example networkx documents, d = 0.85.
	assertScores(t, run(t, 3, []Edge{{0, 1}, {0, 2}, {1, 2}, {2, 0}}),
		[]float64{0.3877897117, 0.2148106275, 0.3973996608}, 1e-9)
}

func TestPageRankSpreadsDanglingRank(t *testing.T) {
	// D has no outgoing links; its rank must flow back to everyone, not vanish.
	assertScores(t, run(t, 4, []Edge{{0, 1}, {1, 2}, {2, 0}, {2, 3}}),
		[]float64{0.2137621541, 0.2646222887, 0.3078534031, 0.2137621541}, 1e-9)
}

// exact solves r = d*S^T r + (1-d)/n * 1, with S the transition matrix whose
// dangling rows are uniform, by Gaussian elimination: an independent check on
// the iteration.
func exact(n int, edges []Edge, d float64) []float64 {
	out := make([][]int, n)
	seen := make(map[Edge]bool)

	for _, e := range edges {
		if e.From != e.To && !seen[e] {
			seen[e] = true
			out[e.From] = append(out[e.From], int(e.To))
		}
	}

	a := make([][]float64, n)
	for i := range a {
		a[i] = make([]float64, n+1)
		a[i][i] = 1
		a[i][n] = (1 - d) / float64(n)
	}

	for j := 0; j < n; j++ {
		if len(out[j]) == 0 {
			for i := 0; i < n; i++ {
				a[i][j] -= d / float64(n)
			}

			continue
		}

		for _, i := range out[j] {
			a[i][j] -= d / float64(len(out[j]))
		}
	}

	for col := 0; col < n; col++ {
		pivot := col
		for row := col + 1; row < n; row++ {
			if math.Abs(a[row][col]) > math.Abs(a[pivot][col]) {
				pivot = row
			}
		}

		a[col], a[pivot] = a[pivot], a[col]

		for row := 0; row < n; row++ {
			if row == col {
				continue
			}

			f := a[row][col] / a[col][col]
			for k := col; k <= n; k++ {
				a[row][k] -= f * a[col][k]
			}
		}
	}

	r := make([]float64, n)
	for i := range r {
		r[i] = a[i][n] / a[i][i]
	}

	return r
}

func TestPageRankMatchesTheExactSolution(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	for trial := 0; trial < 20; trial++ {
		n := 2 + rng.Intn(40)

		var edges []Edge
		for k := rng.Intn(n * 4); k > 0; k-- {
			edges = append(edges, Edge{int32(rng.Intn(n)), int32(rng.Intn(n))})
		}

		assertScores(t, run(t, n, edges), exact(n, edges, defaults.Damping), 1e-9)
	}
}

func TestPageRankInvariants(t *testing.T) {
	edges := []Edge{{0, 1}, {1, 2}, {2, 0}, {3, 0}, {3, 1}}

	scores := run(t, 5, edges)

	var sum float64
	for _, s := range scores {
		sum += s
	}

	if math.Abs(sum-1) > 1e-9 {
		t.Errorf("scores sum to %v, want 1", sum)
	}

	if math.Abs(scores[4]-scores[3]) > 1e-12 {
		t.Errorf("unlinked nodes 3 and 4 differ: %v vs %v", scores[3], scores[4])
	}

	// Renumbering the nodes renumbers the scores and changes nothing else.
	perm := []int32{4, 2, 0, 3, 1}

	renumbered := make([]Edge, len(edges))
	for i, e := range edges {
		renumbered[i] = Edge{perm[e.From], perm[e.To]}
	}

	permuted := run(t, 5, renumbered)
	for i, s := range scores {
		if math.Abs(permuted[perm[i]]-s) > 1e-12 {
			t.Errorf("node %d: %v after renumbering, %v before", i, permuted[perm[i]], s)
		}
	}
}

func TestNewGraphIgnoresSelfLoopsAndRepeats(t *testing.T) {
	plain := run(t, 3, []Edge{{0, 1}, {1, 2}})
	noisy := run(t, 3, []Edge{{0, 1}, {0, 1}, {0, 0}, {1, 2}, {1, 1}, {1, 2}})

	assertScores(t, noisy, plain, 1e-12)
}

func TestPageRankRejectsBadInput(t *testing.T) {
	if _, err := NewGraph(0, nil); !errors.Is(err, ErrInvalidGraph) {
		t.Errorf("empty graph: %v", err)
	}

	if _, err := NewGraph(2, []Edge{{0, 2}}); !errors.Is(err, ErrInvalidGraph) {
		t.Errorf("edge outside the graph: %v", err)
	}

	g, _ := NewGraph(2, []Edge{{0, 1}})
	for _, opts := range []Options{
		{Damping: 1, Tolerance: 1e-9, MaxIterations: 10},
		{Damping: 0.85, Tolerance: 0, MaxIterations: 10},
		{Damping: 0.85, Tolerance: 1e-9, MaxIterations: 0},
	} {
		if _, err := PageRank(g, opts); !errors.Is(err, ErrInvalidOptions) {
			t.Errorf("%+v: %v", opts, err)
		}
	}
}

func TestPageRankReportsNonConvergence(t *testing.T) {
	g, _ := NewGraph(3, []Edge{{0, 1}, {1, 2}, {2, 0}, {0, 2}})

	res, err := PageRank(g, Options{Damping: 0.85, Tolerance: 1e-15, MaxIterations: 2})
	if err != nil {
		t.Fatal(err)
	}

	if res.Converged || res.Iterations != 2 {
		t.Errorf("converged %v after %d iterations", res.Converged, res.Iterations)
	}
}

func TestCollapse(t *testing.T) {
	// Pages 0,1 on site 0, pages 2,3 on site 1, page 4 on site 2.
	groupOf := []int32{0, 0, 1, 1, 2}
	edges := []Edge{{0, 1}, {0, 2}, {1, 2}, {1, 3}, {2, 3}, {3, 0}, {4, 0}}

	got := Collapse(edges, groupOf)
	want := map[Edge]bool{{0, 1}: true, {1, 0}: true, {2, 0}: true}

	if len(got) != len(want) {
		t.Fatalf("collapsed = %v, want %v", got, want)
	}

	for _, e := range got {
		if !want[e] {
			t.Errorf("unexpected site edge %v", e)
		}
	}
}
