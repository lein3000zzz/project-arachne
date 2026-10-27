package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore keeps every time in the database's clock, so nodes with
// drifting clocks agree on what has expired.
type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const uniqueViolation = "23505"

func (s *PostgresStore) UserCount(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)

	return n, err
}

func (s *PostgresStore) CreateUser(ctx context.Context, username, passwordHash string, admin bool) (User, error) {
	var u User

	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (username, password_hash, is_admin) VALUES ($1, $2, $3)
		RETURNING id, username, is_admin, created_at`,
		username, passwordHash, admin).Scan(&u.ID, &u.Username, &u.Admin, &u.CreatedAt)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return User{}, ErrUserExists
	}

	return u, err
}

func (s *PostgresStore) AccountByName(ctx context.Context, username string) (Account, error) {
	var a Account

	err := s.pool.QueryRow(ctx, `
		SELECT id, username, is_admin, created_at, password_hash, disabled_at IS NOT NULL
		FROM users WHERE lower(username) = lower($1)`, username).
		Scan(&a.ID, &a.Username, &a.Admin, &a.CreatedAt, &a.PasswordHash, &a.Disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrUserNotFound
	}

	return a, err
}

func (s *PostgresStore) PasswordHash(ctx context.Context, userID int64) (string, error) {
	var hash string

	err := s.pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE id = $1 AND disabled_at IS NULL`, userID).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUserNotFound
	}

	return hash, err
}

func (s *PostgresStore) SetPasswordHash(ctx context.Context, userID int64, hash string) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, userID, hash)
	return err
}

func (s *PostgresStore) ReplacePassword(ctx context.Context, userID int64, hash string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, userID, hash)
		if err != nil {
			return err
		}

		if tag.RowsAffected() == 0 {
			return ErrUserNotFound
		}

		_, err = tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID)

		return err
	})
}

func (s *PostgresStore) SetDisabled(ctx context.Context, userID int64, disabled bool) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE users SET disabled_at = CASE WHEN $2 THEN coalesce(disabled_at, now()) END WHERE id = $1`,
			userID, disabled); err != nil {
			return err
		}

		if !disabled {
			return nil
		}

		_, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID)

		return err
	})
}

func (s *PostgresStore) CreateSession(ctx context.Context, tokenHash []byte, userID int64, ttl time.Duration, userAgent string) (time.Time, error) {
	var expires time.Time

	err := s.pool.QueryRow(ctx, `
		WITH expired AS (DELETE FROM sessions WHERE expires_at < now())
		INSERT INTO sessions (token_hash, user_id, expires_at, user_agent)
		VALUES ($1, $2, now() + make_interval(secs => $3), $4)
		RETURNING expires_at`,
		tokenHash, userID, ttl.Seconds(), userAgent).Scan(&expires)

	return expires, err
}

func (s *PostgresStore) SessionUser(ctx context.Context, tokenHash []byte, ttl, renewAfter time.Duration) (User, time.Time, error) {
	var (
		u       User
		renewed *time.Time
	)

	err := s.pool.QueryRow(ctx, `
		WITH s AS (
			SELECT user_id, last_seen_at < now() - make_interval(secs => $3) AS stale
			FROM sessions WHERE token_hash = $1 AND expires_at > now()
		), renewed AS (
			UPDATE sessions SET last_seen_at = now(), expires_at = now() + make_interval(secs => $2)
			WHERE token_hash = $1 AND (SELECT stale FROM s)
			RETURNING expires_at
		)
		SELECT u.id, u.username, u.is_admin, u.created_at, (SELECT expires_at FROM renewed)
		FROM s JOIN users u ON u.id = s.user_id
		WHERE u.disabled_at IS NULL`,
		tokenHash, ttl.Seconds(), renewAfter.Seconds()).Scan(&u.ID, &u.Username, &u.Admin, &u.CreatedAt, &renewed)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, time.Time{}, ErrUnauthenticated
	}

	if err != nil || renewed == nil {
		return u, time.Time{}, err
	}

	return u, *renewed, nil
}

func (s *PostgresStore) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	return err
}

func (s *PostgresStore) SaveToken(ctx context.Context, userID int64, name string, tokenHash []byte) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO api_tokens (token_hash, user_id, name) VALUES ($1, $2, $3)
		ON CONFLICT (user_id, name) DO UPDATE
		SET token_hash = EXCLUDED.token_hash, created_at = now(), last_used_at = NULL`,
		tokenHash, userID, name)

	return err
}

func (s *PostgresStore) DeleteToken(ctx context.Context, userID int64, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM api_tokens WHERE user_id = $1 AND name = $2`, userID, name)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrTokenNotFound
	}

	return err
}

func (s *PostgresStore) TokenUser(ctx context.Context, tokenHash []byte, touchAfter time.Duration) (User, error) {
	var u User

	err := s.pool.QueryRow(ctx, `
		WITH t AS (
			SELECT user_id, coalesce(last_used_at < now() - make_interval(secs => $2), true) AS stale
			FROM api_tokens WHERE token_hash = $1
		), touched AS (
			UPDATE api_tokens SET last_used_at = now() WHERE token_hash = $1 AND (SELECT stale FROM t)
		)
		SELECT u.id, u.username, u.is_admin, u.created_at
		FROM t JOIN users u ON u.id = t.user_id
		WHERE u.disabled_at IS NULL`,
		tokenHash, touchAfter.Seconds()).Scan(&u.ID, &u.Username, &u.Admin, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUnauthenticated
	}

	return u, err
}

func (s *PostgresStore) LockedFor(ctx context.Context, keys []string) (time.Duration, error) {
	if len(keys) == 0 {
		return 0, nil
	}

	var seconds float64

	err := s.pool.QueryRow(ctx, `
		SELECT coalesce(extract(epoch FROM max(locked_until) - now()), 0)::float8
		FROM login_failures WHERE key = ANY($1) AND locked_until > now()`, keys).Scan(&seconds)

	return time.Duration(seconds * float64(time.Second)), err
}

// RecordFailure returns how many failures in a row the key has had, counting
// afresh once the last one is older than window. Stale rows of other keys are
// swept on the way.
func (s *PostgresStore) RecordFailure(ctx context.Context, key string, window time.Duration) (int, error) {
	var n int

	err := s.pool.QueryRow(ctx, `
		WITH stale AS (
			DELETE FROM login_failures
			WHERE key <> $1 AND last_failed_at < now() - make_interval(secs => $2)
			  AND (locked_until IS NULL OR locked_until < now())
		)
		INSERT INTO login_failures AS f (key, failures, last_failed_at) VALUES ($1, 1, now())
		ON CONFLICT (key) DO UPDATE SET
			failures = CASE WHEN f.last_failed_at < now() - make_interval(secs => $2) THEN 1 ELSE f.failures + 1 END,
			last_failed_at = now()
		RETURNING failures`,
		key, window.Seconds()).Scan(&n)

	return n, err
}

func (s *PostgresStore) Lock(ctx context.Context, key string, d time.Duration) error {
	_, err := s.pool.Exec(ctx, `UPDATE login_failures SET locked_until = now() + make_interval(secs => $2) WHERE key = $1`,
		key, d.Seconds())

	return err
}

func (s *PostgresStore) ClearFailures(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM login_failures WHERE key = $1`, key)
	return err
}
