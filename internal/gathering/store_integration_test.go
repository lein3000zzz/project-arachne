//go:build integration

package gathering

import (
	"context"
	"errors"
	"testing"
	"time"
	"web-crawler/internal/database/dbtest"
)

func TestPostgresStore(t *testing.T) {
	pool := dbtest.NewPool(t)
	store := NewPostgresStore(pool)
	ctx := context.Background()

	var labID int64
	if err := pool.QueryRow(ctx, `
		WITH u AS (INSERT INTO users (username, password_hash) VALUES ('lein', 'x') RETURNING id)
		INSERT INTO labs (owner_id, name) SELECT id, 'go' FROM u RETURNING id`).Scan(&labID); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Latest(ctx, labID); !errors.Is(err, ErrNotFound) {
		t.Errorf("before any: %v", err)
	}

	id, err := store.Start(ctx, labID, "more", []string{"brave"}, "small")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.Start(ctx, labID, "again", nil, "small"); !errors.Is(err, ErrRunning) {
		t.Errorf("a second at once: %v", err)
	}

	_ = store.AddStep(ctx, id, Step{Kind: StepSearch, Query: "go", Results: []Seen{{URL: "https://go.dev/", Title: "Go"}}, At: time.Now()})
	_ = store.AddStep(ctx, id, Step{Kind: StepAdd, URL: "https://go.dev/", Reason: "home", At: time.Now()})

	if err := store.Finish(ctx, id, StatusDone, "covered", ""); err != nil {
		t.Fatal(err)
	}

	g, err := store.Latest(ctx, labID)
	if err != nil || g.ID != id || g.Status != StatusDone || g.Summary != "covered" || g.FinishedAt == nil || len(g.Steps) != 2 ||
		g.Steps[0].Results[0].Title != "Go" || g.Steps[1].Reason != "home" || g.Engines[0] != "brave" {
		t.Fatalf("latest: %+v, %v", g, err)
	}

	second, err := store.Start(ctx, labID, "", []string{}, "small")
	if err != nil {
		t.Fatalf("after the first finished: %v", err)
	}

	if n, err := store.Interrupt(ctx); err != nil || n != 1 {
		t.Errorf("interrupted %d, %v", n, err)
	}

	if g, _ := store.Get(ctx, second); g.Status != StatusFailed || g.Error != "interrupted by a restart" {
		t.Errorf("after a restart: %+v", g)
	}
}
