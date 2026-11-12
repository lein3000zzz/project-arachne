package endpoints

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func (s *PostgresStore) Get(ctx context.Context, userID string) (Endpoint, []byte, error) {
	var (
		e      Endpoint
		sealed []byte
	)

	err := s.pool.QueryRow(ctx, `
		SELECT provider, base_url, model, api_key, key_hint, updated_at FROM user_endpoints WHERE user_id = $1`, userID).
		Scan(&e.Provider, &e.BaseURL, &e.Model, &sealed, &e.KeyHint, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Endpoint{}, nil, ErrNotSet
	}

	e.KeySet = sealed != nil

	return e, sealed, err
}

func (s *PostgresStore) Set(ctx context.Context, userID string, e Endpoint, sealed []byte) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_endpoints (user_id, provider, base_url, model, api_key, key_hint, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (user_id) DO UPDATE SET provider = EXCLUDED.provider, base_url = EXCLUDED.base_url, model = EXCLUDED.model,
			api_key = EXCLUDED.api_key, key_hint = EXCLUDED.key_hint, updated_at = now()`,
		userID, e.Provider, e.BaseURL, e.Model, sealed, e.KeyHint)

	return err
}

func (s *PostgresStore) Delete(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM user_endpoints WHERE user_id = $1`, userID)
	return err
}
