package pages

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

const metaExcludeFingerprint = "exclude_fingerprint"

// EnsureExcluded runs when the exclude list has changed since it last ran: the
// fetched pages it now matches leave every lab, and their URLs are returned so
// the index drops them too. They stay in the graph - an excluded link is still
// recorded, only never followed - so PageRank is unchanged.
func (repo *PagePostgresRepo) EnsureExcluded(ctx context.Context, fingerprint string, excluded func(url string) bool) ([]string, error) {
	var stored string

	err := repo.pool.QueryRow(ctx, `SELECT value FROM meta WHERE key = $1`, metaExcludeFingerprint).Scan(&stored)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	if stored == fingerprint {
		return nil, nil
	}

	rows, err := repo.pool.Query(ctx, `SELECT id, url FROM pages WHERE fetched_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}

	pages, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedPage, error) {
		var page storedPage
		err := row.Scan(&page.id, &page.url)

		return page, err
	})
	if err != nil {
		return nil, err
	}

	var (
		ids  []int64
		urls []string
	)

	for _, page := range pages {
		if excluded(page.url) {
			ids = append(ids, page.id)
			urls = append(urls, page.url)
		}
	}

	err = pgx.BeginFunc(ctx, repo.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM source_pages WHERE page_id = ANY($1)`, ids); err != nil {
			return err
		}

		_, err := tx.Exec(ctx, `
			INSERT INTO meta (key, value) VALUES ($1, $2)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, metaExcludeFingerprint, fingerprint)

		return err
	})
	if err != nil {
		return nil, err
	}

	repo.logger.Infow("Applied a changed exclude list to stored pages", "excluded", len(urls))

	return urls, nil
}
