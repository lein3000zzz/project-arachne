package labs

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"web-crawler/internal/copies"
)

func TestQualitySaysWhatALabsPagesAre(t *testing.T) {
	ok := func(url, host, hash string) QualityPage {
		return QualityPage{URL: url, Host: host, Title: url, Status: 200, Decision: "index", Hash: hash}
	}

	q := QualityOf([]QualityPage{
		ok("https://a.dev/1", "a.dev", "h1"),
		ok("https://a.dev/1?tab=x", "a.dev", "h1"),
		ok("https://a.dev/1/v2", "a.dev", "h1"),
		ok("https://a.dev/2", "a.dev", "h2"),
		ok("https://a.dev/3", "a.dev", "h3"),
		ok("https://b.dev/1", "b.dev", "h4"),
		{URL: "https://c.dev/menu", Host: "c.dev", Status: 200, Decision: "skip", Reasons: []string{"navigation_only"}},
		{URL: "https://c.dev/thin", Host: "c.dev", Status: 200, Decision: "skip", Reasons: []string{"too_thin", "navigation_only"}},
		{URL: "https://medium.com/x", Host: "medium.com", Status: 403},
		{URL: "https://d.dev/new", Host: "d.dev", Status: 200, Hash: "h5"},
	})

	if q.Pages != 10 || q.Usable != 5 || q.Duplicates != 2 || q.SetAside != 2 || q.NotFetched != 1 || q.Unreviewed != 1 || q.Verdict != "mixed" {
		t.Errorf("quality %+v", q)
	}

	if q.Reasons["navigation_only"] != 2 || q.Reasons["too_thin"] != 1 {
		t.Errorf("reasons %v", q.Reasons)
	}

	if len(q.DuplicateGroups) != 1 || len(q.DuplicateGroups[0].URLs) != 3 {
		t.Errorf("duplicate groups %+v", q.DuplicateGroups)
	}

	if q.TopSite != "a.dev" || q.Sites != 3 || q.TopSiteShare != 0.6 {
		t.Errorf("sites: %d, top %s %.2f", q.Sites, q.TopSite, q.TopSiteShare)
	}

	joined := strings.Join(q.Notes, " | ")
	for _, want := range []string{"2 pages set aside on review: 1 too thin, 2 menus only", "2 pages have the same text", "a.dev gives 60%", "1 page could not be fetched"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes %q lack %q", joined, want)
		}
	}

	if !strings.HasPrefix(q.Notes[0], "2 ") || !slices.ContainsFunc(q.Notes, func(n string) bool { return strings.Contains(n, "without a review") }) {
		t.Errorf("notes %q: the largest first", q.Notes)
	}

	if e := QualityOf(nil); e.Verdict != "empty" || e.Notes == nil || e.DuplicateGroups == nil {
		t.Errorf("an empty lab: %+v", e)
	}

	good := QualityOf([]QualityPage{ok("https://a.dev/1", "a.dev", "1"), ok("https://b.dev/1", "b.dev", "2"), ok("https://c.dev/1", "c.dev", "3")})
	if good.Verdict != "good" || len(good.Notes) != 0 {
		t.Errorf("a clean lab: %+v", good)
	}
}

// Two versions of one docs page, or a mirror with its own header, are one
// text: the quality report counts the first and names the rest.
func TestNearCopiesAreCopiesToo(t *testing.T) {
	words := func(seed, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("s%dw%d", seed, i)
		}

		return out
	}

	body := words(1, 400)
	v2 := slices.Clone(body)
	v2[200] = "changed"

	sketch := func(w []string) copies.Sketch { return copies.SketchOf(copies.SetOf(strings.Join(w, " "))) }
	page := func(url string, w []string) QualityPage {
		return QualityPage{URL: url, Host: "a.dev", Title: url, Status: 200, Decision: "index", Hash: url, Sketch: sketch(w)}
	}

	q := QualityOf([]QualityPage{
		page("https://a.dev/v1/intro", body),
		page("https://a.dev/v2/intro", v2),
		page("https://a.dev/other", words(2, 400)),
		{URL: "https://a.dev/old", Host: "a.dev", Status: 200, Decision: "index", Hash: "old"},
	})

	if q.Usable != 3 || q.Duplicates != 1 || len(q.DuplicateGroups) != 1 || !q.DuplicateGroups[0].Near ||
		!slices.Equal(q.DuplicateGroups[0].URLs, []string{"https://a.dev/v1/intro", "https://a.dev/v2/intro"}) {
		t.Errorf("quality %+v", q)
	}

	if !slices.ContainsFunc(q.Notes, func(n string) bool { return strings.Contains(n, "1 page has the same or nearly the same text") }) {
		t.Errorf("notes %q", q.Notes)
	}
}

// A page fetched but not indexed is not usable, whatever else it is: still
// waiting, or failed every try - then named with why.
func TestPagesNotIndexedAreNotUsable(t *testing.T) {
	q := QualityOf([]QualityPage{
		{URL: "https://a.dev/1", Host: "a.dev", Status: 200, Decision: "index", Hash: "1"},
		{URL: "https://a.dev/new", Host: "a.dev", Status: 200, Queue: QueueWaiting},
		{URL: "https://a.dev/refreshing", Host: "a.dev", Status: 200, Decision: "index", Hash: "2", Queue: QueueWaiting},
		{URL: "https://a.dev/broken", Host: "a.dev", Status: 200, Queue: QueueDead, QueueError: "embedding 4 chunks: connection refused"},
		{URL: "https://a.dev/gone", Host: "a.dev", Status: 404, Queue: QueueDead},
	})

	if q.Usable != 2 || q.Waiting != 1 || q.NotIndexed != 1 || q.NotFetched != 1 || q.Unreviewed != 0 {
		t.Errorf("quality %+v; a page waiting as a newer version stays usable", q)
	}

	if len(q.NotIndexedPages) != 1 || q.NotIndexedPages[0] != (FailedPage{URL: "https://a.dev/broken", Error: "embedding 4 chunks: connection refused"}) {
		t.Errorf("failed pages %+v", q.NotIndexedPages)
	}

	joined := strings.Join(q.Notes, " | ")
	for _, want := range []string{"1 page fetched but could not be indexed", "1 page still waiting to be indexed"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes %q lack %q", joined, want)
		}
	}
}
