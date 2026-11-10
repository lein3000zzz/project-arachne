package labs

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"web-crawler/internal/copies"
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
	// Nil when the page has none: too short, or not sketched yet.
	Sketch copies.Sketch
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
	// Pages whose text is the same or nearly the same as another page's, past
	// the first.
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
	// Not word for word: another version, a mirror with its own header.
	Near bool `json:"near,omitempty"`
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

	var kept []QualityPage

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

		kept = append(kept, p)
	}

	sites := map[string]int{}

	for _, group := range sameText(kept) {
		q.Usable++
		sites[kept[group[0]].Host]++

		if len(group) < 2 {
			continue
		}

		q.Duplicates += len(group) - 1

		d := Duplicate{Title: kept[group[0]].Title}
		for _, i := range group {
			d.URLs = append(d.URLs, kept[i].URL)
			d.Near = d.Near || kept[i].Hash == "" || kept[i].Hash != kept[group[0]].Hash
		}

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

// sameText groups pages that are one text, word for word or nearly, by
// index: each group in page order, the first page the one that counts.
func sameText(pages []QualityPage) [][]int {
	parent := make([]int, len(pages))
	for i := range parent {
		parent[i] = i
	}

	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}

		return parent[i]
	}

	join := func(i, j int) {
		a, b := find(i), find(j)
		if a > b {
			a, b = b, a
		}

		parent[b] = a
	}

	byHash := map[string]int{}
	sketches := make([]copies.Sketch, len(pages))

	for i, p := range pages {
		sketches[i] = p.Sketch

		if p.Hash == "" {
			continue
		}

		if first, ok := byHash[p.Hash]; ok {
			join(first, i)
		} else {
			byHash[p.Hash] = i
		}
	}

	for _, pair := range copies.Candidates(sketches) {
		if copies.Same(sketches[pair[0]], sketches[pair[1]]) {
			join(pair[0], pair[1])
		}
	}

	var groups [][]int

	at := map[int]int{}

	for i := range pages {
		root := find(i)
		if g, ok := at[root]; ok {
			groups[g] = append(groups[g], i)
			continue
		}

		at[root] = len(groups)
		groups = append(groups, []int{i})
	}

	return groups
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
		same := "the same text"
		if slices.ContainsFunc(q.DuplicateGroups, func(d Duplicate) bool { return d.Near }) {
			same = "the same or nearly the same text"
		}

		out = append(out, note{q.Duplicates, fmt.Sprintf("%d %s %s as another page of the lab", q.Duplicates, pluralHas(q.Duplicates), same)})
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
