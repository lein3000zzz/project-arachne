//go:build integration

package uploads

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"web-crawler/internal/database/dbtest"
)

func TestUploadsAreTheirOwnersAndForgottenWithTheirLastSource(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()

	var purged []string
	store := NewStore(pool, func(_ context.Context, url string) error {
		purged = append(purged, url)
		return nil
	})

	var lein, other int64
	for name, id := range map[string]*int64{"lein": &lein, "other": &other} {
		if err := pool.QueryRow(ctx, `INSERT INTO users (username, password_hash) VALUES ($1, 'x') RETURNING id`, name).Scan(id); err != nil {
			t.Fatal(err)
		}
	}

	paper, err := store.Save(ctx, lein, "resnet.pdf", "application/pdf", []byte("%PDF-1.5 resnet"))
	if err != nil || !Is(paper) {
		t.Fatalf("save: %q, %v", paper, err)
	}

	if again, _ := store.Save(ctx, lein, "copy of resnet.pdf", "application/pdf", []byte("%PDF-1.5 resnet")); again != paper {
		t.Errorf("the same file again is the same upload: %q, %q", again, paper)
	}

	if theirs, _ := store.Save(ctx, other, "resnet.pdf", "application/pdf", []byte("%PDF-1.5 resnet")); theirs == paper {
		t.Errorf("another user's copy shares the address: %q", theirs)
	}

	id, _ := ID(paper)

	if f, err := store.Owned(ctx, lein, id); err != nil || f.Name != "resnet.pdf" || string(f.Data) != "%PDF-1.5 resnet" {
		t.Errorf("owned: %+v, %v", f, err)
	}

	if _, err := store.Owned(ctx, other, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user reads it: %v", err)
	}

	res, err := NewFetcher(nil, pool).Fetch(paper)
	if err != nil || res.Status != http.StatusOK || string(res.Body) != "%PDF-1.5 resnet" || res.ContentType != "application/pdf" || res.FinalURL != paper ||
		res.Filename != "resnet.pdf" {
		t.Errorf("crawler's fetch: %+v, %v", res, err)
	}

	exec := func(query string, args ...any) {
		t.Helper()

		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	// Held by two of lein's labs, crawled and reviewed.
	exec(`INSERT INTO labs (owner_id, name) VALUES ($1, 'a'), ($1, 'b')`, lein)
	exec(`INSERT INTO sources (lab_id, kind, url, max_depth, max_links) SELECT id, 'upload', $1, 1, 1 FROM labs`, paper)
	exec(`INSERT INTO pages (url, host, status, fetched_at) VALUES ($1, $2, 200, now())`, paper, Host)
	exec(`INSERT INTO page_reviews (url, content_hash, decision, model, tool_calls, seconds) VALUES ($1, 'h', 'index', 'm', 1, 1)`, paper)

	exec(`DELETE FROM sources WHERE lab_id = (SELECT id FROM labs WHERE name = 'a')`)

	if err := store.Forget(ctx, lein, []string{paper}); err != nil || len(purged) != 0 {
		t.Fatalf("forget while lab b holds it: %v, purged %v", err, purged)
	}

	if _, err := store.Owned(ctx, lein, id); err != nil {
		t.Errorf("lab b still holds it: %v", err)
	}

	exec(`DELETE FROM sources`)

	if err := store.Forget(ctx, other, []string{paper}); err != nil || len(purged) != 0 {
		t.Errorf("another user cannot make it forgotten: %v, purged %v", err, purged)
	}

	if err := store.Forget(ctx, lein, []string{paper, "https://example.com/"}); err != nil || !slices.Equal(purged, []string{paper}) {
		t.Fatalf("forget: %v, purged %v", err, purged)
	}

	var left int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM uploads WHERE owner_id = $1) + (SELECT count(*) FROM pages WHERE url = $2)
		+ (SELECT count(*) FROM page_reviews WHERE url = $2)`, lein, paper).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d rows left of the file, its page and its review, %v", left, err)
	}

	if res, err := NewFetcher(nil, pool).Fetch(paper); err != nil || res.Status != http.StatusNotFound {
		t.Errorf("a forgotten file is gone for the crawler: %+v, %v", res, err)
	}
}
