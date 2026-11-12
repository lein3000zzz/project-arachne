package facts

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"web-crawler/internal/copies"
	"web-crawler/internal/llm"

	"go.uber.org/zap"
)

type memRelate struct {
	claims []Claim
	links  map[[2]string]string
	states map[string]string
}

func (m *memRelate) Unembedded(_ context.Context, _ string, limit int) ([]Claim, error) {
	var out []Claim
	for _, c := range m.claims {
		if c.Vector == nil && len(out) < limit {
			out = append(out, Claim{ID: c.ID, URL: c.URL, Text: c.Text})
		}
	}

	return out, nil
}

func (m *memRelate) SaveEmbeddings(_ context.Context, _ string, claims []Claim) error {
	for _, c := range claims {
		for i := range m.claims {
			if m.claims[i].ID == c.ID {
				m.claims[i].Vector = c.Vector
			}
		}
	}

	return nil
}

func (m *memRelate) LabsToRelate(context.Context, string) (map[string]string, error) {
	if m.states["lab"] == "now" {
		return map[string]string{}, nil
	}

	return map[string]string{"lab": "now"}, nil
}

func (m *memRelate) LabClaims(context.Context, string, string) ([]Claim, error) { return m.claims, nil }

func (m *memRelate) Compared(context.Context, []string) (map[[2]string]bool, error) {
	out := map[[2]string]bool{}
	for k := range m.links {
		out[k] = true
	}

	return out, nil
}

func (m *memRelate) SaveLink(_ context.Context, a, b, relation string, _ float64, _ string) error {
	m.links[key(a, b)] = relation
	return nil
}

func (m *memRelate) SetLabState(_ context.Context, lab, signature string) error {
	m.states[lab] = signature
	return nil
}

// Each claim's vector is set by the first word of its text, so the test says
// which claims are close: "chiefs …" and "chiefs …" are, "banana …" is not.
type wordEmbedder struct{ calls int }

func (e *wordEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls++
	out := make([][]float32, len(texts))

	for i, t := range texts {
		t = strings.TrimPrefix(t, embedInstruction)
		v := []float32{0, 0, 0}
		switch strings.Fields(t)[0] {
		case "chiefs":
			v = []float32{1, 0.1, 0}
		case "banana":
			v = []float32{0, 1, 0}
		default:
			v = []float32{0, 0, 1}
		}

		out[i] = v
	}

	return out, nil
}

func (*wordEmbedder) Model() string { return "words" }

type judge struct {
	asked []string
	err   error
}

func (j *judge) Stream(_ context.Context, req llm.Request, _ func(string) error) (llm.Response, error) {
	if j.err != nil {
		return llm.Response{}, j.err
	}

	q := req.Messages[0].Content
	j.asked = append(j.asked, q)

	switch {
	case strings.Contains(q, "A: chiefs compete") && strings.Contains(q, "B: chiefs rival"):
		return llm.Response{Text: `{"relation":"same"}`}, nil
	case strings.Contains(q, "B: chiefs compete") && strings.Contains(q, "A: chiefs rival"):
		return llm.Response{Text: `sure: {"relation": "Same"}`}, nil
	case strings.Contains(q, "chiefs never"):
		// One way round only: a contradiction seen once is a contradiction.
		if strings.HasPrefix(q, "A: chiefs never") {
			return llm.Response{Text: `{"relation":"contradict"}`}, nil
		}

		return llm.Response{Text: `{"relation":"different"}`}, nil
	}

	return llm.Response{Text: `I cannot tell.`}, nil
}

func (*judge) Model() string { return "small" }

type noSketches map[string]copies.Sketch

func (s noSketches) Sketches(context.Context, []string) (map[string]copies.Sketch, error) {
	return s, nil
}

func TestFactsOfDifferentPagesAreComparedOnceAndBothWays(t *testing.T) {
	store := &memRelate{links: map[[2]string]string{}, states: map[string]string{}, claims: []Claim{
		{ID: "1", URL: "a", Text: "chiefs compete with the state"},
		{ID: "2", URL: "b", Text: "chiefs rival the state"},
		{ID: "3", URL: "c", Text: "chiefs never compete with the state"},
		{ID: "4", URL: "a", Text: "chiefs again on page a"},
		{ID: "5", URL: "d", Text: "banana prices rose"},
	}}
	model := &judge{}
	r := NewRelater(zap.NewNop().Sugar(), store, &wordEmbedder{}, model, noSketches{}, RelateConfig{Shortlist: 0.7, EmbedBatch: 2})
	ctx := context.Background()

	if err := r.Embed(ctx); err != nil {
		t.Fatal(err)
	}

	for _, c := range store.claims {
		if len(c.Vector) != 3 {
			t.Fatalf("not embedded: %+v", c)
		}
	}

	if err := r.Relate(ctx); err != nil {
		t.Fatal(err)
	}

	want := map[[2]string]string{{"1", "2"}: Same, {"1", "3"}: Contradict, {"2", "3"}: Contradict, {"2", "4"}: Different, {"3", "4"}: Contradict}
	if len(store.links) != len(want) {
		t.Errorf("links %v, want %v: facts of one page are never compared, nor ones far apart", store.links, want)
	}

	for k, v := range want {
		if store.links[k] != v {
			t.Errorf("%v: %q, want %q", k, store.links[k], v)
		}
	}

	if store.states["lab"] != "now" || len(model.asked) != 2*len(want) {
		t.Errorf("state %v, %d asked", store.states, len(model.asked))
	}

	// Done: nothing is asked again.
	model.asked = nil
	if err := r.Relate(ctx); err != nil || len(model.asked) != 0 {
		t.Errorf("asked again: %v %v", model.asked, err)
	}
}

func TestComparingStopsAtTheBudgetAndWhenTheModelIsDown(t *testing.T) {
	claims := []Claim{{ID: "1", URL: "a", Text: "chiefs compete"}, {ID: "2", URL: "b", Text: "chiefs rival"}, {ID: "3", URL: "c", Text: "chiefs never"}}
	store := &memRelate{links: map[[2]string]string{}, states: map[string]string{}, claims: slices.Clone(claims)}
	r := NewRelater(zap.NewNop().Sugar(), store, &wordEmbedder{}, &judge{}, noSketches{}, RelateConfig{Shortlist: 0.7, PairsPerSweep: 2})
	ctx := context.Background()

	_ = r.Embed(ctx)
	if err := r.Relate(ctx); err != nil || len(store.links) != 2 || store.states["lab"] != "" {
		t.Errorf("first sweep: %v, %v, %v", err, store.links, store.states)
	}

	if err := r.Relate(ctx); err != nil || len(store.links) != 3 || store.states["lab"] != "now" {
		t.Errorf("second sweep: %v, %v, %v", err, store.links, store.states)
	}

	down := &memRelate{links: map[[2]string]string{}, states: map[string]string{}, claims: slices.Clone(claims)}
	r = NewRelater(zap.NewNop().Sugar(), down, &wordEmbedder{}, &judge{err: errors.New("connection refused")}, noSketches{}, RelateConfig{Shortlist: 0.7})
	_ = r.Embed(ctx)

	if err := r.Relate(ctx); err == nil || len(down.links) != 0 || down.states["lab"] != "" {
		t.Errorf("model down: %v, %v, %v", err, down.links, down.states)
	}
}

func TestACopyOfAPageDoesNotAgreeWithIt(t *testing.T) {
	sketch := make(copies.Sketch, 64)
	store := &memRelate{links: map[[2]string]string{}, states: map[string]string{}, claims: []Claim{
		{ID: "1", URL: "a", Text: "chiefs compete with the state"},
		{ID: "2", URL: "mirror", Text: "chiefs rival the state"},
	}}
	r := NewRelater(zap.NewNop().Sugar(), store, &wordEmbedder{}, &judge{}, noSketches{"a": sketch, "mirror": sketch}, RelateConfig{Shortlist: 0.7})

	_ = r.Embed(context.Background())
	if err := r.Relate(context.Background()); err != nil || len(store.links) != 0 {
		t.Errorf("a page and its copy compared: %v %v", store.links, err)
	}
}
