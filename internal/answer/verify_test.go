package answer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"web-crawler/internal/llm"
)

// checker replies to each check by the claim it is asked about.
type checker struct {
	replies map[string]string
	asked   []llm.Request
}

func (c *checker) Model() string { return "checker" }

func (c *checker) Stream(_ context.Context, req llm.Request, _ func(string) error) (llm.Response, error) {
	c.asked = append(c.asked, req)

	prompt := req.Messages[0].Content
	for claim, reply := range c.replies {
		if strings.HasSuffix(prompt, "Claim: "+claim) {
			if reply == "fail" {
				return llm.Response{}, errors.New("model unreachable")
			}

			return llm.Response{Text: reply}, nil
		}
	}

	return llm.Response{Text: `{"verdict": "maybe"}`}, nil
}

func verified(t *testing.T, answer string, shown []string, c *checker, maxClaims int) (Verification, []int) {
	t.Helper()

	s := (&Service{}).Verifies(c, VerifyConfig{MaxClaims: maxClaims, MaxTokens: 100, Timeout: time.Minute})
	res := Result{Answer: answer, Sources: make([]Source, len(shown)), shown: shown}

	var events []int

	v, err := s.Verify(context.Background(), res, VerifyEvents{
		Claims: func(parts []Part, claims []Claim) error {
			events = append(events, -len(claims))
			return nil
		},
		Claim: func(i int, _ Claim) error {
			events = append(events, i)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	return v, events
}

func TestClaimsAreCheckedAgainstWhatTheModelRead(t *testing.T) {
	shown := []string{
		"18. Hence the saying: If you know the enemy and know yourself, you need not fear the result of a hundred battles.",
		"We present a residual learning framework to ease the training of networks that are substantially deeper.",
	}
	answer := "Knowing the enemy and yourself means not fearing a hundred battles [1]. " +
		"Residual learning eases training of deeper networks [2]. " +
		"Residual learning was invented in 1990 [2]. " +
		"Sun Tzu wrote it in a cave [1]. " +
		"Deeper networks always win [2]."

	c := &checker{replies: map[string]string{
		"Knowing the enemy and yourself means not fearing a hundred battles.": "```json\n{\"verdict\": \"Supported\", \"quote\": \"If you know the enemy and know yourself, you need not fear\"}\n```",
		"Residual learning eases training of deeper networks.":                `{"verdict": "supported", "quote": "a residual learning framework to ease the training of networks"}`,
		"Residual learning was invented in 1990.":                             `{"verdict": "unsupported", "quote": ""}`,
		"Sun Tzu wrote it in a cave.":                                         `{"verdict": "supported", "quote": "Sun Tzu wrote this in a cave"}`,
		"Deeper networks always win.":                                         "fail",
	}}

	v, events := verified(t, answer, shown, c, 8)

	want := []struct {
		verdict Verdict
		quote   bool
	}{{Supported, true}, {Supported, true}, {Unsupported, false}, {Partly, false}, {Unchecked, false}}

	for i, w := range want {
		if got := v.Claims[i]; got.Verdict != w.verdict || (got.Quote != "") != w.quote {
			t.Errorf("claim %d %q: %s, quote %q, note %q; want %s", i, got.Text, got.Verdict, got.Quote, got.Note, w.verdict)
		}
	}

	if v.Claims[3].Note == "" || v.Claims[4].Note == "" {
		t.Errorf("a quote not in the excerpt and a failed check say why: %+v", v.Claims[3:])
	}

	if len(events) != 6 || events[0] != -5 {
		t.Errorf("events = %v; want the split first, then one per claim", events)
	}

	first := c.asked[0]
	if !first.JSON || !strings.Contains(first.Messages[0].Content, "[1] "+shown[0]) || strings.Contains(first.Messages[0].Content, shown[1]) {
		t.Errorf("a check reads the excerpt the claim cites, and no other:\n%s", first.Messages[0].Content)
	}
}

func TestOnlyTheFirstClaimsAreChecked(t *testing.T) {
	c := &checker{}
	v, _ := verified(t, "First claim is here [1]. Second claim is there [1].", []string{"text"}, c, 1)

	if len(c.asked) != 1 || v.Claims[1].Verdict != Unchecked || v.Claims[1].Note == "" {
		t.Errorf("checked %d, second claim %+v", len(c.asked), v.Claims[1])
	}
}

func TestQuoted(t *testing.T) {
	excerpts := []string{"If you know the enemy and know yourself, you need not fear the result of a hundred battles."}

	for quote, want := range map[string]bool{
		"If you KNOW the enemy, and know yourself":       true,
		"know the enemy … need not fear the result":      true,
		"know the enemy... need not fear":                true,
		"you know the enemies":                           false,
		"need not fear the result of a thousand battles": false,
		"": false,
		"know the enemy … and win every battle without fighting": false,
	} {
		if got := quoted(quote, excerpts); got != want {
			t.Errorf("quoted(%q) = %v, want %v", quote, got, want)
		}
	}
}
