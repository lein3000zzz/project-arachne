//go:build integration

package facts

import (
	"context"
	"errors"
	"slices"
	"testing"
	"web-crawler/internal/database/dbtest"
	"web-crawler/internal/labs"
)

func TestTheLedgerFollowsAPageThroughItsVersions(t *testing.T) {
	pool := dbtest.NewPool(t)
	store := NewPostgresStore(pool)
	ctx := context.Background()

	exec := func(sql string, args ...any) {
		t.Helper()

		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	exec(`WITH u AS (INSERT INTO users (username, password_hash) VALUES ('lein', 'x') RETURNING id),
		l AS (INSERT INTO labs (owner_id, name) SELECT id, 'chiefs' FROM u RETURNING id),
		s AS (INSERT INTO sources (lab_id, kind, url, max_depth, max_links) SELECT id, 'research', 'https://a.dev/', 1, 1 FROM l RETURNING id),
		p AS (INSERT INTO pages (url, host, status) VALUES ('https://a.dev/', 'a.dev', 200), ('https://shared.dev/', 'shared.dev', 200) RETURNING id, url)
		INSERT INTO source_pages (source_id, page_id) SELECT s.id, p.id FROM s, p WHERE p.url = 'https://a.dev/'`)
	exec(`INSERT INTO page_reviews (url, content_hash, decision, model, tool_calls, seconds) VALUES
		('https://a.dev/', 'r1', 'index', 'm', 0, 1), ('https://shared.dev/', 'r1', 'index', 'm', 0, 1)`)

	due, err := store.Due(ctx, 10)
	if err != nil || len(due) != 1 || due[0].URL != "https://a.dev/" || due[0].ReviewHash != "r1" {
		t.Fatalf("due: %+v, %v; the shared base alone is not digested", due, err)
	}

	v1 := []ChunkText{{0, "Henn measures state capacity by distance. Chiefs compete with the state."}, {1, "Unchanged closing text of the page."}}
	if err := store.Version(ctx, "https://a.dev/", "r1", "i1", []int{0, 1}, v1); err != nil {
		t.Fatal(err)
	}

	if todo, _ := store.Undigested(ctx, "https://a.dev/", "i1", []int{0, 1}); !slices.Equal(todo, []int{0, 1}) {
		t.Errorf("undigested %v", todo)
	}

	save := func(index string, n int, text string, facts ...Fact) {
		t.Helper()

		if err := store.SaveChunk(ctx, "https://a.dev/", index, Digested{Ordinal: n, TextHash: TextHash(text), Facts: facts, Model: "small"}); err != nil {
			t.Fatal(err)
		}
	}

	save("i1", 0, v1[0].Text, Fact{Claim: "Henn measures state capacity by distance.", Quote: "Henn measures state capacity by distance."},
		Fact{Claim: "Chiefs compete with the state.", Quote: "Chiefs compete with the state."})
	save("i1", 0, v1[0].Text, Fact{Claim: "twice", Quote: "twice"})
	save("i1", 1, v1[1].Text)

	if due, _ := store.Due(ctx, 10); len(due) != 0 {
		t.Errorf("digested in full, still due: %+v", due)
	}

	// A new version: one sentence gone, one kept but moved to another chunk,
	// the closing chunk the same.
	exec(`UPDATE page_reviews SET content_hash = 'r2', reviewed_at = now() WHERE url = 'https://a.dev/'`)

	if due, _ := store.Due(ctx, 10); len(due) != 1 || due[0].ReviewHash != "r2" {
		t.Fatalf("a new version is due: %+v", due)
	}

	v2 := []ChunkText{{0, "A new opening."}, {1, "Since then, much changed. Chiefs compete with the state."}, {2, "Unchanged closing text of the page."}}
	if err := store.Version(ctx, "https://a.dev/", "r2", "i2", []int{0, 1, 2}, v2); err != nil {
		t.Fatal(err)
	}

	type row struct {
		claim, index string
		ordinal      int
		stale        bool
	}

	rows, _ := pool.Query(ctx, `SELECT claim, index_hash, ordinal, stale_at IS NOT NULL FROM facts ORDER BY claim`)
	var got []row
	for rows.Next() {
		var r row
		_ = rows.Scan(&r.claim, &r.index, &r.ordinal, &r.stale)
		got = append(got, r)
	}

	want := []row{{"Chiefs compete with the state.", "i2", 1, false}, {"Henn measures state capacity by distance.", "i1", 0, true}}
	if !slices.Equal(got, want) {
		t.Errorf("after the new version:\n got %+v\nwant %+v", got, want)
	}

	if todo, _ := store.Undigested(ctx, "https://a.dev/", "i2", []int{0, 1, 2}); !slices.Equal(todo, []int{0, 1}) {
		t.Errorf("the unchanged chunk carried over, the rest to do: %v", todo)
	}

	// Digesting the moved sentence again finds the fact carried over already.
	save("i2", 1, v2[1].Text, Fact{Claim: "Chiefs still compete.", Quote: "Chiefs compete with the state."})

	var live int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM facts WHERE stale_at IS NULL`).Scan(&live)
	if live != 1 {
		t.Errorf("%d live facts, want the carried one alone", live)
	}

	var owner, lab string
	_ = pool.QueryRow(ctx, `SELECT owner_id, id FROM labs`).Scan(&owner, &lab)

	ledger, err := store.Ledger(ctx, owner, lab)
	if err != nil || len(ledger.Facts) != 2 || ledger.Facts[0].Stale || !ledger.Facts[1].Stale || ledger.Pages != 1 || ledger.Digested != 0 || ledger.Chunks != 2 {
		t.Errorf("ledger %+v, %v", ledger, err)
	}

	if _, err := store.Ledger(ctx, lab, lab); !errors.Is(err, labs.ErrNotFound) {
		t.Errorf("someone else's lab: %v", err)
	}

	release, ok, err := store.Lead(ctx)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}

	if _, again, _ := store.Lead(ctx); again {
		t.Error("two digesters at once")
	}

	release()

	if r, ok, _ := store.Lead(ctx); !ok {
		t.Error("the lock was not given back")
	} else {
		r()
	}
}
