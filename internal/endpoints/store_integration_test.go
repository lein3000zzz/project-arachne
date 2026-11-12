//go:build integration

package endpoints

import (
	"context"
	"errors"
	"testing"
	"web-crawler/internal/database/dbtest"
)

func TestEndpointsAreKeptPerUser(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()
	store := NewPostgresStore(pool)

	var lein string
	if err := pool.QueryRow(ctx, `INSERT INTO users (username, password_hash) VALUES ('lein', 'x') RETURNING id`).Scan(&lein); err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.Get(ctx, lein); !errors.Is(err, ErrNotSet) {
		t.Errorf("none yet: %v", err)
	}

	e := Endpoint{Provider: "anthropic", BaseURL: "https://api.anthropic.com/v1", Model: "claude-haiku-4-5", KeyHint: "…abcd"}
	if err := store.Set(ctx, lein, e, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}

	got, sealed, err := store.Get(ctx, lein)
	if err != nil || got.Model != "claude-haiku-4-5" || !got.KeySet || got.KeyHint != "…abcd" || len(sealed) != 3 || got.UpdatedAt.IsZero() {
		t.Errorf("get: %+v, %v, %v", got, sealed, err)
	}

	e.Model, e.KeyHint = "claude-sonnet-5", ""
	if err := store.Set(ctx, lein, e, nil); err != nil {
		t.Fatal(err)
	}

	if got, sealed, _ := store.Get(ctx, lein); got.Model != "claude-sonnet-5" || got.KeySet || sealed != nil {
		t.Errorf("replaced: %+v, %v", got, sealed)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, lein); err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.Get(ctx, lein); !errors.Is(err, ErrNotSet) {
		t.Errorf("an account's endpoint goes with it: %v", err)
	}
}
