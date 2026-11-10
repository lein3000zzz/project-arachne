package labs

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
)

// QualityPage is a page of a lab as the quality report reads it: how it was
// fetched, what its review said, and a hash of its text.
type QualityPage struct {
	URL    string
	Host   string
	Title  string
	Status int
	// Empty when the page was never reviewed.
	Decision   string
	Reasons    []string
	Unreviewed bool
	Hash       string
}

// Quality is how good a lab's material is, before an answer leans on it:
// what share of its pages a model can read with profit, and what the rest is.
type Quality struct {
	Pages int `json:"pages"`
	// Fetched, kept on review, and no copy of another page.
	Usable int `json:"usable"`
	// The usable share: good from 0.75, mixed from 0.45, poor below.
	Share   float64 `json:"share"`
	Verdict string  `json:"verdict"`

	NotFetched int            `json:"not_fetched"`
	SetAside   int            `json:"set_aside"`
	Reasons    map[string]int `json:"reasons"`
	Unreviewed int            `json:"unreviewed"`
	// Pages whose text is the same as another page's, past the first.
	Duplicates      int         `json:"duplicates"`
	DuplicateGroups []Duplicate `json:"duplicate_groups"`

	// Sites the usable pages come from, and how much of it the largest gives.
	Sites        int     `json:"sites"`
	TopSite      string  `json:"top_site,omitempty"`
	TopSiteShare float64 `json:"top_site_share"`

	// What stands out, in words: the largest losses first, then caveats.
	Notes []string `json:"notes"`
}

type Duplicate struct {
	Title string   `json:"title"`
	URLs  []string `json:"urls"`
}

const (
	goodShare  = 0.75
	mixedShare = 0.45
	// One site giving this much of the usable pages is worth saying: the lab
	// sees the topic through it.
	dominantSite    = 0.5
	duplicateGroups = 10
)

var reasonWords = map[string]string{
	"navigation_only": "menus only", "too_thin": "too thin", "spam": "spam", "error_page": "error pages",
	"machine_junk": "machine junk", "prompt_injection": "aimed at AI models",
}

// QualityOf reports on a lab's pages.
func QualityOf(pages []QualityPage) Quality {
	q := Quality{Pages: len(pages), Reasons: map[string]int{}, DuplicateGroups: []Duplicate{}, Notes: []string{}}

	byHash := map[string][]QualityPage{}
	sites := map[string]int{}

	for _, p := range pages {
		switch {
		case p.Status < 200 || p.Status > 299:
			q.NotFetched++
			continue
		case p.Decision == "skip":
			q.SetAside++
			for _, r := range p.Reasons {
				q.Reasons[r]++
			}

			continue
		case p.Decision == "" || p.Unreviewed:
			q.Unreviewed++
		}

		if p.Hash != "" {
			byHash[p.Hash] = append(byHash[p.Hash], p)
			if len(byHash[p.Hash]) > 1 {
				q.Duplicates++
				continue
			}
		}

		q.Usable++
		sites[p.Host]++
	}

	for _, group := range byHash {
		if len(group) < 2 {
			continue
		}

		d := Duplicate{Title: group[0].Title}
		for _, p := range group {
			d.URLs = append(d.URLs, p.URL)
		}

		slices.Sort(d.URLs)
		q.DuplicateGroups = append(q.DuplicateGroups, d)
	}

	slices.SortFunc(q.DuplicateGroups, func(a, b Duplicate) int {
		return cmp.Or(cmp.Compare(len(b.URLs), len(a.URLs)), cmp.Compare(a.URLs[0], b.URLs[0]))
	})
	q.DuplicateGroups = q.DuplicateGroups[:min(len(q.DuplicateGroups), duplicateGroups)]

	q.Sites = len(sites)
	for site, n := range sites {
		if n > sites[q.TopSite] || (n == sites[q.TopSite] && site < q.TopSite) {
			q.TopSite = site
		}
	}

	if q.Usable > 0 {
		q.TopSiteShare = float64(sites[q.TopSite]) / float64(q.Usable)
	}

	if q.Pages > 0 {
		q.Share = float64(q.Usable) / float64(q.Pages)
	}

	switch {
	case q.Pages == 0:
		q.Verdict = "empty"
	case q.Share >= goodShare:
		q.Verdict = "good"
	case q.Share >= mixedShare:
		q.Verdict = "mixed"
	default:
		q.Verdict = "poor"
	}

	q.Notes = notes(q)

	return q
}

func notes(q Quality) []string {
	type note struct {
		n    int
		text string
	}

	var out []note

	if q.SetAside > 0 {
		var why []string
		for r, n := range q.Reasons {
			why = append(why, fmt.Sprintf("%d %s", n, cmp.Or(reasonWords[r], r)))
		}

		slices.Sort(why)
		text := fmt.Sprintf("%d %s set aside on review", q.SetAside, pagesWord(q.SetAside))
		if len(why) > 0 {
			text += ": " + strings.Join(why, ", ")
		}

		out = append(out, note{q.SetAside, text})
	}

	if q.NotFetched > 0 {
		out = append(out, note{q.NotFetched, fmt.Sprintf("%d %s could not be fetched", q.NotFetched, pagesWord(q.NotFetched))})
	}

	if q.Duplicates > 0 {
		out = append(out, note{q.Duplicates, fmt.Sprintf("%d %s the same text as another page of the lab", q.Duplicates, pluralHas(q.Duplicates))})
	}

	if q.Unreviewed > 0 {
		out = append(out, note{q.Unreviewed, fmt.Sprintf("%d %s indexed without a review", q.Unreviewed, pagesWord(q.Unreviewed))})
	}

	if q.Usable >= 4 && q.TopSiteShare >= dominantSite {
		// A caveat on balance, not material lost: after the losses.
		out = append(out, note{0, fmt.Sprintf("%s gives %.0f%% of the usable pages: the lab sees the topic mostly through one site",
			q.TopSite, 100*q.TopSiteShare)})
	}

	slices.SortStableFunc(out, func(a, b note) int { return cmp.Compare(b.n, a.n) })

	texts := make([]string, len(out))
	for i, n := range out {
		texts[i] = n.text
	}

	return texts
}

func pagesWord(n int) string {
	if n == 1 {
		return "page"
	}

	return "pages"
}

func pluralHas(n int) string {
	if n == 1 {
		return "page has"
	}

	return "pages have"
}

// Quality reports on how good the lab's material is.
func (s *Service) Quality(ctx context.Context, ownerID, labID int64) (Quality, error) {
	pages, err := s.store.QualityPages(ctx, ownerID, labID)
	if err != nil {
		return Quality{}, err
	}

	return QualityOf(pages), nil
}
