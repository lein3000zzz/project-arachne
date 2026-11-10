package indexing

import (
	"context"
	"errors"
	"time"
	"web-crawler/internal/documents"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Job is a queued page, as a worker holds it.
type Job struct {
	ID       int64
	Doc      documents.Document
	Attempts int
}

// Queue keeps pages waiting to be indexed across restarts.
type Queue interface {
	Push(ctx context.Context, doc *documents.Document) error
	// Waiting counts pages not yet indexed, dead ones aside.
	Waiting(ctx context.Context) (int, error)
	// Claim takes the oldest page ready to index whose URL has no older
	// version waiting; nil when there is none.
	Claim(ctx context.Context, owner string, lease time.Duration) (*Job, error)
	Extend(ctx context.Context, id int64, owner string, lease time.Duration) error
	Done(ctx context.Context, id int64) error
	// Retry counts a failed attempt and holds the page back for a while.
	Retry(ctx context.Context, id int64, after time.Duration, why string) error
	// Release gives a page back untried: its worker is stopping.
	Release(ctx context.Context, id int64) error
	// Bury keeps a page that failed every try, with why.
	Bury(ctx context.Context, id int64, why string) error
	// Prune drops dead pages older than the age given.
	Prune(ctx context.Context, age time.Duration) (int64, error)
}

type PostgresQueue struct {
	pool *pgxpool.Pool
}

func NewPostgresQueue(pool *pgxpool.Pool) *PostgresQueue {
	return &PostgresQueue{pool: pool}
}

func (q *PostgresQueue) Push(ctx context.Context, doc *documents.Document) error {
	_, err := q.pool.Exec(ctx, `
		INSERT INTO index_queue (url, run_id, title, content, content_type, fetched_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		doc.URL, doc.RunID, doc.Title, doc.Content, doc.ContentType, doc.FetchedAt)

	return err
}

func (q *PostgresQueue) Waiting(ctx context.Context) (int, error) {
	var n int
	err := q.pool.QueryRow(ctx, `SELECT count(*) FROM index_queue WHERE NOT dead`).Scan(&n)

	return n, err
}

func (q *PostgresQueue) Claim(ctx context.Context, owner string, lease time.Duration) (*Job, error) {
	var j Job

	err := q.pool.QueryRow(ctx, `
		UPDATE index_queue SET claimed_by = $1, claimed_until = now() + $2 * interval '1 second'
		WHERE id = (
			SELECT c.id FROM index_queue c
			WHERE NOT c.dead AND c.ready_at <= now() AND (c.claimed_until IS NULL OR c.claimed_until < now())
			  AND NOT EXISTS (SELECT 1 FROM index_queue o WHERE o.url = c.url AND o.id < c.id AND NOT o.dead)
			ORDER BY c.id
			LIMIT 1
			FOR UPDATE SKIP LOCKED)
		RETURNING id, url, run_id, title, content, content_type, fetched_at, attempts`,
		owner, lease.Seconds()).Scan(&j.ID, &j.Doc.URL, &j.Doc.RunID, &j.Doc.Title, &j.Doc.Content, &j.Doc.ContentType, &j.Doc.FetchedAt, &j.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	return &j, nil
}

func (q *PostgresQueue) Extend(ctx context.Context, id int64, owner string, lease time.Duration) error {
	_, err := q.pool.Exec(ctx, `UPDATE index_queue SET claimed_until = now() + $3 * interval '1 second' WHERE id = $1 AND claimed_by = $2`,
		id, owner, lease.Seconds())

	return err
}

func (q *PostgresQueue) Done(ctx context.Context, id int64) error {
	_, err := q.pool.Exec(ctx, `DELETE FROM index_queue WHERE id = $1`, id)
	return err
}

func (q *PostgresQueue) Retry(ctx context.Context, id int64, after time.Duration, why string) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE index_queue SET attempts = attempts + 1, ready_at = now() + $2 * interval '1 second', error = $3, claimed_by = NULL, claimed_until = NULL
		WHERE id = $1`, id, after.Seconds(), why)

	return err
}

func (q *PostgresQueue) Release(ctx context.Context, id int64) error {
	_, err := q.pool.Exec(ctx, `UPDATE index_queue SET claimed_by = NULL, claimed_until = NULL WHERE id = $1`, id)
	return err
}

func (q *PostgresQueue) Bury(ctx context.Context, id int64, why string) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE index_queue SET attempts = attempts + 1, dead = true, error = $2, claimed_by = NULL, claimed_until = NULL WHERE id = $1`, id, why)

	return err
}

func (q *PostgresQueue) Prune(ctx context.Context, age time.Duration) (int64, error) {
	tag, err := q.pool.Exec(ctx, `DELETE FROM index_queue WHERE dead AND queued_at < now() - $1 * interval '1 second'`, age.Seconds())
	return tag.RowsAffected(), err
}
