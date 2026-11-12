package answer

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"web-crawler/internal/llm"
)

// conflictModel answers the claim check with "unsupported", the comparison
// with what it is given, and the check of one pair as confirm says - yes to
// every pair when confirm is nil.
type conflictModel struct {
	comparison string
	compared   int
	confirm    func(prompt string) bool
	asked      []string
}

func (m *conflictModel) Model() string { return "checker" }

func (m *conflictModel) Stream(_ context.Context, req llm.Request, _ func(string) error) (llm.Response, error) {
	switch req.System {
	case conflictsPrompt:
		m.compared++
		return llm.Response{Text: m.comparison}, nil
	case confirmPrompt:
		prompt := req.Messages[0].Content
		m.asked = append(m.asked, prompt)

		yes := m.confirm == nil || m.confirm(prompt)

		return llm.Response{Text: fmt.Sprintf(`{"a_is_about": "x", "b_is_about": "x", "same_thing": %v, "contradict": %v}`, yes, yes)}, nil
	}

	return llm.Response{Text: `{"verdict": "unsupported"}`}, nil
}

var benchmarkExcerpts = []string{
	"In our HTTP benchmark Zig served 182,000 requests per second on eight cores, ahead of Rust.",
	"Rust with Actix served 240,000 requests per second, ahead of Zig, on the same eight cores.",
	"Both languages compile through LLVM.",
}

func compare(t *testing.T, comparison string, shown []string, on bool) (Verification, *conflictModel, []Conflict) {
	t.Helper()

	return compareWith(t, &conflictModel{comparison: comparison}, shown, on)
}

func compareWith(t *testing.T, m *conflictModel, shown []string, on bool) (Verification, *conflictModel, []Conflict) {
	t.Helper()

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

const onePair = `{"conflicts": [{"a": 1, "b": 2, "about": "who is ahead", "quote_a": "Zig served 182,000 requests per second", "quote_b": "Rust with Actix served 240,000 requests per second, ahead of Zig"}]}`

// A pair found contradictory across the whole answer is asked about alone,
// both ways round: two quotes about different things are no conflict.
func TestAConflictStandsOnlyWhenThePairIsFoundContradictoryAlone(t *testing.T) {
	v, m, _ := compareWith(t, &conflictModel{comparison: onePair, confirm: func(string) bool { return false }}, benchmarkExcerpts, true)
	if len(v.Conflicts) != 0 || len(m.asked) != 2 {
		t.Errorf("conflicts %+v after %d questions; a pair found not contradictory either way is dropped", v.Conflicts, len(m.asked))
	}

	// Seen one way round only: it stands.
	secondWay := func(prompt string) bool {
		return strings.HasPrefix(prompt[strings.Index(prompt, "Quote A:"):], `Quote A: "Rust`)
	}
	if v, m, _ := compareWith(t, &conflictModel{comparison: onePair, confirm: secondWay}, benchmarkExcerpts, true); len(v.Conflicts) != 1 || len(m.asked) != 2 {
		t.Errorf("conflicts %+v after %d questions", v.Conflicts, len(m.asked))
	}

	_, m, _ = compare(t, onePair, benchmarkExcerpts, true)
	if len(m.asked) != 1 || !strings.Contains(m.asked[0], "Question: zig vs rust") ||
		!strings.Contains(m.asked[0], "Around it: In our HTTP benchmark Zig served 182,000 requests per second on eight cores, ahead of Rust.") {
		t.Errorf("asked %q; once is enough when the first answer is yes, and each quote comes with its context", m.asked)
	}
}

func TestAroundAQuoteIsTheTextNearIt(t *testing.T) {
	excerpt := strings.Repeat("filler words here. ", 40) + "The n-body run: Zig is 1.54 times faster than Rust. " + strings.Repeat("more filler. ", 40)

	got := around(excerpt, "zig is 1.54 times faster ... than Rust")
	if !strings.Contains(got, "The n-body run: Zig is 1.54 times faster than Rust.") || len([]rune(got)) > 2*aroundRunes+40 {
		t.Errorf("around = %q", got)
	}

	if got := around(excerpt, "words the excerpt does not hold"); got != "words the excerpt does not hold" {
		t.Errorf("a quote not found as written: %q", got)
	}
}
