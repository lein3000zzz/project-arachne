//go:build integration

package review

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"web-crawler/internal/copies"
	"web-crawler/internal/database/dbtest"
)

func TestSketchesAreKeptAndFilledIn(t *testing.T) {
	pool := dbtest.NewPool(t)
	store := NewPostgresStore(pool)
	ctx := context.Background()

	var text strings.Builder
	for i := range 60 {
		fmt.Fprintf(&text, "w%d ", i)
	}

	sketch := copies.SketchOf(copies.SetOf(text.String()))
	if sketch == nil {
		t.Fatal("no sketch to store")
	}

	index := Verdict{Decision: DecisionIndex, Reasons: []string{}, Model: "m"}
	for url, s := range map[string]copies.Sketch{"https://a.dev/sketched": sketch, "https://a.dev/short": nil} {
		if err := store.Save(ctx, url, "h", s, index); err != nil {
			t.Fatal(err)
		}
	}

	// Reviews from before sketches: one indexed, one set aside.
	for _, row := range []struct{ url, decision string }{{"https://a.dev/old", "index"}, {"https://a.dev/skipped", "skip"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO page_reviews (url, content_hash, decision, model, tool_calls, seconds)
			VALUES ($1, 'h', $2, 'm', 0, 0)`, row.url, row.decision); err != nil {
			t.Fatal(err)
		}
	}

	urls, err := store.Unsketched(ctx, 10)
	if err != nil || !slices.Equal(urls, []string{"https://a.dev/old"}) {
		t.Fatalf("unsketched %v, %v; a short page has an empty sketch, a page set aside needs none", urls, err)
	}

	if err := store.SaveSketch(ctx, "https://a.dev/old", sketch); err != nil {
		t.Fatal(err)
	}

	if urls, _ := store.Unsketched(ctx, 10); len(urls) != 0 {
		t.Errorf("still unsketched: %v", urls)
	}

	// A review since is newer than what a backfill read.
	if err := store.SaveSketch(ctx, "https://a.dev/sketched", nil); err != nil {
		t.Fatal(err)
	}

	got, err := store.Sketches(ctx, []string{"https://a.dev/sketched", "https://a.dev/short", "https://a.dev/missing"})
	if err != nil || len(got) != 1 || copies.Resemblance(got["https://a.dev/sketched"], sketch) != 1 {
		t.Errorf("sketches %v, %v; only pages with one", got, err)
	}

	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT sketch FROM page_reviews WHERE url = 'https://a.dev/sketched'`).Scan(&stored); err != nil ||
		copies.Resemblance(copies.SketchFrom(stored), sketch) != 1 {
		t.Errorf("sketch read back %d bytes, %v", len(stored), err)
	}
}
