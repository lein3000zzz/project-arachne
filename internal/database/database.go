// Package database opens the Postgres pool every binary shares and keeps its
// schema current. Migrations are embedded SQL files applied in name order.
package database

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Any constant works as long as nothing else in the database uses it: it
// serializes migrations across crawler nodes, search and rank starting at once.
const migrationLockKey = 0x61726163686e65

func Open(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing postgres DSN: %w", err)
	}

	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}

	return pool, nil
}

// Migrate applies every embedded migration not yet recorded, each in its own
// transaction.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("taking the migration lock: %w", err)
	}

	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name       text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return err
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}

	for _, name := range names {
		if err := apply(ctx, conn.Conn(), name); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}

	return nil
}

func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}

	sort.Strings(names)

	return names, nil
}

func apply(ctx context.Context, conn *pgx.Conn, name string) error {
	var applied bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&applied); err != nil {
		return err
	}

	if applied {
		return nil
	}

	body, err := migrationFiles.ReadFile("migrations/" + name)
	if err != nil {
		return err
	}

	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return err
		}

		_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name)

		return err
	})
}

// Retryable reports errors after which the whole transaction can simply be run
// again: deadlocks and serialization failures between concurrent savers.
func Retryable(err error) bool {
	var pgErr interface{ SQLState() string }
	if !errors.As(err, &pgErr) {
		return false
	}

	switch pgErr.SQLState() {
	case "40001", "40P01":
		return true
	}

	return false
}
