//go:build integration

package artifacts

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
	"web-crawler/internal/answer"
	"web-crawler/internal/database/dbtest"
	"web-crawler/internal/labs"
)

func TestArtifactsAreVersionedAndTheirOwners(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()
	store := NewPostgresStore(pool)

	var lein, other, lab string
	for name, id := range map[string]*string{"lein": &lein, "other": &other} {
		if err := pool.QueryRow(ctx, `INSERT INTO users (username, password_hash) VALUES ($1, 'x') RETURNING id`, name).Scan(id); err != nil {
			t.Fatal(err)
		}
	}

	if err := pool.QueryRow(ctx, `INSERT INTO labs (owner_id, name) VALUES ($1, 'resnets') RETURNING id`, lein).Scan(&lab); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Create(ctx, other, lab, KindReport, "theirs", ""); !errors.Is(err, labs.ErrNotFound) {
		t.Errorf("into another user's lab: %v", err)
	}

	id, err := store.Create(ctx, lein, lab, KindReport, "Residual networks", "a report")
	if err != nil {
		t.Fatal(err)
	}

	outline := Outline{Title: "Residual networks", Sections: []Planned{{Heading: "Blocks", Focus: "what a block computes"}}}
	running := Version{Status: StatusRunning, Outline: outline, Model: "m", Sections: []Section{{Heading: "Blocks", State: StateWriting}}}

	n, err := store.StartVersion(ctx, id, running)
	if err != nil || n != 1 {
		t.Fatalf("first version: %d, %v", n, err)
	}

	if _, err := store.StartVersion(ctx, id, running); !errors.Is(err, ErrBuilding) {
		t.Errorf("a second build while one runs: %v", err)
	}

	finished := time.Now()
	done := running
	done.Number, done.Status, done.FinishedAt, done.Seconds, done.Markdown = 1, StatusDone, &finished, 42, "# Residual networks\n"
	done.Sections = []Section{{Heading: "Blocks", State: StateDone, Text: "A block adds its input [1].",
		Claims: []answer.Claim{{Text: "A block adds its input.", Cites: []int{1}, Verdict: answer.Supported}}}}
	done.Sources = []Source{{N: 1, URL: "https://arxiv.org/abs/1512.03385", Title: "Deep Residual Learning"}}

	if err := store.SaveVersion(ctx, id, done); err != nil {
		t.Fatal(err)
	}

	if n, err := store.StartVersion(ctx, id, running); err != nil || n != 2 {
		t.Fatalf("rebuild: %d, %v", n, err)
	}

	list, err := store.List(ctx, lein, lab)
	if err != nil || len(list) != 1 || list[0].Latest.Number != 2 || list[0].Latest.Status != StatusRunning ||
		list[0].Latest.Sections[0].State != StateWriting || list[0].Latest.Sections[0].Text != "" {
		t.Fatalf("list: %+v, %v", list, err)
	}

	v1, err := store.Version(ctx, lein, id, 1)
	if err != nil || v1.Status != StatusDone || v1.Sections[0].Claims[0].Verdict != answer.Supported || v1.Sources[0].N != 1 ||
		v1.Markdown == "" || v1.Seconds != 42 || v1.Outline.Sections[0].Focus != "what a block computes" {
		t.Errorf("version 1: %+v, %v", v1, err)
	}

	if latest, err := store.Version(ctx, lein, id, 0); err != nil || latest.Number != 2 {
		t.Errorf("latest: %+v, %v", latest, err)
	}

	if numbers, err := store.Versions(ctx, lein, id); err != nil || !slices.Equal(numbers, []int{1, 2}) {
		t.Errorf("versions: %v, %v", numbers, err)
	}

	if _, err := store.Get(ctx, other, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user reads it: %v", err)
	}

	if _, err := store.Version(ctx, other, id, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user reads a version: %v", err)
	}

	if n, err := store.Interrupt(ctx); err != nil || n != 1 {
		t.Errorf("interrupt: %d, %v", n, err)
	}

	if v2, _ := store.Version(ctx, lein, id, 2); v2.Status != StatusFailed || v2.Error == "" {
		t.Errorf("an interrupted build: %+v", v2)
	}

	if err := store.Delete(ctx, other, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user deletes it: %v", err)
	}

	if err := store.Delete(ctx, lein, id); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Get(ctx, lein, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted, still there: %v", err)
	}
}

func TestTheNewestVersionIsCheckedAgainstItsPages(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()
	store := NewPostgresStore(pool)

	var owner, lab string
	if err := pool.QueryRow(ctx, `
		WITH u AS (INSERT INTO users (username, password_hash) VALUES ('lein', 'x') RETURNING id)
		INSERT INTO labs (owner_id, name) SELECT id, 'resnets' FROM u RETURNING owner_id, id`).Scan(&owner, &lab); err != nil {
		t.Fatal(err)
	}

	for _, u := range []string{"https://a.dev/", "https://b.dev/"} {
		if _, err := pool.Exec(ctx, `INSERT INTO page_reviews (url, content_hash, decision, model, tool_calls, seconds) VALUES ($1, 'r1', 'index', 'm', 0, 1)`, u); err != nil {
			t.Fatal(err)
		}
	}

	id, _ := store.Create(ctx, owner, lab, KindReport, "Residual networks", "")
	v := Version{Status: StatusRunning, Model: "m"}
	_, _ = store.StartVersion(ctx, id, v)

	if due, _ := store.ToCheck(ctx, 10); len(due) != 0 {
		t.Errorf("a version still being written is checked: %+v", due)
	}

	finished := time.Now()
	v.Number, v.Status, v.FinishedAt = 1, StatusDone, &finished
	v.Sources = []Source{{N: 1, URL: "https://a.dev/"}, {N: 2, URL: "https://b.dev/"}}
	v.Sections = []Section{{Heading: "Blocks", State: StateDone, Claims: []answer.Claim{{Text: "x", Cites: []int{1}, Verdict: answer.Supported, Quote: "q"}}}}
	_ = store.SaveVersion(ctx, id, v)

	due, err := store.ToCheck(ctx, 10)
	if err != nil || len(due) != 1 || due[0].ArtifactID != id || len(due[0].Sources) != 2 || len(due[0].Sections[0].Claims) != 1 {
		t.Fatalf("due %+v, %v", due, err)
	}

	hashes, _ := store.ReviewHashes(ctx, []string{"https://a.dev/", "https://b.dev/", "https://c.dev/"})
	if len(hashes) != 2 || hashes["https://a.dev/"] != "r1" {
		t.Errorf("hashes %v", hashes)
	}

	// One page checked: the other is still due.
	_ = store.SaveCheck(ctx, id, 1, Check{URL: "https://a.dev/", ReviewHash: "r1", Held: [][2]int{{0, 0}}})
	if due, _ := store.ToCheck(ctx, 10); len(due) != 1 {
		t.Errorf("a page not checked yet: %+v", due)
	}

	_ = store.SaveCheck(ctx, id, 1, Check{URL: "https://b.dev/", ReviewHash: "r1"})
	if due, _ := store.ToCheck(ctx, 10); len(due) != 0 {
		t.Errorf("checked in full, still due: %+v", due)
	}

	_, _ = pool.Exec(ctx, `UPDATE page_reviews SET content_hash = 'r2' WHERE url = 'https://a.dev/'`)
	if due, _ := store.ToCheck(ctx, 10); len(due) != 1 {
		t.Fatalf("a page reviewed anew: %+v", due)
	}

	changed := time.Now().Truncate(time.Microsecond)
	_ = store.SaveCheck(ctx, id, 1, Check{URL: "https://a.dev/", ReviewHash: "r2", Held: [][2]int{{0, 0}}, Missing: [][2]int{{0, 0}}, ChangedAt: &changed})

	got, err := store.Version(ctx, owner, id, 0)
	if err != nil || len(got.Drift) != 1 || got.Drift[0].URL != "https://a.dev/" || got.Drift[0].Claim != 0 || !got.Drift[0].ChangedAt.Equal(changed) {
		t.Errorf("drift %+v, %v", got.Drift, err)
	}

	list, _ := store.List(ctx, owner, lab)
	if len(list) != 1 || list[0].Drifted != 1 {
		t.Errorf("list %+v", list)
	}

	// A page that left the index is not looked for forever.
	_, _ = pool.Exec(ctx, `DELETE FROM page_reviews WHERE url = 'https://b.dev/'`)
	_ = store.SaveCheck(ctx, id, 1, Check{URL: "https://b.dev/", ReviewHash: "", Gone: true})

	if due, _ := store.ToCheck(ctx, 10); len(due) != 0 {
		t.Errorf("a gone page keeps its version due: %+v", due)
	}
}
