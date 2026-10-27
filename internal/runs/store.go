// Package runs submits crawl runs and reports how they are doing.
package runs

import (
	"context"
	"errors"
	"time"
	"web-crawler/internal/domain/config"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Status string

const (
	StatusQueued   Status = "queued"
	StatusRunning  Status = "running"
	StatusFinished Status = "finished"
	StatusDropped  Status = "dropped"
	// Started, not finished, and nothing fetched for a while: the crawler that
	// held it died and the lease ran out, or it is waiting on a dead host.
	StatusStalled Status = "stalled"
)

type Run struct {
	ID          string     `json:"id"`
	StartURL    string     `json:"start_url"`
	Scope       string     `json:"scope"`
	MaxDepth    int        `json:"max_depth"`
	MaxLinks    int        `json:"max_links"`
	SubmittedAt time.Time  `json:"submitted_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	Note        string     `json:"note,omitempty"`
	Status      Status     `json:"status"`
	// Pages fetched whose latest crawl was this run.
	Pages     int        `json:"pages"`
	LastFetch *time.Time `json:"last_fetch,omitempty"`
}

type Store struct {
	pool       *pgxpool.Pool
	stallAfter time.Duration
}

// NewStore reports a started run as stalled once nothing has happened in it for
// stallAfter - the crawler's slot lease is the natural choice.
func NewStore(pool *pgxpool.Pool, stallAfter time.Duration) *Store {
	return &Store{pool: pool, stallAfter: stallAfter}
}

func (s *Store) Submitted(ctx context.Context, run *config.Run) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO runs (id, start_url, scope, max_depth, max_links) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING`,
		run.ID, run.StartURL, run.Scope, run.MaxDepth, run.MaxLinks)

	return err
}

// Started records the run as the crawler accepted it - with its canonical start
// URL and scope - creating it if it was submitted some other way.
func (s *Store) Started(ctx context.Context, run *config.Run, startURL string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO runs (id, start_url, scope, max_depth, max_links, started_at) VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (id) DO UPDATE SET start_url = EXCLUDED.start_url, scope = EXCLUDED.scope,
			started_at = coalesce(runs.started_at, now())`,
		run.ID, startURL, run.Scope, run.MaxDepth, run.MaxLinks)

	return err
}

// Finished records the end of a run; a note says why it ended without crawling.
func (s *Store) Finished(ctx context.Context, run *config.Run, note string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO runs (id, start_url, scope, max_depth, max_links, finished_at, note) VALUES ($1, $2, $3, $4, $5, now(), $6)
		ON CONFLICT (id) DO UPDATE SET finished_at = coalesce(runs.finished_at, now()), note = EXCLUDED.note`,
		run.ID, run.StartURL, run.Scope, run.MaxDepth, run.MaxLinks, note)

	return err
}

const selectRuns = `
	SELECT r.id, r.start_url, r.scope, r.max_depth, r.max_links, r.submitted_at, r.started_at, r.finished_at, r.note,
	       count(p.id) FILTER (WHERE p.fetched_at IS NOT NULL), max(p.fetched_at)
	FROM runs r LEFT JOIN pages p ON p.last_run_id = r.id`

func (s *Store) List(ctx context.Context, limit int) ([]Run, error) {
	rows, err := s.pool.Query(ctx, selectRuns+` GROUP BY r.id ORDER BY r.submitted_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}

	runs, err := pgx.CollectRows(rows, scanRun)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	for i := range runs {
		runs[i].Status = s.status(runs[i], now)
	}

	return runs, nil
}

func (s *Store) Get(ctx context.Context, id string) (Run, error) {
	rows, err := s.pool.Query(ctx, selectRuns+` WHERE r.id = $1 GROUP BY r.id`, id)
	if err != nil {
		return Run{}, err
	}

	run, err := pgx.CollectExactlyOneRow(rows, scanRun)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}

	if err != nil {
		return Run{}, err
	}

	run.Status = s.status(run, time.Now())

	return run, nil
}

func scanRun(row pgx.CollectableRow) (Run, error) {
	var r Run

	err := row.Scan(&r.ID, &r.StartURL, &r.Scope, &r.MaxDepth, &r.MaxLinks, &r.SubmittedAt,
		&r.StartedAt, &r.FinishedAt, &r.Note, &r.Pages, &r.LastFetch)

	return r, err
}

func (s *Store) status(r Run, now time.Time) Status {
	switch {
	case r.FinishedAt != nil && r.Note != "":
		return StatusDropped
	case r.FinishedAt != nil:
		return StatusFinished
	case r.StartedAt == nil:
		return StatusQueued
	}

	last := *r.StartedAt
	if r.LastFetch != nil && r.LastFetch.After(last) {
		last = *r.LastFetch
	}

	if now.Sub(last) > s.stallAfter {
		return StatusStalled
	}

	return StatusRunning
}
