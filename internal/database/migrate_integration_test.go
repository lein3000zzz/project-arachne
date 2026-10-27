//go:build integration

package database_test

import (
	"context"
	"sync"
	"testing"
	"web-crawler/internal/database"
	"web-crawler/internal/database/dbtest"
)

func TestMigrateIsSafeToRunConcurrentlyAndAgain(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()

	var wg sync.WaitGroup

	for i := 0; i < 5; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if err := database.Migrate(ctx, pool); err != nil {
				t.Error(err)
			}
		}()
	}

	wg.Wait()

	var applied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}

	if applied != 1 {
		t.Errorf("%d migrations recorded, want 1", applied)
	}
}
