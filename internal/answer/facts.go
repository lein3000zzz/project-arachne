package answer

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"web-crawler/internal/facts"
)

// Ledger gives the facts of the pages an answer reads, and digests the
// passages it read that have none yet.
type Ledger interface {
	PageFacts(ctx context.Context, urls, among []string) ([]facts.LabFact, error)
	WantPassages(url string, passages []string)
}

// ReadsFacts has answers read the sentences a page's facts rest on where the
// ledger has them, instead of whole passages: fewer words for a small model
// to read and lose its way in, each one stated by the page, and what other
// pages say of it known beforehand.
func (s *Service) ReadsFacts(ledger Ledger) *Service {
	s.ledger = ledger
	return s
}

// Read says how the sources were read: as facts, or as excerpts where a
// passage had none.
type Read struct {
	Facts    int `json:"facts"`
	Excerpts int `json:"excerpts"`
}

// readFact is a fact an answer read, and the source it was read as.
type readFact struct {
	n int
	facts.LabFact
}

// readFacts swaps each passage of each source for its facts' sentences,
// where it has facts. among is the pages agreement is counted over: the
// labs', or the sources' own.
func (s *Service) readFacts(ctx context.Context, res *Result, among []string, query string) {
	urls := make([]string, len(res.Sources))
	for i, src := range res.Sources {
		urls[i] = src.URL
	}

	if among == nil {
		among = urls
	}

	all, err := s.ledger.PageFacts(ctx, urls, among)
	if err != nil {
		// The excerpts stand: an answer is not lost to the ledger failing.
		return
	}

	byURL := map[string][]facts.LabFact{}
	for _, f := range all {
		byURL[f.URL] = append(byURL[f.URL], f)
	}

	asked := terms(query)
	res.Read = &Read{}

	for i, src := range res.Sources {
		var parts []string

		for _, passage := range append([]string{src.Snippet}, src.Excerpts...) {
			held := inPassage(byURL[src.URL], passage)
			if len(held) == 0 {
				parts = append(parts, focus(passage, s.cfg.SourceRunes, asked))
				res.Read.Excerpts++
				s.ledger.WantPassages(src.URL, []string{passage})

				continue
			}

			quotes := make([]string, len(held))
			for j, f := range held {
				quotes[j] = f.Quote
				res.facts = append(res.facts, readFact{n: src.N, LabFact: f})
			}

			parts = append(parts, strings.Join(quotes, "\n"))
			res.Read.Facts += len(held)
		}

		res.shown[i] = strings.Join(parts, "\n…\n")
	}

	res.prompted = make([]string, len(res.shown))
	for i, src := range res.Sources {
		res.prompted[i] = annotate(res.shown[i], src.N, res.facts)
	}
}

// inPassage is the facts whose sentences the passage holds, in its order.
func inPassage(list []facts.LabFact, passage string) []facts.LabFact {
	text := facts.Normalize(passage)

	var held []facts.LabFact
	at := map[string]int{}

	for _, f := range list {
		if i := strings.Index(text, f.Quote); i >= 0 && f.Quote != "" {
			if _, dup := at[f.Quote]; dup {
				continue
			}

			at[f.Quote] = i
			held = append(held, f)
		}
	}

	slices.SortStableFunc(held, func(a, b facts.LabFact) int { return at[a.Quote] - at[b.Quote] })

	return held
}

// annotate tells the model, after each sentence it reads, which other source
// it read says the same or the opposite, and how many pages of the labs state
// it: agreement is the ledger's to know, not the model's to guess.
func annotate(shown string, n int, read []readFact) string {
	lines := strings.Split(shown, "\n")

	for i, line := range lines {
		for _, f := range read {
			if f.n != n || f.Quote != line {
				continue
			}

			var also, against []string

			for _, g := range read {
				if g.n == n {
					continue
				}

				switch {
				case slices.Contains(f.Same, g.ID):
					also = append(also, fmt.Sprintf("[%d]", g.n))
				case slices.Contains(f.Contested, g.ID):
					against = append(against, fmt.Sprintf("[%d]", g.n))
				}
			}

			var notes []string
			if len(also) > 0 {
				notes = append(notes, "also stated in "+strings.Join(dedupe(also), ""))
			} else if f.Agree > 1 {
				notes = append(notes, fmt.Sprintf("stated by %d pages of the labs", f.Agree))
			}

			if len(against) > 0 {
				notes = append(notes, "contradicted by "+strings.Join(dedupe(against), ""))
			}

			if len(notes) > 0 {
				lines[i] = line + " (" + strings.Join(notes, "; ") + ")"
			}

			break
		}
	}

	return strings.Join(lines, "\n")
}

func dedupe(list []string) []string {
	slices.Sort(list)
	return slices.Compact(list)
}

// knownConflicts is the contradictions between sources the ledger found
// while indexing: no model is asked again about them.
func knownConflicts(res Result) []Conflict {
	out := []Conflict{}

	for _, f := range res.facts {
		for _, g := range res.facts {
			if f.n < g.n && slices.Contains(f.Contested, g.ID) &&
				!slices.ContainsFunc(out, func(c Conflict) bool { return c.A == f.n && c.B == g.n }) {
				out = append(out, Conflict{A: f.n, B: g.n, About: cutRunes(f.About, maxConflictAbout), QuoteA: f.Quote, QuoteB: g.Quote})
			}
		}
	}

	return out
}

// agreement is how many pages state what the claim's quote bears out: the
// most of the read facts of its sources that hold the quote or are held by it.
func agreement(c Claim, read []readFact) int {
	if c.Quote == "" || (c.Verdict != Supported && c.Verdict != Partly) {
		return 0
	}

	quote := facts.Normalize(c.Quote)
	most := 0

	for _, f := range read {
		if slices.Contains(c.Cites, f.n) && (strings.Contains(f.Quote, quote) || strings.Contains(quote, f.Quote)) {
			most = max(most, f.Agree)
		}
	}

	return most
}
