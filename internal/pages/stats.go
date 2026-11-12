package pages

import (
	"context"
	"sync"
	"time"
	"web-crawler/internal/uploads"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Stats is the shared base at a glance: what everyone's look-ups search.
// Uploads are left out - they are private to their labs.
type Stats struct {
	Pages int `json:"pages"`
	Sites int `json:"sites"`
	// Turned down on review: kept, not searchable.
	SetAside int `json:"set_aside"`
	// In the indexing queue, not yet searchable.
	Waiting  int        `json:"waiting"`
	Crawling int        `json:"crawling"`
	Latest   *time.Time `json:"latest,omitempty"`
}

// StatsReader counts over the whole base, so it keeps an answer a while:
// the Look up page asks on every visit.
type StatsReader struct {
	pool *pgxpool.Pool
	keep time.Duration

	mu    sync.Mutex
	last  Stats
	until time.Time
}

func NewStatsReader(pool *pgxpool.Pool, keep time.Duration) *StatsReader {
	return &StatsReader{pool: pool, keep: keep}
}

func (r *StatsReader) Stats(ctx context.Context) (Stats, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if time.Now().Before(r.until) {
		return r.last, nil
	}

	var s Stats

	err := r.pool.QueryRow(ctx, `
		SELECT count(*), count(DISTINCT host), max(fetched_at),
			(SELECT count(*) FROM page_reviews WHERE decision = 'skip'),
			(SELECT count(*) FROM index_queue WHERE NOT dead),
			(SELECT count(*) FROM runs WHERE started_at IS NOT NULL AND finished_at IS NULL)
		FROM pages WHERE status BETWEEN 200 AND 299 AND host <> $1`, uploads.Host).
		Scan(&s.Pages, &s.Sites, &s.Latest, &s.SetAside, &s.Waiting, &s.Crawling)
	if err != nil {
		return Stats{}, err
	}

	r.last, r.until = s, time.Now().Add(r.keep)

	return s, nil
}
