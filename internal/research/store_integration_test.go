//go:build integration

package research

import (
	"context"
	"errors"
	"testing"
	"web-crawler/internal/database/dbtest"
)

func TestPostgresStoreKeepsRunsAndTheMap(t *testing.T) {
	pool := dbtest.NewPool(t)
	store := NewPostgresStore(pool)
	ctx := context.Background()

	var labID, sourceID string
	if err := pool.QueryRow(ctx, `
		WITH u AS (INSERT INTO users (username, password_hash) VALUES ('lein', 'x') RETURNING id)
		INSERT INTO labs (owner_id, name) SELECT id, 'resnets' FROM u RETURNING id`).Scan(&labID); err != nil {
		t.Fatal(err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO sources (lab_id, kind, url, max_depth, max_links) VALUES ($1, 'research', 'https://arxiv.org/pdf/1505.00387', 1, 1) RETURNING id`,
		labID).Scan(&sourceID); err != nil {
		t.Fatal(err)
	}

	if _, err := store.LatestRun(ctx, labID); !errors.Is(err, ErrNotFound) {
		t.Errorf("before any run: %v", err)
	}

	run, err := store.StartRun(ctx, labID, "", Budget{Found: 300, Read: 50}, Focus{}, "small")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.StartRun(ctx, labID, "", Budget{Found: 1, Read: 1}, Focus{}, "small"); !errors.Is(err, ErrRunning) {
		t.Errorf("a second at once: %v", err)
	}

	subs, err := store.AddSubtopics(ctx, labID, []Subtopic{{Name: "Blocks", Focus: "residual blocks", Queries: []string{"resnet"}}, {Name: "Shortcuts"}})
	if err != nil || len(subs) != 2 || subs[0].Queries[0] != "resnet" || subs[1].Queries == nil {
		t.Fatalf("subtopics %+v, %v", subs, err)
	}

	if more, _ := store.AddSubtopics(ctx, labID, []Subtopic{{Name: "blocks"}, {Name: "Training"}}); len(more) != 3 || more[2].Name != "Training" {
		t.Errorf("a name the lab has is not added twice: %+v", more)
	}

	resnet, added, err := store.Find(ctx, labID, run, Work{URL: "https://arxiv.org/abs/1512.03385", Kind: KindWeb, Title: "ResNet", DOI: "10.48550/arxiv.1512.03385"},
		Find{Channel: ChannelSearch, Query: "resnet", Round: 1})
	if err != nil || !added {
		t.Fatal(added, err)
	}

	cited := 9
	same, added, err := store.Find(ctx, labID, run, Work{URL: "https://doi.org/10.1109/cvpr.2016.90", Kind: KindPaper, DOI: "10.48550/arxiv.1512.03385",
		OpenAlexID: "W1", Snippet: "Deeper networks", CitedBy: &cited, Year: 2016}, Find{Channel: ChannelCiting, Via: resnet, Round: 2})
	if err != nil || added || same != resnet {
		t.Fatalf("found again by its DOI: %s %v %v", same, added, err)
	}

	if _, _, err := store.Find(ctx, labID, run, Work{URL: "https://arxiv.org/abs/1512.03385"}, Find{Channel: ChannelSearch, Query: "resnet", Round: 3}); err != nil {
		t.Fatal(err)
	}

	highway, _, _ := store.Find(ctx, labID, run, Work{URL: "https://arxiv.org/pdf/1505.00387", Kind: KindPaper, OpenAlexID: "W2"},
		Find{Channel: ChannelReference, Via: resnet, Round: 2})

	if err := store.Link(ctx, labID, Link{From: resnet, To: highway}); err != nil {
		t.Fatal(err)
	}

	_ = store.Link(ctx, labID, Link{From: resnet, To: highway})

	if err := store.Judge(ctx, resnet, VerdictKeep, "the paper itself", subs[0].ID); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE works SET verdict = 'drop', by_user = true WHERE id = $1`, highway); err != nil {
		t.Fatal(err)
	}

	if err := store.Judge(ctx, highway, VerdictKeep, "the model's", ""); err != nil {
		t.Fatal(err)
	}

	_ = store.SetSource(ctx, highway, sourceID)
	_ = store.Expanded(ctx, resnet, true, false)

	works, err := store.Works(ctx, labID)
	if err != nil || len(works) != 2 {
		t.Fatalf("works %+v, %v", works, err)
	}

	r := works[0]
	if r.Kind != KindPaper || r.Title != "ResNet" || r.Snippet != "Deeper networks" || r.OpenAlexID != "W1" || r.Year != 2016 || r.CitedBy == nil ||
		*r.CitedBy != 9 || r.Verdict != VerdictKeep || r.SubtopicID != subs[0].ID || !r.ReferencesDone || r.CitingDone || len(r.Finds) != 2 ||
		r.Finds[1].Via != resnet || r.Read != "" {
		t.Errorf("merged work %+v", r)
	}

	h := works[1]
	if h.Verdict != VerdictDrop || !h.ByUser || h.SourceID != sourceID || h.Read != ReadFailed {
		t.Errorf("the user's verdict holds, and a source never crawled failed: %+v", h)
	}

	if links, _ := store.Links(ctx, labID); len(links) != 1 || links[0] != (Link{From: resnet, To: highway}) {
		t.Errorf("links %+v", links)
	}

	if refs, err := store.ReferencesOf(ctx, []string{highway}); err != nil || len(refs) != 0 {
		t.Errorf("a work never crawled cites nothing: %+v, %v", refs, err)
	}

	if err := store.SaveRounds(ctx, run, []Round{{N: 1, Found: map[string]int{ChannelSearch: 2}, Kept: 1}}); err != nil {
		t.Fatal(err)
	}

	if err := store.FinishRun(ctx, run, StatusDone, StopSaturated, ""); err != nil {
		t.Fatal(err)
	}

	latest, err := store.LatestRun(ctx, labID)
	if err != nil || latest.Stopped != StopSaturated || latest.FinishedAt == nil || latest.Rounds[0].Found[ChannelSearch] != 2 || latest.Budget.Found != 300 {
		t.Errorf("latest %+v, %v", latest, err)
	}

	second, _ := store.StartRun(ctx, labID, "more", Budget{Found: 10, Read: 1}, Focus{}, "small")

	if err := store.SaveRounds(ctx, second, nil); err != nil {
		t.Fatal(err)
	}

	if r, _ := store.Run(ctx, second); r.Rounds == nil {
		t.Error("a run without rounds reads as null, which the page cannot take")
	}
	if n, err := store.Interrupt(ctx); err != nil || n != 1 {
		t.Errorf("interrupted %d, %v", n, err)
	}

	if r, _ := store.Run(ctx, second); r.Status != StatusFailed || r.Stopped != StopInterrupted {
		t.Errorf("after a restart: %+v", r)
	}
}

// A work read reports what its site did: refused it (blocked), or gave a page
// with no text to index (empty).
func TestReadsKeptOutBySitesAreNamed(t *testing.T) {
	pool := dbtest.NewPool(t)
	store := NewPostgresStore(pool)
	ctx := context.Background()

	var labID string
	if err := pool.QueryRow(ctx, `
		WITH u AS (INSERT INTO users (username, password_hash) VALUES ('lein', 'x') RETURNING id)
		INSERT INTO labs (owner_id, name) SELECT id, 'zig' FROM u RETURNING id`).Scan(&labID); err != nil {
		t.Fatal(err)
	}

	read := func(url string, status int, queued int) string {
		t.Helper()

		var sourceID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO sources (lab_id, kind, url, max_depth, max_links) VALUES ($1, 'research', $2, 1, 1) RETURNING id`, labID, url).Scan(&sourceID); err != nil {
			t.Fatal(err)
		}

		_, err := pool.Exec(ctx, `
			WITH r AS (INSERT INTO runs (id, start_url, max_depth, max_links, submitted_at, finished_at, source_id, docs_queued)
			           VALUES ($1, $1, 1, 1, now() - interval '5 minutes', now() - interval '4 minutes', $2, $4)),
			     p AS (INSERT INTO pages (url, host, status, fetched_at) VALUES ($1, 'example.com', $3, now()) RETURNING id)
			INSERT INTO source_pages (source_id, page_id) SELECT $2, id FROM p`, url, sourceID, status, queued)
		if err != nil {
			t.Fatal(err)
		}

		id, _, err := store.Find(ctx, labID, "", Work{URL: url, Kind: KindWeb}, Find{Channel: ChannelSearch, Query: "zig"})
		if err != nil {
			t.Fatal(err)
		}

		_ = store.SetSource(ctx, id, sourceID)

		return id
	}

	blocked := read("https://medium.com/zig", 403, 0)
	empty := read("https://spa.example.com/zig", 200, 0)
	gone := read("https://penchef.example.com/zig", 404, 0)
	robots := read("https://www.reddit.com/r/zig", 0, 0)
	paper := read("https://arxiv.org/abs/zig", 200, 1)
	mirror := read("https://mirror.example.com/zig", 200, 1)

	// The mirror's text is the paper's, word for word.
	for _, url := range []string{"https://arxiv.org/abs/zig", "https://mirror.example.com/zig"} {
		if _, err := pool.Exec(ctx, `INSERT INTO page_reviews (url, content_hash, decision, model, tool_calls, seconds)
			VALUES ($1, 'same', 'index', 'm', 0, 0)`, url); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := pool.Exec(ctx, `UPDATE runs SET note = 'robots.txt disallows it' WHERE start_url = 'https://www.reddit.com/r/zig'`); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE pages SET status = NULL WHERE url = 'https://www.reddit.com/r/zig'`); err != nil {
		t.Fatal(err)
	}

	works, err := store.Works(ctx, labID)
	if err != nil {
		t.Fatal(err)
	}

	for _, w := range works {
		want := map[string]string{blocked: ReadBlocked, empty: ReadEmpty, gone: ReadGone, robots: ReadBlocked, paper: ReadIn, mirror: ReadCopy}[w.ID]
		if w.Read != want {
			t.Errorf("%s reads as %q, want %q", w.URL, w.Read, want)
		}

		if w.ID == mirror && w.CopyOf != "https://arxiv.org/abs/zig" {
			t.Errorf("the mirror copies %q", w.CopyOf)
		}
	}
}

func TestSubtopicsAreRenamedAndMerged(t *testing.T) {
	pool := dbtest.NewPool(t)
	store := NewPostgresStore(pool)
	ctx := context.Background()

	var labID, otherLab string
	if err := pool.QueryRow(ctx, `
		WITH u AS (INSERT INTO users (username, password_hash) VALUES ('lein', 'x') RETURNING id)
		INSERT INTO labs (owner_id, name) SELECT id, 'resnets' FROM u RETURNING id`).Scan(&labID); err != nil {
		t.Fatal(err)
	}

	if err := pool.QueryRow(ctx, `INSERT INTO labs (owner_id, name) SELECT owner_id, 'other' FROM labs WHERE id = $1 RETURNING id`, labID).Scan(&otherLab); err != nil {
		t.Fatal(err)
	}

	subs, _ := store.AddSubtopics(ctx, labID, []Subtopic{{Name: "Blocks", Queries: []string{"resnet", "blocks"}}, {Name: "Shortcuts", Queries: []string{"resnet", "highway"}}})
	blocks, shortcuts := subs[0].ID, subs[1].ID

	if err := store.RenameSubtopic(ctx, labID, blocks, "Residual blocks", "what a block computes"); err != nil {
		t.Fatal(err)
	}

	if err := store.RenameSubtopic(ctx, labID, blocks, "shortcuts", ""); !errors.Is(err, ErrSubtopicTaken) {
		t.Errorf("onto another's name: %v", err)
	}

	if err := store.RenameSubtopic(ctx, otherLab, blocks, "Mine", ""); !errors.Is(err, ErrSubtopicNotFound) {
		t.Errorf("through another lab: %v", err)
	}

	run, _ := store.StartRun(ctx, labID, "", Budget{Found: 1, Read: 1}, Focus{Subtopic: shortcuts}, "m")
	work, _, _ := store.Find(ctx, labID, run, Work{URL: "https://arxiv.org/abs/1505.00387", Kind: KindPaper, Title: "Highway"}, Find{Channel: ChannelSearch, Query: "highway"})
	if err := store.Judge(ctx, work, VerdictKeep, "", shortcuts); err != nil {
		t.Fatal(err)
	}

	if err := store.MergeSubtopics(ctx, otherLab, shortcuts, blocks); !errors.Is(err, ErrSubtopicNotFound) {
		t.Errorf("through another lab: %v", err)
	}

	if err := store.MergeSubtopics(ctx, labID, shortcuts, blocks); err != nil {
		t.Fatal(err)
	}

	left, _ := store.Subtopics(ctx, labID)
	if len(left) != 1 || left[0].Name != "Residual blocks" || len(left[0].Queries) != 3 {
		t.Errorf("after the merge: %+v", left)
	}

	works, _ := store.Works(ctx, labID)
	if len(works) != 1 || works[0].SubtopicID != blocks {
		t.Errorf("the work moved: %+v", works)
	}

	if r, _ := store.Run(ctx, run); r.Focus.Subtopic != blocks {
		t.Errorf("the run's focus moved: %+v", r.Focus)
	}
}
