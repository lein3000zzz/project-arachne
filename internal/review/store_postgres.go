package review

import (
	"context"
	"web-crawler/internal/copies"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func (s *PostgresStore) Save(ctx context.Context, url, contentHash string, sketch copies.Sketch, v Verdict) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO page_reviews (url, content_hash, sketch, decision, reasons, summary, unreviewed, model, tool_calls, seconds)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (url) DO UPDATE SET
			content_hash = EXCLUDED.content_hash, sketch = EXCLUDED.sketch, decision = EXCLUDED.decision, reasons = EXCLUDED.reasons,
			summary = EXCLUDED.summary, unreviewed = EXCLUDED.unreviewed, model = EXCLUDED.model,
			tool_calls = EXCLUDED.tool_calls, seconds = EXCLUDED.seconds, reviewed_at = now()`,
		url, contentHash, sketch.Bytes(), v.Decision, v.Reasons, v.Summary, v.Unreviewed, v.Model, v.ToolCalls, v.Seconds)

	return err
}

// Unsketched lists pages indexed on review that have no sketch yet: reviewed
// before sketches were kept.
func (s *PostgresStore) Unsketched(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT url FROM page_reviews WHERE sketch IS NULL AND decision = 'index' ORDER BY url LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Sketches gives the sketch of each of urls that has one.
func (s *PostgresStore) Sketches(ctx context.Context, urls []string) (map[string]copies.Sketch, error) {
	rows, err := s.pool.Query(ctx, `SELECT url, sketch FROM page_reviews WHERE url = ANY($1) AND length(sketch) > 0`, urls)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]copies.Sketch{}

	for rows.Next() {
		var (
			url    string
			sketch []byte
		)

		if err := rows.Scan(&url, &sketch); err != nil {
			return nil, err
		}

		if sk := copies.SketchFrom(sketch); sk != nil {
			out[url] = sk
		}
	}

	return out, rows.Err()
}

func (s *PostgresStore) SaveSketch(ctx context.Context, url string, sketch copies.Sketch) error {
	_, err := s.pool.Exec(ctx, `UPDATE page_reviews SET sketch = $2 WHERE url = $1 AND sketch IS NULL`, url, sketch.Bytes())
	return err
}
