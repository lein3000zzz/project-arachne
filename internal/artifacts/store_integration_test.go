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

	var lein, other, lab int64
	for name, id := range map[string]*int64{"lein": &lein, "other": &other} {
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
