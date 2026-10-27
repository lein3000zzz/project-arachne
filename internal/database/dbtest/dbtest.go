// Package dbtest gives integration tests a throwaway, migrated database on the
// Postgres at POSTGRES_DSN, so they never touch the crawler's data.
package dbtest

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
	"web-crawler/internal/database"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const defaultDSN = "postgres://arachne:arachne-postgres@localhost:5432/arachne?sslmode=disable"

func NewPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	ctx := context.Background()

	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		dsn = defaultDSN
	}

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}

	name := fmt.Sprintf("arachne_test_%d", time.Now().UnixNano())

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}

	cfg.ConnConfig.Database = name

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(context.Background())
	})

	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	return pool
}
