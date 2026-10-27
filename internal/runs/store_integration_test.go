//go:build integration

package runs

import (
	"context"
	"errors"
	"testing"
	"time"
	"web-crawler/internal/database/dbtest"
	"web-crawler/internal/domain/config"
)

func TestStoreTracksARun(t *testing.T) {
	pool := dbtest.NewPool(t)
	store := NewStore(pool, 10*time.Minute)
	ctx := context.Background()

	run := &config.Run{ID: "r1", StartURL: "go.dev/doc", MaxDepth: 2, MaxLinks: 10}

	if err := store.Submitted(ctx, run); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(ctx, "r1")
	if err != nil || got.Status != StatusQueued {
		t.Fatalf("after submit: %+v, %v", got, err)
	}

	run.Scope = "https://go.dev/doc/"
	if err := store.Started(ctx, run, "https://go.dev/doc"); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO pages (url, host, status, last_run_id, fetched_at) VALUES
			('https://go.dev/doc', 'go.dev', 200, 'r1', now()),
			('https://go.dev/doc/a', 'go.dev', 200, 'r1', now()),
			('https://go.dev/doc/b', 'go.dev', NULL, 'r1', NULL),
			('https://go.dev/other', 'go.dev', 200, 'r0', now())`); err != nil {
		t.Fatal(err)
	}

	got, err = store.Get(ctx, "r1")
	if err != nil || got.Status != StatusRunning || got.Pages != 2 || got.StartURL != "https://go.dev/doc" || got.Scope != "https://go.dev/doc/" {
		t.Fatalf("while running: %+v, %v", got, err)
	}

	if err := store.Finished(ctx, run, ""); err != nil {
		t.Fatal(err)
	}

	unknown := &config.Run{ID: "r2", StartURL: "https://x.example/", MaxDepth: 1, MaxLinks: 1}
	if err := store.Started(ctx, unknown, "https://x.example/"); err != nil {
		t.Fatal(err)
	}

	list, err := store.List(ctx, 10)
	if err != nil || len(list) != 2 || list[0].ID != "r2" || list[0].Status != StatusRunning || list[1].Status != StatusFinished {
		t.Fatalf("list: %+v, %v", list, err)
	}

	if _, err := store.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown run: %v", err)
	}
}
