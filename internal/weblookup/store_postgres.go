package weblookup

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresPages struct {
	pool *pgxpool.Pool
}

func NewPostgresPages(pool *pgxpool.Pool) *PostgresPages {
	return &PostgresPages{pool: pool}
}

func (p *PostgresPages) Fresh(ctx context.Context, urls []string, d time.Duration) ([]string, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT url FROM pages
		WHERE url = ANY($1) AND status BETWEEN 200 AND 299 AND fetched_at > now() - make_interval(secs => $2)`,
		urls, d.Seconds())
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (p *PostgresPages) OfRun(ctx context.Context, runID string) ([]string, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT url FROM pages WHERE last_run_id = $1 AND status BETWEEN 200 AND 299 ORDER BY url`, runID)
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, pgx.RowTo[string])
}
