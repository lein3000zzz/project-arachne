package answer

import (
	"context"
	"strings"
	"testing"
	"time"
	"web-crawler/internal/facts"
	"web-crawler/internal/retrieval"
)

type fakeLedger struct {
	facts  []facts.LabFact
	among  []string
	wanted []string
}

func (f *fakeLedger) PageFacts(_ context.Context, urls, among []string) ([]facts.LabFact, error) {
	f.among = among

	var out []facts.LabFact
	for _, x := range f.facts {
		for _, u := range urls {
			if x.URL == u {
				out = append(out, x)
			}
		}
	}

	return out, nil
}

func (f *fakeLedger) WantPassages(url string, _ []string) { f.wanted = append(f.wanted, url) }

func TestAnAnswerReadsFactsWhereTheLedgerHasThem(t *testing.T) {
	search := &fakeSearch{results: []retrieval.Result{
		{URL: "https://a.dev/", Title: "A", Snippet: "Chiefs compete with the state.  Some filler about the weather here. Henn measures capacity by distance."},
		{URL: "https://b.dev/", Title: "B", Snippet: "Henn measures capacity by roads. Other text of b."},
		{URL: "https://c.dev/", Title: "C", Snippet: "A page not digested yet, read as it is."},
	}}
	ledger := &fakeLedger{facts: []facts.LabFact{
		{ID: "a2", URL: "https://a.dev/", Quote: "Henn measures capacity by distance.", About: "capacity", Agree: 1, Contested: []string{"b1"}},
		{ID: "a1", URL: "https://a.dev/", Quote: "Chiefs compete with the state.", Agree: 3, Same: []string{"x"}},
		{ID: "b1", URL: "https://b.dev/", Quote: "Henn measures capacity by roads.", Agree: 1, Contested: []string{"a2"}},
		{ID: "z", URL: "https://a.dev/", Quote: "A sentence of another passage.", Agree: 1},
	}}
	chat := &fakeChat{reply: "Chiefs compete with the state [1]."}

	s, _ := New(search, chat, Config{Sources: 3, MaxTokens: 100})
	s.ReadsFacts(ledger)

	labs := []string{"https://a.dev/", "https://b.dev/", "https://c.dev/", "https://x.dev/"}

	res, err := s.Answer(context.Background(), Request{Query: "how do chiefs and the state relate", LabURLs: labs, LabOnly: true}, Events{})
	if err != nil {
		t.Fatal(err)
	}

	if got := res.shown[0]; got != "Chiefs compete with the state.\nHenn measures capacity by distance." {
		t.Errorf("source 1 read as %q: its facts' sentences, in the page's order, the filler left out", got)
	}

	if !strings.Contains(res.shown[2], "not digested yet") || len(ledger.wanted) != 1 || ledger.wanted[0] != "https://c.dev/" {
		t.Errorf("a passage without facts is read as an excerpt and asked for: %q, %v", res.shown[2], ledger.wanted)
	}

	if res.Read == nil || res.Read.Facts != 3 || res.Read.Excerpts != 1 || len(ledger.among) != 4 {
		t.Errorf("read %+v, among %v", res.Read, ledger.among)
	}

	asked := chat.got.Messages[0].Content
	if !strings.Contains(asked, "Chiefs compete with the state. (stated by 3 pages of the labs)") ||
		!strings.Contains(asked, "Henn measures capacity by distance. (contradicted by [2])") ||
		!strings.Contains(asked, "Henn measures capacity by roads. (contradicted by [1])") {
		t.Errorf("the model was not told what the ledger knows:\n%s", asked)
	}

	if strings.Contains(strings.Join(res.shown, "\n"), "stated by") {
		t.Error("the notes went into the text claims are checked against")
	}

	c := &checker{replies: map[string]string{"Chiefs compete with the state.": `{"verdict": "supported", "quote": "Chiefs compete with the state."}`}}
	s.Verifies(c, VerifyConfig{MaxClaims: 5, MaxTokens: 100, Timeout: time.Minute})

	v, err := s.Verify(context.Background(), res, VerifyEvents{})
	if err != nil {
		t.Fatal(err)
	}

	if len(v.Claims) != 1 || v.Claims[0].Verdict != Supported || v.Claims[0].Agree != 3 {
		t.Errorf("claims %+v: a supported claim carries how many pages state it", v.Claims)
	}

	if len(v.Conflicts) != 1 || v.Conflicts[0].A != 1 || v.Conflicts[0].B != 2 || v.Conflicts[0].QuoteB != "Henn measures capacity by roads." {
		t.Errorf("conflicts %+v: one the ledger knows is shown without asking", v.Conflicts)
	}
}

func TestWithoutTheLedgerAnswersReadExcerptsAsBefore(t *testing.T) {
	search := &fakeSearch{results: []retrieval.Result{{URL: "https://a.dev/", Title: "A", Snippet: "Chiefs compete with the state. Filler."}}}
	chat := &fakeChat{reply: "Yes [1]."}
	s, _ := New(search, chat, Config{Sources: 2, MaxTokens: 100})

	res, err := s.Answer(context.Background(), Request{Query: "chiefs"}, Events{})
	if err != nil || res.Read != nil || res.prompted != nil || !strings.Contains(res.shown[0], "Filler") {
		t.Errorf("%+v, %v", res, err)
	}
}
