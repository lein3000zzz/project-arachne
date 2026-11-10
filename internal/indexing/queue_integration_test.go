//go:build integration

package indexing

import (
	"context"
	"sync"
	"testing"
	"time"
	"web-crawler/internal/database/dbtest"
	"web-crawler/internal/documents"
)

func TestPostgresQueueHandsPagesOutInTurn(t *testing.T) {
	q := NewPostgresQueue(dbtest.NewPool(t))
	ctx := context.Background()

	push := func(url, content string) {
		t.Helper()

		if err := q.Push(ctx, &documents.Document{URL: url, RunID: "r", Title: "T", Content: content, ContentType: "text/html", FetchedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	push("https://a.dev/", "a1")
	push("https://a.dev/", "a2")
	push("https://b.dev/", "b1")

	first, err := q.Claim(ctx, "one", time.Minute)
	if err != nil || first == nil || first.Doc.Content != "a1" || first.Doc.RunID != "r" || first.Doc.ContentType != "text/html" {
		t.Fatalf("first claim %+v, %v", first, err)
	}

	// a2 waits behind a1, held or not: the next is b.
	second, _ := q.Claim(ctx, "two", time.Minute)
	if second == nil || second.Doc.Content != "b1" {
		t.Fatalf("second claim %+v: a page's next version waits for the one before", second)
	}

	if none, _ := q.Claim(ctx, "two", time.Minute); none != nil {
		t.Fatalf("claimed %+v while everything ready is held", none)
	}

	// a1 fails: it waits, and a2 still waits behind it.
	if err := q.Retry(ctx, first.ID, time.Hour, "embedder down"); err != nil {
		t.Fatal(err)
	}

	if none, _ := q.Claim(ctx, "one", time.Minute); none != nil {
		t.Fatalf("claimed %+v: a1 is held back, a2 behind it", none)
	}

	if err := q.Bury(ctx, first.ID, "embedder down for good"); err != nil {
		t.Fatal(err)
	}

	// Dead, a1 no longer holds a2 back.
	next, _ := q.Claim(ctx, "one", time.Minute)
	if next == nil || next.Doc.Content != "a2" {
		t.Fatalf("after a1 died: %+v", next)
	}

	if n, err := q.Waiting(ctx); err != nil || n != 2 {
		t.Errorf("waiting %d, %v; dead pages are not waiting", n, err)
	}

	for _, id := range []int64{next.ID, second.ID} {
		if err := q.Done(ctx, id); err != nil {
			t.Fatal(err)
		}
	}

	if n, _ := q.Waiting(ctx); n != 0 {
		t.Errorf("waiting %d after both done", n)
	}

	if n, err := q.Prune(ctx, time.Hour); err != nil || n != 0 {
		t.Errorf("pruned %d, %v; the dead page is younger than that", n, err)
	}

	if n, err := q.Prune(ctx, -time.Second); err != nil || n != 1 {
		t.Errorf("pruned %d, %v", n, err)
	}
}

func TestPostgresQueueLeases(t *testing.T) {
	q := NewPostgresQueue(dbtest.NewPool(t))
	ctx := context.Background()

	_ = q.Push(ctx, &documents.Document{URL: "https://a.dev/", FetchedAt: time.Now()})

	held, _ := q.Claim(ctx, "dead crawler", 300*time.Millisecond)
	if held == nil {
		t.Fatal("nothing claimed")
	}

	// Another owner cannot renew it.
	_ = q.Extend(ctx, held.ID, "someone else", time.Hour)

	time.Sleep(400 * time.Millisecond)

	again, _ := q.Claim(ctx, "live crawler", time.Hour)
	if again == nil || again.ID != held.ID {
		t.Fatalf("a lapsed hold: %+v", again)
	}

	if err := q.Extend(ctx, again.ID, "live crawler", time.Hour); err != nil {
		t.Fatal(err)
	}

	if err := q.Release(ctx, again.ID); err != nil {
		t.Fatal(err)
	}

	if back, _ := q.Claim(ctx, "next start", time.Minute); back == nil || back.Attempts != 0 {
		t.Errorf("a page given back is taken again, untried: %+v", back)
	}
}

func TestPostgresQueueGivesEachPageToOneWorker(t *testing.T) {
	q := NewPostgresQueue(dbtest.NewPool(t))
	ctx := context.Background()

	for i := range 20 {
		_ = q.Push(ctx, &documents.Document{URL: "https://a.dev/" + string(rune('a'+i)), FetchedAt: time.Now()})
	}

	var (
		mu   sync.Mutex
		seen = map[int64]int{}
		wg   sync.WaitGroup
	)

	for w := range 4 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for {
				job, err := q.Claim(ctx, string(rune('0'+w)), time.Minute)
				if err != nil {
					t.Error(err)
					return
				}

				if job == nil {
					return
				}

				mu.Lock()
				seen[job.ID]++
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	if len(seen) != 20 {
		t.Errorf("%d pages handed out, want 20", len(seen))
	}

	for id, n := range seen {
		if n != 1 {
			t.Errorf("page %d handed out %d times", id, n)
		}
	}
}
