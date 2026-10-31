// Package runs submits crawl runs and reports how they are doing.
package runs

import (
	"context"
	"errors"
	"time"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/uploads"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

type Status string

const (
	StatusQueued  Status = "queued"
	StatusRunning Status = "running"
	// Crawled, but the indexer still has pages of it to embed.
	StatusIndexing Status = "indexing"
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
	// Documents handed to the indexer, and how many it indexed, failed, or
	// turned down on review.
	DocsQueued   int        `json:"docs_queued"`
	DocsIndexed  int        `json:"docs_indexed"`
	DocsFailed   int        `json:"docs_failed"`
	DocsRejected int        `json:"docs_rejected"`
	IndexedAt    *time.Time `json:"indexed_at,omitempty"`
	// PageRank has been recomputed since the crawl finished.
	Ranked bool `json:"ranked"`
	// The lab source the run crawled for, if any.
	SourceID int64 `json:"source_id,omitempty"`
}

const countTimeout = 2 * time.Second

type Store struct {
	pool       *pgxpool.Pool
	logger     *zap.SugaredLogger
	stallAfter time.Duration
}

// NewStore reports a started run as stalled once nothing has happened in it for
// stallAfter - the crawler's slot lease is the natural choice.
func NewStore(logger *zap.SugaredLogger, pool *pgxpool.Pool, stallAfter time.Duration) *Store {
	return &Store{pool: pool, logger: logger, stallAfter: stallAfter}
}

// Submitted records the run before it is produced, so its row - and with it
// the lab source its pages join - exists before the crawler saves any of them.
func (s *Store) Submitted(ctx context.Context, run *config.Run, origin Origin) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO runs (id, start_url, scope, max_depth, max_links, submitted_by, source_id)
		VALUES ($1, $2, $3, $4, $5, nullif($6, 0), nullif($7, 0))
		ON CONFLICT (id) DO NOTHING`,
		run.ID, run.StartURL, run.Scope, run.MaxDepth, run.MaxLinks, origin.UserID, origin.SourceID)

	return err
}

// Started records the run as the crawler accepted it - with its canonical start
// URL and scope - creating it if it was submitted some other way.
func (s *Store) Started(ctx context.Context, run *config.Run, startURL string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO runs (id, start_url, scope, max_depth, max_links, started_at) VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (id) DO UPDATE SET start_url = EXCLUDED.start_url, scope = EXCLUDED.scope,
			max_depth = EXCLUDED.max_depth, max_links = EXCLUDED.max_links,
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

// Queued and Done count a run's documents through the indexer. They only log
// their failures: losing a count must not lose the document. A document can come
// before the crawler has recorded its run, so the row is created if missing and
// filled in by Started.
func (s *Store) Queued(ctx context.Context, runID string) {
	s.count(ctx, runID, `
		INSERT INTO runs (id, start_url, max_depth, max_links, docs_queued) VALUES ($1, '', 0, 0, 1)
		ON CONFLICT (id) DO UPDATE SET docs_queued = runs.docs_queued + 1`)
}

func (s *Store) Done(ctx context.Context, runID string, failed bool) {
	column := "docs_indexed"
	if failed {
		column = "docs_failed"
	}

	s.count(ctx, runID, `UPDATE runs SET `+column+` = `+column+` + 1, indexed_at = now() WHERE id = $1`)
}

func (s *Store) Rejected(ctx context.Context, runID string) {
	s.count(ctx, runID, `UPDATE runs SET docs_rejected = docs_rejected + 1, indexed_at = now() WHERE id = $1`)
}

func (s *Store) count(ctx context.Context, runID, statement string) {
	ctx, cancel := context.WithTimeout(ctx, countTimeout)
	defer cancel()

	if _, err := s.pool.Exec(ctx, statement, runID); err != nil && s.logger != nil {
		s.logger.Warnw("Failed to count an indexed document", "runID", runID, "error", err)
	}
}

const selectRuns = `
	SELECT r.id, r.start_url, r.scope, r.max_depth, r.max_links, r.submitted_at, r.started_at, r.finished_at, r.note,
	       count(p.id) FILTER (WHERE p.fetched_at IS NOT NULL), max(p.fetched_at),
	       r.docs_queued, r.docs_indexed, r.docs_failed, r.docs_rejected, r.indexed_at,
	       coalesce((SELECT max(computed_at) FROM rank_runs) >= r.finished_at, false),
	       coalesce(r.source_id, 0)
	FROM runs r LEFT JOIN pages p ON p.last_run_id = r.id`

func (s *Store) List(ctx context.Context, limit int) ([]Run, error) {
	// Everyone sees this list: crawls of uploaded files are their owners' alone.
	rows, err := s.pool.Query(ctx, selectRuns+` WHERE r.start_url NOT LIKE $2 GROUP BY r.id ORDER BY r.submitted_at DESC LIMIT $1`,
		limit, "https://"+uploads.Host+"/%")
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
		&r.StartedAt, &r.FinishedAt, &r.Note, &r.Pages, &r.LastFetch,
		&r.DocsQueued, &r.DocsIndexed, &r.DocsFailed, &r.DocsRejected, &r.IndexedAt, &r.Ranked, &r.SourceID)

	return r, err
}

func (s *Store) status(r Run, now time.Time) Status {
	switch {
	case r.FinishedAt != nil && r.Note != "":
		return StatusDropped
	case r.FinishedAt != nil && r.DocsIndexed+r.DocsFailed+r.DocsRejected < r.DocsQueued:
		// Indexing that stopped moving - documents abandoned at a shutdown - would
		// otherwise keep the run "indexing" for good.
		last := *r.FinishedAt
		if r.IndexedAt != nil && r.IndexedAt.After(last) {
			last = *r.IndexedAt
		}

		if now.Sub(last) > s.stallAfter {
			return StatusFinished
		}

		return StatusIndexing
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
