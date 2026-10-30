package review

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func (s *PostgresStore) Save(ctx context.Context, url, contentHash string, v Verdict) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO page_reviews (url, content_hash, decision, reasons, summary, unreviewed, model, tool_calls, seconds)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (url) DO UPDATE SET
			content_hash = EXCLUDED.content_hash, decision = EXCLUDED.decision, reasons = EXCLUDED.reasons,
			summary = EXCLUDED.summary, unreviewed = EXCLUDED.unreviewed, model = EXCLUDED.model,
			tool_calls = EXCLUDED.tool_calls, seconds = EXCLUDED.seconds, reviewed_at = now()`,
		url, contentHash, v.Decision, v.Reasons, v.Summary, v.Unreviewed, v.Model, v.ToolCalls, v.Seconds)

	return err
}
