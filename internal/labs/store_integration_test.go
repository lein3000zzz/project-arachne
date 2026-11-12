//go:build integration

package labs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"web-crawler/internal/copies"
	"web-crawler/internal/database/dbtest"

	"github.com/jackc/pgx/v5/pgxpool"
)

func newStore(t *testing.T) (*PostgresStore, *pgxpool.Pool, string, string) {
	t.Helper()

	pool := dbtest.NewPool(t)
	ctx := context.Background()

	var lein, other string
	if err := pool.QueryRow(ctx, `INSERT INTO users (username, password_hash) VALUES ('lein', 'x') RETURNING id`).Scan(&lein); err != nil {
		t.Fatal(err)
	}

	if err := pool.QueryRow(ctx, `INSERT INTO users (username, password_hash) VALUES ('other', 'x') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}

	return NewPostgresStore(pool), pool, lein, other
}

func exec(t *testing.T, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()

	if _, err := pool.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// crawled pretends a run of the source fetched url with status.
func crawled(t *testing.T, pool *pgxpool.Pool, sourceID, url string, status int) {
	t.Helper()

	run := "run-" + sourceID
	exec(t, pool, `INSERT INTO runs (id, start_url, max_depth, max_links, source_id) VALUES ($1, '', 1, 1, $2) ON CONFLICT DO NOTHING`,
		run, sourceID)
	exec(t, pool, `
		INSERT INTO pages (url, host, status, title, last_run_id, fetched_at) VALUES ($1, 'example.com', $2, 'T', $3, now())
		ON CONFLICT (url) DO NOTHING`, url, status, run)
	exec(t, pool, `INSERT INTO source_pages (source_id, page_id) SELECT $1, id FROM pages WHERE url = $2`, sourceID, url)
}

func TestLabsBelongToTheirOwner(t *testing.T) {
	store, _, lein, other := newStore(t)
	ctx := context.Background()

	lab, err := store.Create(ctx, lein, "Go concurrency", "goroutines and channels", nil)
	if err != nil || lab.Name != "Go concurrency" || lab.Sources != 0 {
		t.Fatalf("create: %+v, %v", lab, err)
	}

	if _, err := store.Create(ctx, lein, "go CONCURRENCY", "", nil); !errors.Is(err, ErrNameTaken) {
		t.Errorf("same name in another case: %v", err)
	}

	if _, err := store.Create(ctx, other, "Go concurrency", "", nil); err != nil {
		t.Errorf("another user may use the name: %v", err)
	}

	for name, call := range map[string]func() error{
		"get":     func() error { _, err := store.Get(ctx, other, lab.ID); return err },
		"delete":  func() error { return store.Delete(ctx, other, lab.ID) },
		"sources": func() error { _, err := store.Sources(ctx, other, lab.ID); return err },
		"pages":   func() error { _, err := store.Pages(ctx, other, lab.ID, 10); return err },
		"urls":    func() error { _, err := store.PageURLs(ctx, other, []string{lab.ID}); return err },
		"add source": func() error {
			_, err := store.AddSource(ctx, other, lab.ID, Source{URL: "https://evil.example/", MaxDepth: 1, MaxLinks: 1})
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s by another user: %v, want not found", name, err)
		}
	}

	list, err := store.List(ctx, lein)
	if err != nil || len(list) != 1 || list[0].ID != lab.ID {
		t.Errorf("list: %+v, %v", list, err)
	}

	if err := store.Delete(ctx, lein, lab.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Get(ctx, lein, lab.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
}

func TestALabIsRenamedByItsOwnerOnly(t *testing.T) {
	store, _, lein, other := newStore(t)
	ctx := context.Background()

	lab, _ := store.Create(ctx, lein, "Zig Programming Language", "drafted", nil)
	_, _ = store.Create(ctx, lein, "Rust", "", nil)

	renamed, err := store.Rename(ctx, lein, lab.ID, "Zig", "comptime and allocators")
	if err != nil || renamed.Name != "Zig" || renamed.Topic != "comptime and allocators" || !renamed.UpdatedAt.After(lab.UpdatedAt) {
		t.Fatalf("%+v, %v", renamed, err)
	}

	if _, err := store.Rename(ctx, lein, lab.ID, "rust", ""); !errors.Is(err, ErrNameTaken) {
		t.Errorf("onto another lab's name: %v", err)
	}

	if _, err := store.Rename(ctx, other, lab.ID, "Mine", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("by another user: %v", err)
	}

	if got, _ := store.Get(ctx, lein, lab.ID); got.Name != "Zig" {
		t.Errorf("after refusals: %+v", got)
	}
}

func TestSourcesAndTheirPages(t *testing.T) {
	store, pool, lein, other := newStore(t)
	ctx := context.Background()

	golab, _ := store.Create(ctx, lein, "go", "", nil)
	rustlab, _ := store.Create(ctx, lein, "rust", "", nil)
	foreign, _ := store.Create(ctx, other, "theirs", "", nil)

	doc, err := store.AddSource(ctx, lein, golab.ID, Source{URL: "https://go.dev/doc/", Scope: "section", MaxDepth: 2, MaxLinks: 50})
	if err != nil || doc.URL != "https://go.dev/doc/" || doc.Scope != "section" || doc.LastRunID != "" {
		t.Fatalf("add: %+v, %v", doc, err)
	}

	if _, err := store.AddSource(ctx, lein, golab.ID, Source{URL: "https://go.dev/doc/", MaxDepth: 1, MaxLinks: 1}); !errors.Is(err, ErrSourceExists) {
		t.Errorf("the same source twice: %v", err)
	}

	blog, _ := store.AddSource(ctx, lein, golab.ID, Source{URL: "https://go.dev/blog/", MaxDepth: 1, MaxLinks: 10})
	book, _ := store.AddSource(ctx, lein, rustlab.ID, Source{URL: "https://doc.rust-lang.org/book/", MaxDepth: 1, MaxLinks: 10})

	crawled(t, pool, doc.ID, "https://go.dev/doc/", 200)
	crawled(t, pool, doc.ID, "https://go.dev/doc/effective_go", 200)
	crawled(t, pool, doc.ID, "https://go.dev/doc/gone", 404)
	crawled(t, pool, blog.ID, "https://go.dev/blog/", 200)
	exec(t, pool, `INSERT INTO source_pages (source_id, page_id) SELECT $1, id FROM pages WHERE url = 'https://go.dev/doc/'`, blog.ID)
	crawled(t, pool, book.ID, "https://doc.rust-lang.org/book/", 200)

	got, err := store.Get(ctx, lein, golab.ID)
	if err != nil || got.Sources != 2 || got.Pages != 3 {
		t.Errorf("go lab: %d sources, %d pages, %v; want 2 and 3 (a page two sources share counts once, a 404 not at all)",
			got.Sources, got.Pages, err)
	}

	sources, err := store.Sources(ctx, lein, golab.ID)
	if err != nil || len(sources) != 2 || sources[0].Pages != 2 || sources[0].LastRunID != "run-"+doc.ID || sources[1].Pages != 2 {
		t.Errorf("sources: %+v, %v", sources, err)
	}

	urls, err := store.PageURLs(ctx, lein, []string{golab.ID, rustlab.ID})
	if err != nil || fmt.Sprint(urls) != "[https://doc.rust-lang.org/book/ https://go.dev/blog/ https://go.dev/doc/ https://go.dev/doc/effective_go]" {
		t.Errorf("urls of both labs: %v, %v", urls, err)
	}

	if _, err := store.PageURLs(ctx, lein, []string{golab.ID, foreign.ID}); !errors.Is(err, ErrNotFound) {
		t.Errorf("urls with another user's lab among them: %v", err)
	}

	exec(t, pool, `INSERT INTO page_reviews (url, content_hash, decision, reasons, summary, model, tool_calls, seconds)
		VALUES ('https://go.dev/blog/', 'h', 'skip', '{navigation_only}', 'A list of posts.', 'm', 1, 2.5)`)

	pages, err := store.Pages(ctx, lein, golab.ID, 10)
	if err != nil || len(pages) != 4 {
		t.Errorf("pages, failed fetches included: %+v, %v", pages, err)
	}

	reviewed := 0
	for _, p := range pages {
		if p.Review != nil {
			reviewed++
			if p.URL != "https://go.dev/blog/" || p.Review.Decision != "skip" || p.Review.Reasons[0] != "navigation_only" || p.Review.Summary != "A list of posts." {
				t.Errorf("review = %+v", p.Review)
			}
		}
	}

	if got, _ := store.Get(ctx, lein, golab.ID); reviewed != 1 || got.SetAside != 1 {
		t.Errorf("%d pages with a review, %d set aside", reviewed, got.SetAside)
	}

	var text strings.Builder
	for i := range 60 {
		fmt.Fprintf(&text, "w%d ", i)
	}

	sketch := copies.SketchOf(copies.SetOf(text.String()))
	exec(t, pool, `INSERT INTO page_reviews (url, content_hash, sketch, decision, model, tool_calls, seconds)
		VALUES ('https://go.dev/doc/', 'h2', $1, 'index', 'm', 1, 1)`, sketch.Bytes())

	quality, err := store.QualityPages(ctx, lein, golab.ID)
	if err != nil || len(quality) != 4 {
		t.Fatalf("quality pages %+v, %v", quality, err)
	}

	for _, p := range quality {
		if want := p.URL == "https://go.dev/doc/"; (p.Sketch != nil) != want || want && copies.Resemblance(p.Sketch, sketch) != 1 {
			t.Errorf("%s: sketch %v", p.URL, p.Sketch)
		}
	}

	// effective_go failed every indexing try; the blog index waits (an older
	// failed version of it was tried again since).
	exec(t, pool, `INSERT INTO index_queue (url, fetched_at, dead, error, attempts) VALUES
		('https://go.dev/doc/effective_go', now(), true, 'embedder down', 6),
		('https://go.dev/blog/', now(), true, 'old failure', 6),
		('https://go.dev/blog/', now(), false, '', 0),
		('https://elsewhere.dev/', now(), true, 'not this lab', 6)`)

	quality, err = store.QualityPages(ctx, lein, golab.ID)
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range quality {
		want := map[string]string{"https://go.dev/doc/effective_go": QueueDead, "https://go.dev/blog/": QueueWaiting}[p.URL]
		if p.Queue != want || (want == QueueDead) != (p.QueueError == "embedder down") {
			t.Errorf("%s: queue %q %q, want %q", p.URL, p.Queue, p.QueueError, want)
		}
	}

	if _, err := store.RetryFailed(ctx, other, golab.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user retrying: %v", err)
	}

	if n, err := store.RetryFailed(ctx, lein, golab.ID); err != nil || n != 1 {
		t.Errorf("retried %d, %v; the lab's dead pages, not another's, nor a version a newer one replaced", n, err)
	}

	var left string
	if err := pool.QueryRow(ctx, `SELECT string_agg(url || ':' || dead, ' ' ORDER BY id) FROM index_queue`).Scan(&left); err != nil ||
		left != "https://go.dev/doc/effective_go:false https://go.dev/blog/:false https://elsewhere.dev/:true" {
		t.Errorf("queue after the retry: %s, %v", left, err)
	}

	if err := store.RemoveSource(ctx, other, golab.ID, doc.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user removing a source: %v", err)
	}

	if err := store.RemoveSource(ctx, lein, golab.ID, doc.ID); err != nil {
		t.Fatal(err)
	}

	if got, _ := store.Get(ctx, lein, golab.ID); got.Sources != 1 || got.Pages != 2 {
		t.Errorf("after removing a source: %d sources, %d pages", got.Sources, got.Pages)
	}

	var pagesLeft int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pages`).Scan(&pagesLeft); err != nil || pagesLeft != 5 {
		t.Errorf("%d pages left in the shared base, want all 5", pagesLeft)
	}
}
