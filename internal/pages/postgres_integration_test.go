//go:build integration

package pages

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"web-crawler/internal/database/dbtest"
	"web-crawler/internal/domain/data"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"
)

func testRepo(t *testing.T) *PagePostgresRepo {
	t.Helper()

	return NewPostgresRepo(zap.NewNop().Sugar(), dbtest.NewPool(t))
}

func page(url string, hyperlinks []string, resources []string, aliases ...string) *data.PageData {
	now := time.Now()

	return &data.PageData{
		URL: url, Status: 200, Title: "T", LastRunID: "run", LastUpdatedAt: now, FoundAt: now,
		Links: append(append([]string{}, hyperlinks...), resources...), PageLinks: hyperlinks, Aliases: aliases,
	}
}

func (repo *PagePostgresRepo) strings(t *testing.T, query string, args ...any) []string {
	t.Helper()

	rows, err := repo.pool.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}

	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}

	sort.Strings(out)

	return out
}

func (repo *PagePostgresRepo) exec(t *testing.T, query string, args ...any) {
	t.Helper()

	if _, err := repo.pool.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func (repo *PagePostgresRepo) save(t *testing.T, p *data.PageData) {
	t.Helper()

	if err := repo.SavePage(p); err != nil {
		t.Fatalf("SavePage(%s): %v", p.URL, err)
	}
}

func assertStrings(t *testing.T, what string, got []string, want ...string) {
	t.Helper()

	sort.Strings(want)

	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

const linksOf = `
	SELECT d.url || ' ' || l.kind FROM links l
	JOIN pages s ON s.id = l.src JOIN pages d ON d.id = l.dst
	WHERE s.url = $1`

func TestSavePageReplacesLinksAndKeepsKinds(t *testing.T) {
	repo := testRepo(t)

	repo.save(t, page("https://example.com/",
		[]string{"https://example.com/a", "https://example.com/b", "https://other.example/x"},
		[]string{"https://example.com/app.js", "https://example.com/a"}))

	assertStrings(t, "links", repo.strings(t, linksOf, "https://example.com/"),
		"https://example.com/a 1", "https://example.com/b 1", "https://other.example/x 1", "https://example.com/app.js 2")

	assertStrings(t, "placeholders", repo.strings(t, `SELECT url || ' ' || host FROM pages WHERE status IS NULL`),
		"https://example.com/a example.com", "https://example.com/b example.com",
		"https://other.example/x other.example", "https://example.com/app.js example.com")

	repo.save(t, page("https://example.com/", []string{"https://example.com/b"}, []string{"https://example.com/a"}))

	assertStrings(t, "links after re-crawl", repo.strings(t, linksOf, "https://example.com/"),
		"https://example.com/b 1", "https://example.com/a 2")

	var foundAt, firstFound time.Time
	if err := repo.pool.QueryRow(context.Background(), `SELECT found_at FROM pages WHERE url = 'https://example.com/a'`).Scan(&foundAt); err != nil {
		t.Fatal(err)
	}

	repo.save(t, page("https://example.com/a", nil, nil))

	if err := repo.pool.QueryRow(context.Background(), `SELECT found_at FROM pages WHERE url = 'https://example.com/a'`).Scan(&firstFound); err != nil {
		t.Fatal(err)
	}

	if !firstFound.Equal(foundAt) {
		t.Errorf("found_at moved from %v to %v when the placeholder was crawled", foundAt, firstFound)
	}
}

func TestSavePageFoldsAliases(t *testing.T) {
	repo := testRepo(t)

	repo.save(t, page("https://example.com/", []string{"http://example.com/doc", "https://example.com/other"}, nil))
	repo.save(t, page("https://example.com/doc/", nil, nil, "http://example.com/doc"))

	assertStrings(t, "links of / after the alias was found", repo.strings(t, linksOf, "https://example.com/"),
		"https://example.com/doc/ 1", "https://example.com/other 1")

	repo.save(t, page("https://example.com/other", []string{"http://example.com/doc"}, nil))

	assertStrings(t, "a link to a known alias", repo.strings(t, linksOf, "https://example.com/other"),
		"https://example.com/doc/ 1")

	repo.save(t, page("https://example.com/docs/", nil, nil, "https://example.com/doc/"))

	assertStrings(t, "chained aliases", repo.strings(t,
		`SELECT a.url || ' -> ' || p.url FROM aliases a JOIN pages p ON p.id = a.target_id`),
		"http://example.com/doc -> https://example.com/docs/", "https://example.com/doc/ -> https://example.com/docs/")

	assertStrings(t, "links of / follow the chain", repo.strings(t, linksOf, "https://example.com/"),
		"https://example.com/docs/ 1", "https://example.com/other 1")

	repo.save(t, page("https://example.com/doc/", nil, nil))

	assertStrings(t, "aliases after the old URL became a page again", repo.strings(t, `SELECT url FROM aliases`),
		"http://example.com/doc")

	assertStrings(t, "pages", repo.strings(t, `SELECT url FROM pages`),
		"https://example.com/", "https://example.com/doc/", "https://example.com/docs/", "https://example.com/other")
}

func TestSavePageRecordsLabMembership(t *testing.T) {
	repo := testRepo(t)

	repo.exec(t, `INSERT INTO users (username, password_hash) VALUES ('lein', 'x')`)
	repo.exec(t, `INSERT INTO labs (owner_id, name) SELECT id, 'go' FROM users`)
	repo.exec(t, `INSERT INTO sources (lab_id, kind, url, max_depth, max_links) SELECT id, 'link', 'https://example.com/', 2, 10 FROM labs`)
	repo.exec(t, `INSERT INTO runs (id, start_url, max_depth, max_links, source_id) SELECT 'lab-run', url, 2, 10, id FROM sources`)
	repo.exec(t, `INSERT INTO runs (id, start_url, max_depth, max_links) VALUES ('plain-run', 'https://example.com/', 2, 10)`)

	members := `SELECT p.url FROM source_pages sp JOIN pages p ON p.id = sp.page_id`

	lab := page("https://example.com/a", []string{"https://example.com/linked"}, nil)
	lab.LastRunID = "lab-run"
	repo.save(t, lab)

	plain := page("https://example.com/b", nil, nil)
	plain.LastRunID = "plain-run"
	repo.save(t, plain)

	unknown := page("https://example.com/c", nil, nil)
	unknown.LastRunID = "make-crawl-run"
	repo.save(t, unknown)

	assertStrings(t, "members: the lab run's page, not its link targets", repo.strings(t, members), "https://example.com/a")

	repo.save(t, lab)
	assertStrings(t, "members after a re-crawl", repo.strings(t, members), "https://example.com/a")

	// The server now says /a is /a/: the page moves, and stays in the lab.
	moved := page("https://example.com/a/", nil, nil, "https://example.com/a")
	moved.LastRunID = "plain-run"
	repo.save(t, moved)

	assertStrings(t, "members after the alias fold", repo.strings(t, members), "https://example.com/a/")

	repo.exec(t, `DELETE FROM labs`)

	assertStrings(t, "members after the lab is deleted", repo.strings(t, members))
	assertStrings(t, "pages outlive the lab", repo.strings(t, `SELECT url FROM pages WHERE status IS NOT NULL`),
		"https://example.com/a/", "https://example.com/b", "https://example.com/c")
}

func TestEnsureExcludedTakesMatchingPagesOutOfLabsOnce(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	repo.exec(t, `INSERT INTO users (username, password_hash) VALUES ('lein', 'x')`)
	repo.exec(t, `INSERT INTO labs (owner_id, name) SELECT id, 'x/sync' FROM users`)
	repo.exec(t, `INSERT INTO sources (lab_id, kind, url, max_depth, max_links) SELECT id, 'link', 'https://pkg.go.dev/golang.org/x/sync', 2, 10 FROM labs`)
	repo.exec(t, `INSERT INTO runs (id, start_url, max_depth, max_links, source_id) SELECT 'lab-run', url, 2, 10, id FROM sources`)

	for _, url := range []string{"https://pkg.go.dev/golang.org/x/sync/errgroup", "https://pkg.go.dev/golang.org/x/sync@v0.1.0"} {
		p := page(url, []string{"https://pkg.go.dev/golang.org/x/sync@v0.2.0"}, nil)
		p.LastRunID = "lab-run"
		repo.save(t, p)
	}

	versioned := func(url string) bool { return strings.Contains(url, "@v") }

	dropped, err := repo.EnsureExcluded(ctx, "rules-1", versioned)
	if err != nil || fmt.Sprint(dropped) != "[https://pkg.go.dev/golang.org/x/sync@v0.1.0]" {
		t.Fatalf("dropped %v, %v: the fetched match only, not the never-fetched placeholder", dropped, err)
	}

	assertStrings(t, "lab members", repo.strings(t, `SELECT p.url FROM source_pages sp JOIN pages p ON p.id = sp.page_id`),
		"https://pkg.go.dev/golang.org/x/sync/errgroup")
	assertStrings(t, "fetched pages stay in the graph", repo.strings(t, `SELECT url FROM pages WHERE fetched_at IS NOT NULL`),
		"https://pkg.go.dev/golang.org/x/sync/errgroup", "https://pkg.go.dev/golang.org/x/sync@v0.1.0")

	if again, err := repo.EnsureExcluded(ctx, "rules-1", versioned); err != nil || len(again) != 0 {
		t.Errorf("the same rules again: %v, %v - want nothing to do", again, err)
	}

	if all, err := repo.EnsureExcluded(ctx, "rules-2", func(string) bool { return true }); err != nil || len(all) != 2 {
		t.Errorf("new rules: %v, %v", all, err)
	}
}

func TestSavePageDrawsNoSelfLoops(t *testing.T) {
	repo := testRepo(t)

	repo.save(t, page("https://example.com/doc/", []string{"https://example.com/doc/", "https://example.com/other"}, nil,
		"https://example.com/doc"))
	repo.save(t, page("https://example.com/doc/", []string{"https://example.com/doc"}, nil))

	assertStrings(t, "links", repo.strings(t, linksOf, "https://example.com/doc/"))
}

func TestSavePageAliasKeepsTheStrongerKind(t *testing.T) {
	repo := testRepo(t)

	repo.save(t, page("https://example.com/", []string{"https://example.com/b/"}, []string{"https://example.com/b"}))
	repo.save(t, page("https://example.com/b/", nil, nil, "https://example.com/b"))

	assertStrings(t, "links", repo.strings(t, linksOf, "https://example.com/"), "https://example.com/b/ 1")
}

func TestConcurrentSavesCreateOneRowPerURL(t *testing.T) {
	repo := testRepo(t)

	shared := make([]string, 30)
	for i := range shared {
		shared[i] = fmt.Sprintf("https://example.com/shared/%02d", i)
	}

	var wg sync.WaitGroup

	for i := 0; i < 16; i++ {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			links := append([]string{}, shared...)
			if i%2 == 1 {
				sort.Sort(sort.Reverse(sort.StringSlice(links)))
			}

			if err := repo.SavePage(page(fmt.Sprintf("https://example.com/p%d", i), links, nil)); err != nil {
				t.Error(err)
			}
		}(i)
	}

	wg.Wait()

	var pages, links int
	if err := repo.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM pages), (SELECT count(*) FROM links)`).Scan(&pages, &links); err != nil {
		t.Fatal(err)
	}

	if pages != 16+30 || links != 16*30 {
		t.Errorf("pages %d, links %d; want %d and %d", pages, links, 16+30, 16*30)
	}
}

func TestEnsureCanonicalMergesSpellings(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	if _, err := repo.EnsureCanonical(ctx); err != nil {
		t.Fatal(err)
	}

	repo.exec(t, `
		INSERT INTO pages (url, host, status, title, fetched_at) VALUES
			('https://example.com/a', 'example.com', 200, 'A', now()),
			('https://EXAMPLE.com/q?x=1&y=2#top', 'example.com', 200, 'new q', now()),
			('https://example.com/q?x=1&y=2', 'example.com', NULL, NULL, NULL),
			('https://&/', '&', NULL, NULL, NULL),
			('https://proxy.golang.org,direct"', 'proxy.golang.org,direct"', 200, 'kept', now())`)
	repo.exec(t, `
		INSERT INTO links (src, dst, kind)
		SELECT s.id, d.id, 1 FROM pages s, pages d
		WHERE (s.url, d.url) IN (('https://example.com/a', 'https://EXAMPLE.com/q?x=1&y=2#top'),
		                         ('https://example.com/a', 'https://example.com/q?x=1&y=2'),
		                         ('https://example.com/a', 'https://&/'),
		                         ('https://EXAMPLE.com/q?x=1&y=2#top', 'https://example.com/a'))`)
	repo.exec(t, `
		INSERT INTO aliases (url, target_id)
		SELECT u, (SELECT id FROM pages WHERE url = 'https://example.com/a')
		FROM unnest(ARRAY['https://Example.com/r', 'http://example.com/a']) AS u`)
	repo.exec(t, `UPDATE meta SET value = '0' WHERE key = 'urlnorm_version'`)

	removed, err := repo.EnsureCanonical(ctx)
	if err != nil {
		t.Fatal(err)
	}

	assertStrings(t, "removed", sortedCopy(removed), "https://EXAMPLE.com/q?x=1&y=2#top")

	assertStrings(t, "pages", repo.strings(t, `SELECT url || ' ' || coalesce(title, '-') FROM pages`),
		"https://example.com/a A", "https://example.com/q?x=1&y=2 new q", `https://proxy.golang.org,direct" kept`)

	assertStrings(t, "links of /a", repo.strings(t, linksOf, "https://example.com/a"), "https://example.com/q?x=1&y=2 1")
	assertStrings(t, "links of q", repo.strings(t, linksOf, "https://example.com/q?x=1&y=2"), "https://example.com/a 1")
	assertStrings(t, "aliases", repo.strings(t, `SELECT url FROM aliases`), "http://example.com/a")

	again, err := repo.EnsureCanonical(ctx)
	if err != nil || len(again) != 0 {
		t.Errorf("EnsureCanonical at the current version = %v, %v; want a no-op", again, err)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)

	return out
}

func TestGraphStoreRoundTrip(t *testing.T) {
	repo := testRepo(t)
	store := NewGraphStore(repo.pool)
	ctx := context.Background()

	repo.save(t, page("https://a.example/", []string{"https://a.example/x", "https://b.example/"}, []string{"https://a.example/app.js"}))
	repo.save(t, page("https://b.example/", []string{"https://a.example/"}, nil))

	graph, err := store.LoadLinkGraph(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(graph.PageIDs) != 4 || len(graph.Hosts) != 2 || len(graph.Links) != 3 {
		t.Fatalf("pages %d hosts %d links %d; want 4, 2 and 3 - the script is not a hyperlink",
			len(graph.PageIDs), len(graph.Hosts), len(graph.Links))
	}

	pageScores := make([]float64, len(graph.PageIDs))
	for i := range pageScores {
		pageScores[i] = float64(i + 1)
	}

	// A page merged away between loading and saving must not fail the run.
	repo.exec(t, `DELETE FROM pages WHERE url = 'https://a.example/app.js'`)

	if err := store.SaveRanks(ctx, RankRun{Pages: 4}, graph.PageIDs, pageScores, graph.Hosts, []float64{0.7, 0.3}); err != nil {
		t.Fatal(err)
	}

	ranks, err := store.LookupRanks(ctx, []string{"https://a.example/", "https://b.example/", "https://nowhere.example/"})
	if err != nil {
		t.Fatal(err)
	}

	if len(ranks) != 2 || ranks["https://a.example/"].Page <= 0 || ranks["https://a.example/"].Site == ranks["https://b.example/"].Site {
		t.Errorf("ranks = %+v", ranks)
	}

	if err := store.SaveRanks(ctx, RankRun{Pages: 3}, graph.PageIDs[:1], []float64{1}, graph.Hosts[:1], []float64{1}); err != nil {
		t.Fatal(err)
	}

	again, err := store.LookupRanks(ctx, []string{"https://a.example/", "https://b.example/"})
	if err != nil || len(again) != 1 {
		t.Errorf("a new run must replace the old scores entirely: %+v, %v", again, err)
	}

	var runs int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM rank_runs`).Scan(&runs); err != nil || runs != 2 {
		t.Errorf("rank_runs = %d, %v", runs, err)
	}
}

func TestPlaceholderTwinsResolve(t *testing.T) {
	repo := testRepo(t)
	store := NewGraphStore(repo.pool)
	ctx := context.Background()

	repo.save(t, page("https://example.com/", []string{
		"https://example.com/learn", "https://example.com/help/", "http://example.com/docs/?v=1", "https://example.com/solo",
	}, nil))
	repo.save(t, page("https://example.com/learn/", nil, nil))
	repo.save(t, page("https://example.com/help", nil, nil))
	repo.save(t, page("https://example.com/docs?v=1", nil, nil))

	twins, err := store.PlaceholderTwins(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, twin := range twins {
		got[twin.Placeholder] = fmt.Sprint(twin.Fetched)
	}

	want := map[string]string{
		"https://example.com/learn":    "[https://example.com/learn/]",
		"https://example.com/help/":    "[https://example.com/help]",
		"http://example.com/docs/?v=1": "[https://example.com/docs?v=1]",
	}

	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("twins = %v, want %v", got, want)
	}

	if err := store.ResolveTwin(ctx, "https://example.com/learn", "https://example.com/learn/"); err != nil {
		t.Fatal(err)
	}

	if err := store.ResolveTwin(ctx, "https://example.com/help/", "https://example.com/help/"); err != nil {
		t.Fatal(err)
	}

	assertStrings(t, "links of / after resolving", repo.strings(t, linksOf, "https://example.com/"),
		"https://example.com/learn/ 1", "https://example.com/help/ 1", "http://example.com/docs/?v=1 1", "https://example.com/solo 1")

	assertStrings(t, "aliases", repo.strings(t, `SELECT url FROM aliases`), "https://example.com/learn")

	again, err := store.PlaceholderTwins(ctx, 10)
	if err != nil || len(again) != 1 || again[0].Placeholder != "http://example.com/docs/?v=1" {
		t.Errorf("after resolving: %+v, %v; checked placeholders must not come back", again, err)
	}

	repo.save(t, page("https://example.com/", []string{"https://example.com/learn"}, nil))

	assertStrings(t, "a later link to the alias", repo.strings(t, linksOf, "https://example.com/"), "https://example.com/learn/ 1")
}

func TestRankLockAndStaleness(t *testing.T) {
	repo := testRepo(t)
	store := NewGraphStore(repo.pool)
	ctx := context.Background()

	stale, err := store.RanksStale(ctx)
	if err != nil || stale {
		t.Fatalf("empty graph: stale=%v, %v", stale, err)
	}

	repo.save(t, page("https://example.com/", nil, nil))

	if stale, _ = store.RanksStale(ctx); !stale {
		t.Error("pages and no ranking yet must count as stale")
	}

	repo.exec(t, `INSERT INTO rank_runs (pages, links, hosts, host_links, iterations, residual) VALUES (1, 0, 1, 0, 1, 0)`)

	if stale, _ = store.RanksStale(ctx); stale {
		t.Error("ranked after the last crawl: not stale")
	}

	repo.exec(t, `INSERT INTO runs (id, start_url, max_depth, max_links, finished_at) VALUES ('r', 'https://example.com/', 1, 1, now() + interval '1 second')`)
	repo.exec(t, `INSERT INTO runs (id, start_url, max_depth, max_links, finished_at, note) VALUES ('d', 'x', 1, 1, now() + interval '1 hour', 'dropped')`)

	if stale, _ = store.RanksStale(ctx); !stale {
		t.Error("a crawl finished after the last ranking: stale")
	}

	inner := false

	acquired, err := store.WithRankLock(ctx, func() error {
		again, err := store.WithRankLock(ctx, func() error { inner = true; return nil })
		if err != nil || again {
			t.Errorf("a second ranker must not get the lock: %v %v", again, err)
		}

		return nil
	})
	if err != nil || !acquired || inner {
		t.Errorf("acquired %v inner %v err %v", acquired, inner, err)
	}

	if again, _ := store.WithRankLock(ctx, func() error { return nil }); !again {
		t.Error("the lock must be released after the first ranker")
	}
}

func TestSavePageKeepsADocumentsReferences(t *testing.T) {
	repo := testRepo(t)

	refsOf := `
		SELECT r.position || ' ' || r.url || ' ' || r.label FROM page_references r
		JOIN pages p ON p.id = r.page_id WHERE p.url = $1`

	doc := page("https://example.com/paper.pdf", nil, nil)
	doc.References = []data.Reference{
		{URL: "https://arxiv.org/abs/1512.03385", Label: "[12] Deep residual learning."},
		{URL: "https://doi.org/10.1145/1", Label: "[13] A DOI."},
	}
	repo.save(t, doc)

	assertStrings(t, "references", repo.strings(t, refsOf, doc.URL),
		"1 https://arxiv.org/abs/1512.03385 [12] Deep residual learning.", "2 https://doi.org/10.1145/1 [13] A DOI.")
	assertStrings(t, "references are no links", repo.strings(t, linksOf, doc.URL))
	assertStrings(t, "nor placeholder pages", repo.strings(t, `SELECT url FROM pages`), doc.URL)

	// A failed read has none to give: the stored ones stay.
	doc.References = nil
	repo.save(t, doc)
	assertStrings(t, "references after a save without", repo.strings(t, refsOf, doc.URL),
		"1 https://arxiv.org/abs/1512.03385 [12] Deep residual learning.", "2 https://doi.org/10.1145/1 [13] A DOI.")

	doc.References = []data.Reference{{URL: "https://doi.org/10.1145/1", Label: "[1] Renumbered."}}
	repo.save(t, doc)
	assertStrings(t, "references after a re-read", repo.strings(t, refsOf, doc.URL), "1 https://doi.org/10.1145/1 [1] Renumbered.")

	doc.References = []data.Reference{}
	repo.save(t, doc)
	assertStrings(t, "references of a document gone", repo.strings(t, refsOf, doc.URL))
}
