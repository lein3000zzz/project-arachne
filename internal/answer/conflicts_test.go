package answer

import (
	"context"
	"testing"
	"time"
	"web-crawler/internal/llm"
)

// conflictModel answers the claim check with "supported" and the comparison
// with what it is given.
type conflictModel struct {
	comparison string
	compared   int
}

func (m *conflictModel) Model() string { return "checker" }

func (m *conflictModel) Stream(_ context.Context, req llm.Request, _ func(string) error) (llm.Response, error) {
	if req.System == conflictsPrompt {
		m.compared++
		return llm.Response{Text: m.comparison}, nil
	}

	return llm.Response{Text: `{"verdict": "unsupported"}`}, nil
}

var benchmarkExcerpts = []string{
	"In our HTTP benchmark Zig served 182,000 requests per second on eight cores.",
	"Rust with Actix served 240,000 requests per second, ahead of Zig, on the same eight cores.",
	"Both languages compile through LLVM.",
}

func compare(t *testing.T, comparison string, shown []string, on bool) (Verification, *conflictModel, []Conflict) {
	t.Helper()

	m := &conflictModel{comparison: comparison}
	s := (&Service{}).Verifies(m, VerifyConfig{MaxClaims: 8, MaxTokens: 100, Timeout: time.Minute, Conflicts: on})

	var heard []Conflict

	v, err := s.Verify(context.Background(), Result{Answer: "Zig is fast [1].", Sources: make([]Source, len(shown)), shown: shown, question: "zig vs rust"},
		VerifyEvents{Conflicts: func(c []Conflict) error { heard = c; return nil }})
	if err != nil {
		t.Fatal(err)
	}

	return v, m, heard
}

func TestSourcesThatDisagreeAreNamedWithTheirOwnWords(t *testing.T) {
	v, m, heard := compare(t, `{"conflicts": [
		{"a": 2, "b": 1, "about": "requests per second", "quote_a": "Rust with Actix served 240,000 requests per second", "quote_b": "Zig served 182,000 requests per second"},
		{"a": 1, "b": 2, "about": "the same pair again", "quote_a": "Zig served 182,000 requests", "quote_b": "Rust with Actix served 240,000"},
		{"a": 1, "b": 3, "about": "made up", "quote_a": "Zig is slow on every benchmark", "quote_b": "Both languages compile through LLVM"},
		{"a": 3, "b": 3, "about": "itself", "quote_a": "Both languages compile", "quote_b": "through LLVM"},
		{"a": 9, "b": 1, "about": "no such source", "quote_a": "x y z", "quote_b": "Zig served 182,000 requests"}
	]}`, benchmarkExcerpts, true)

	want := Conflict{A: 1, B: 2, About: "requests per second", QuoteA: "Zig served 182,000 requests per second", QuoteB: "Rust with Actix served 240,000 requests per second"}
	if m.compared != 1 || len(v.Conflicts) != 1 || v.Conflicts[0] != want || len(heard) != 1 {
		t.Errorf("conflicts %+v, heard %+v, compared %d", v.Conflicts, heard, m.compared)
	}
}

func TestComparingIsOptionalAndNeedsTwoSources(t *testing.T) {
	if v, m, _ := compare(t, `{"conflicts": []}`, benchmarkExcerpts, false); m.compared != 0 || v.Conflicts != nil {
		t.Errorf("off: compared %d, %+v", m.compared, v.Conflicts)
	}

	if v, m, _ := compare(t, `{"conflicts": []}`, benchmarkExcerpts[:1], true); m.compared != 0 || v.Conflicts == nil || len(v.Conflicts) != 0 {
		t.Errorf("one source: compared %d, %+v", m.compared, v.Conflicts)
	}

	if v, _, _ := compare(t, `not json at all`, benchmarkExcerpts, true); v.Conflicts == nil || len(v.Conflicts) != 0 {
		t.Errorf("a reply that is no use: %+v", v.Conflicts)
	}
}
