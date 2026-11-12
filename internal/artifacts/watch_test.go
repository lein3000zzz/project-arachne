package artifacts

import (
	"context"
	"slices"
	"testing"
	"web-crawler/internal/answer"
	"web-crawler/internal/vectorstore"
)

type memChecks struct {
	watched []Watched
	hashes  map[string]string
	checks  map[string]Check
}

func (m *memChecks) ToCheck(context.Context, int) ([]Watched, error) { return m.watched, nil }

func (m *memChecks) ReviewHashes(context.Context, []string) (map[string]string, error) {
	return m.hashes, nil
}

func (m *memChecks) Checks(context.Context, string, int) (map[string]Check, error) {
	out := map[string]Check{}
	for k, v := range m.checks {
		out[k] = v
	}

	return out, nil
}

func (m *memChecks) SaveCheck(_ context.Context, _ string, _ int, c Check) error {
	m.checks[c.URL] = c
	return nil
}

type pageTexts map[string]string

func (p pageTexts) Chunks(_ context.Context, url string) ([]vectorstore.Row, error) {
	if text, ok := p[url]; ok {
		return []vectorstore.Row{{URL: url, Text: text}}, nil
	}

	return nil, nil
}

func TestAClaimIsFlaggedWhenItsPageNoLongerHasItsWords(t *testing.T) {
	claim := func(quote string, cites ...int) answer.Claim {
		return answer.Claim{Text: "x", Cites: cites, Verdict: answer.Supported, Quote: quote}
	}

	w := Watched{ArtifactID: "a", Version: 1, Sources: []Source{{N: 1, URL: "u1"}, {N: 2, URL: "u2"}}, Sections: []Section{{Claims: []answer.Claim{
		claim("deeper plain networks train worse", 1),
		claim("identity shortcuts add no parameters", 1, 2),
		claim("words the matching never found", 1),
		{Text: "unsupported", Cites: []int{1}, Verdict: answer.Unsupported, Quote: "deeper plain networks train worse"},
	}}}}

	store := &memChecks{watched: []Watched{w}, hashes: map[string]string{"u1": "r1", "u2": "r1"}, checks: map[string]Check{}}
	pages := pageTexts{
		"u1": "Deeper plain networks train worse. Identity shortcuts add no parameters.",
		"u2": "Identity shortcuts add no parameters, says the second page.",
	}
	ctx := context.Background()

	if err := CheckRound(ctx, store, pages); err != nil {
		t.Fatal(err)
	}

	if got := store.checks["u1"]; !slices.Equal(got.Held, [][2]int{{0, 0}, {0, 1}}) || len(got.Missing) != 0 || got.ChangedAt != nil {
		t.Fatalf("baseline u1 %+v", got)
	}

	// u1 changes: one claim's words go. The same review again changes nothing.
	pages["u1"] = "Deeper networks are hard to train. Identity shortcuts add no parameters."
	store.hashes["u1"] = "r2"

	_ = CheckRound(ctx, store, pages)
	u1 := store.checks["u1"]

	if !slices.Equal(u1.Missing, [][2]int{{0, 0}}) || u1.ChangedAt == nil || u1.ReviewHash != "r2" || len(store.checks["u2"].Missing) != 0 {
		t.Fatalf("after u1 changed: %+v; u2 %+v", u1, store.checks["u2"])
	}

	when := *u1.ChangedAt
	store.hashes["u1"] = "r3"
	_ = CheckRound(ctx, store, pages)

	if !store.checks["u1"].ChangedAt.Equal(when) {
		t.Error("a later check moved the time the words went")
	}

	// u2 leaves the index: what it held is missing, and it is gone.
	delete(pages, "u2")
	store.hashes["u2"] = ""
	_ = CheckRound(ctx, store, pages)

	if u2 := store.checks["u2"]; !u2.Gone || !slices.Equal(u2.Missing, [][2]int{{0, 1}}) {
		t.Errorf("gone %+v", u2)
	}

	// u1 changes back: nothing is missing any more.
	pages["u1"] = "Deeper plain networks train worse. Identity shortcuts add no parameters."
	store.hashes["u1"] = "r4"
	_ = CheckRound(ctx, store, pages)

	if u1 := store.checks["u1"]; len(u1.Missing) != 0 || u1.ChangedAt != nil {
		t.Errorf("back as it was: %+v", u1)
	}
}

func TestAVersionWithoutSourcesIsCheckedOnce(t *testing.T) {
	store := &memChecks{watched: []Watched{{ArtifactID: "a", Version: 1}}, hashes: map[string]string{}, checks: map[string]Check{}}

	if err := CheckRound(context.Background(), store, pageTexts{}); err != nil {
		t.Fatal(err)
	}

	if _, ok := store.checks[""]; !ok {
		t.Errorf("checks %+v: a version with nothing to check is recorded as checked", store.checks)
	}
}
