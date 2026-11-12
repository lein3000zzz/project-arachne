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

func TestAgreementAndContestAreReadFromThePairsCompared(t *testing.T) {
	pool := dbtest.NewPool(t)
	store := NewPostgresStore(pool)
	ctx := context.Background()

	var owner, lab string
	if err := pool.QueryRow(ctx, `
		WITH u AS (INSERT INTO users (username, password_hash) VALUES ('lein', 'x') RETURNING id)
		INSERT INTO labs (owner_id, name) SELECT id, 'chiefs' FROM u RETURNING owner_id, id`).Scan(&owner, &lab); err != nil {
		t.Fatal(err)
	}

	for _, u := range []string{"https://a.dev/", "https://b.dev/", "https://c.dev/"} {
		if _, err := pool.Exec(ctx, `
			WITH s AS (INSERT INTO sources (lab_id, kind, url, max_depth, max_links) VALUES ($1, 'research', $2, 1, 1) RETURNING id),
			p AS (INSERT INTO pages (url, host, status) VALUES ($2, 'h', 200) RETURNING id)
			INSERT INTO source_pages (source_id, page_id) SELECT s.id, p.id FROM s, p`, lab, u); err != nil {
			t.Fatal(err)
		}

		_ = store.SaveChunk(ctx, u, "i1", Digested{Ordinal: 0, TextHash: "t", Model: "m", Facts: []Fact{{Claim: "Chiefs compete on " + u, Quote: "q " + u}}})
	}

	claims, err := store.Unembedded(ctx, "e", 10)
	if err != nil || len(claims) != 3 {
		t.Fatalf("unembedded %+v, %v", claims, err)
	}

	for i := range claims {
		claims[i].Vector = []float32{1, float32(i), 0}
	}

	if err := store.SaveEmbeddings(ctx, "e", claims); err != nil {
		t.Fatal(err)
	}

	if more, _ := store.Unembedded(ctx, "e", 10); len(more) != 0 {
		t.Errorf("still unembedded: %+v", more)
	}

	if again, _ := store.Unembedded(ctx, "a new embedder", 10); len(again) != 3 {
		t.Errorf("a new embedder embeds again: %d", len(again))
	}

	labs, err := store.LabsToRelate(ctx, "e")
	if err != nil || len(labs) != 1 || labs[lab] == "" {
		t.Fatalf("labs to relate %v, %v", labs, err)
	}

	got, err := store.LabClaims(ctx, lab, "e")
	if err != nil || len(got) != 3 || len(got[0].Vector) != 3 {
		t.Fatalf("lab claims %+v, %v", got, err)
	}

	a, b, c := got[0].ID, got[1].ID, got[2].ID
	_ = store.SaveLink(ctx, b, a, Same, 0.9, "m")
	_ = store.SaveLink(ctx, a, b, Different, 0.9, "m")
	_ = store.SaveLink(ctx, a, c, Contradict, 0.8, "m")

	if compared, _ := store.Compared(ctx, []string{a, b, c}); len(compared) != 2 || !compared[key(a, b)] {
		t.Errorf("compared %v: the first verdict on a pair stands", compared)
	}

	if err := store.SetLabState(ctx, lab, labs[lab]); err != nil {
		t.Fatal(err)
	}

	if again, _ := store.LabsToRelate(ctx, "e"); len(again) != 0 {
		t.Errorf("related already, due again: %v", again)
	}

	ledger, err := store.Ledger(ctx, owner, lab)
	if err != nil {
		t.Fatal(err)
	}

	byID := map[string]LabFact{}
	for _, f := range ledger.Facts {
		byID[f.ID] = f
	}

	if byID[a].Agree != 2 || byID[b].Agree != 2 || byID[a].Group != byID[b].Group || byID[c].Agree != 1 ||
		!slices.Equal(byID[a].Contested, []string{c}) || !slices.Equal(byID[c].Contested, []string{a}) || ledger.Agreed != 1 || ledger.Contested != 2 {
		t.Errorf("ledger %+v", ledger)
	}

	read, err := store.PageFacts(ctx, []string{"https://a.dev/"}, []string{"https://a.dev/", "https://b.dev/", "https://c.dev/"})
	if err != nil || len(read) != 1 || read[0].Agree != 2 || !slices.Equal(read[0].Same, []string{b}) || !slices.Equal(read[0].Contested, []string{c}) {
		t.Errorf("read for an answer: %+v, %v", read, err)
	}

	// Counted among the pages asked about alone.
	if read, _ := store.PageFacts(ctx, []string{"https://a.dev/"}, []string{"https://a.dev/"}); len(read) != 1 || read[0].Agree != 1 || len(read[0].Contested) != 0 {
		t.Errorf("among a.dev alone: %+v", read)
	}

	// A fact going stale leaves its group and stops contesting.
	_, _ = pool.Exec(ctx, `UPDATE facts SET stale_at = now() WHERE id = $1`, b)

	if l, _ := store.Ledger(ctx, owner, lab); l.Agreed != 0 {
		t.Errorf("a stale fact still agrees: %+v", l)
	}
}
