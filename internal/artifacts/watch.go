package artifacts

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"
	"web-crawler/internal/answer"
	"web-crawler/internal/facts"
	"web-crawler/internal/vectorstore"

	"go.uber.org/zap"
)

// Drift is a claim of a version whose words its page no longer has.
type Drift struct {
	Section int    `json:"section"`
	Claim   int    `json:"claim"`
	URL     string `json:"url"`
	// The page left the index altogether.
	Gone      bool       `json:"gone,omitempty"`
	ChangedAt *time.Time `json:"changed_at,omitempty"`
}

// Watched is the newest version of an artifact, with what its claims rest on.
type Watched struct {
	ArtifactID string
	Version    int
	Sections   []Section
	Sources    []Source
}

// Check is what one cited page said, last time it was looked at.
type Check struct {
	URL        string
	ReviewHash string
	Held       [][2]int
	Missing    [][2]int
	Gone       bool
	ChangedAt  *time.Time
}

type CheckStore interface {
	// ToCheck is newest versions never checked, or citing a page whose
	// version changed since.
	ToCheck(ctx context.Context, limit int) ([]Watched, error)
	ReviewHashes(ctx context.Context, urls []string) (map[string]string, error)
	Checks(ctx context.Context, artifactID string, version int) (map[string]Check, error)
	SaveCheck(ctx context.Context, artifactID string, version int, c Check) error
}

// Chunks reads a page's indexed chunks back.
type Chunks interface {
	Chunks(ctx context.Context, url string) ([]vectorstore.Row, error)
}

const watchBatch = 20

// Watch keeps the newest version of every artifact checked against the pages
// it cites, until ctx ends. A version is looked at once when built, and again
// whenever a page it cites is reviewed in a new version.
func Watch(ctx context.Context, logger *zap.SugaredLogger, store CheckStore, chunks Chunks, every time.Duration) {
	every = cmp.Or(every, time.Minute)

	for {
		if err := CheckRound(ctx, store, chunks); err != nil && ctx.Err() == nil {
			logger.Warnw("Checking artifacts against their pages failed", "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// CheckRound checks what is due, a batch at a time.
func CheckRound(ctx context.Context, store CheckStore, chunks Chunks) error {
	due, err := store.ToCheck(ctx, watchBatch)
	if err != nil {
		return err
	}

	for _, w := range due {
		if err := check(ctx, store, chunks, w); err != nil {
			return err
		}
	}

	return nil
}

func check(ctx context.Context, store CheckStore, chunks Chunks, w Watched) error {
	if len(w.Sources) == 0 {
		return store.SaveCheck(ctx, w.ArtifactID, w.Version, Check{})
	}

	urls := make([]string, len(w.Sources))
	for i, src := range w.Sources {
		urls[i] = src.URL
	}

	hashes, err := store.ReviewHashes(ctx, urls)
	if err != nil {
		return err
	}

	checks, err := store.Checks(ctx, w.ArtifactID, w.Version)
	if err != nil {
		return err
	}

	for _, src := range w.Sources {
		old, had := checks[src.URL]
		if had && old.ReviewHash == hashes[src.URL] {
			continue
		}

		rows, err := chunks.Chunks(ctx, src.URL)
		if err != nil {
			return err
		}

		texts := make([]string, len(rows))
		for i, r := range rows {
			texts[i] = r.Text
		}

		page := facts.Normalize(strings.Join(texts, " "))
		found := holds(w.Sections, src.N, page)

		c := Check{URL: src.URL, ReviewHash: hashes[src.URL], Gone: len(rows) == 0}

		if !had {
			c.Held = found
		} else {
			c.Held = old.Held

			for _, k := range old.Held {
				if !slices.Contains(found, k) {
					c.Missing = append(c.Missing, k)
				}
			}

			c.ChangedAt = old.ChangedAt
			if len(c.Missing) > 0 && c.ChangedAt == nil {
				now := time.Now()
				c.ChangedAt = &now
			}

			if len(c.Missing) == 0 {
				c.ChangedAt = nil
			}
		}

		if err := store.SaveCheck(ctx, w.ArtifactID, w.Version, c); err != nil {
			return err
		}
	}

	return nil
}

// holds is the claims citing source n whose quote the page's text holds.
func holds(sections []Section, n int, page string) [][2]int {
	var out [][2]int

	for i, s := range sections {
		for j, c := range s.Claims {
			if c.Quote == "" || !slices.Contains(c.Cites, n) || (c.Verdict != answer.Supported && c.Verdict != answer.Partly) {
				continue
			}

			if _, ok := facts.Locate(facts.Normalize(c.Quote), page); ok {
				out = append(out, [2]int{i, j})
			}
		}
	}

	return out
}
