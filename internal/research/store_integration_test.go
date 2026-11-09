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

	var labID, sourceID int64
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

	run, err := store.StartRun(ctx, labID, "", Budget{Found: 300, Read: 50}, "small")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.StartRun(ctx, labID, "", Budget{Found: 1, Read: 1}, "small"); !errors.Is(err, ErrRunning) {
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
		t.Fatalf("found again by its DOI: %d %v %v", same, added, err)
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

	if err := store.Judge(ctx, highway, VerdictKeep, "the model's", 0); err != nil {
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

	if refs, err := store.ReferencesOf(ctx, []int64{highway}); err != nil || len(refs) != 0 {
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

	second, _ := store.StartRun(ctx, labID, "more", Budget{Found: 10, Read: 1}, "small")
	if n, err := store.Interrupt(ctx); err != nil || n != 1 {
		t.Errorf("interrupted %d, %v", n, err)
	}

	if r, _ := store.Run(ctx, second); r.Status != StatusFailed || r.Stopped != StopInterrupted {
		t.Errorf("after a restart: %+v", r)
	}
}
