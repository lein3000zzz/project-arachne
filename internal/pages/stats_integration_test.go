//go:build integration

package pages

import (
	"context"
	"testing"
	"time"
	"web-crawler/internal/database/dbtest"
)

func TestStatsCountTheSharedBase(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()

	exec := func(sql string, args ...any) {
		t.Helper()

		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	exec(`INSERT INTO pages (url, host, status, fetched_at) VALUES
		('https://a.dev/', 'a.dev', 200, now()), ('https://a.dev/x', 'a.dev', 200, now()),
		('https://b.dev/', 'b.dev', 200, now()), ('https://b.dev/gone', 'b.dev', 404, now()),
		('https://c.dev/', 'c.dev', NULL, NULL),
		('https://uploads.arachne.invalid/f', 'uploads.arachne.invalid', 200, now())`)
	exec(`INSERT INTO page_reviews (url, content_hash, decision, model, tool_calls, seconds) VALUES ('https://a.dev/x', 'h', 'skip', 'm', 0, 1)`)
	exec(`INSERT INTO index_queue (url, fetched_at) VALUES ('https://b.dev/', now())`)
	exec(`INSERT INTO index_queue (url, fetched_at, dead) VALUES ('https://a.dev/', now(), true)`)
	exec(`INSERT INTO runs (id, start_url, max_depth, max_links, started_at) VALUES ('r1', 'https://a.dev/', 1, 1, now()), ('r2', 'https://b.dev/', 1, 1, NULL)`)

	r := NewStatsReader(pool, time.Hour)

	s, err := r.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if s.Pages != 3 || s.Sites != 2 || s.SetAside != 1 || s.Waiting != 1 || s.Crawling != 1 || s.Latest == nil {
		t.Errorf("%+v", s)
	}

	exec(`DELETE FROM index_queue`)

	if again, _ := r.Stats(ctx); again.Waiting != 1 {
		t.Errorf("counted again within the hour: %+v", again)
	}
}
