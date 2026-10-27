package pages

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
	"web-crawler/internal/urlnorm"

	"github.com/jackc/pgx/v5"
)

const metaURLNormVersion = "urlnorm_version"

type storedPage struct {
	id        int64
	url       string
	fetchedAt *time.Time
}

// EnsureCanonical brings stored URLs in line with the current urlnorm.Version.
// Rows whose URL now canonicalizes to the same string are merged into one, and
// placeholders whose URL is no longer fetchable at all are deleted. It returns the
// URLs that stopped being pages, so whatever was indexed under them can go too.
func (repo *PagePostgresRepo) EnsureCanonical(ctx context.Context) ([]string, error) {
	version, err := repo.storedNormVersion(ctx)
	if err != nil {
		return nil, err
	}

	if version == urlnorm.Version {
		return nil, nil
	}

	repo.logger.Infow("Re-canonicalizing stored URLs", "from", version, "to", urlnorm.Version)

	pages, err := repo.storedPages(ctx)
	if err != nil {
		return nil, err
	}

	groups := make(map[string][]storedPage)

	var unusable []int64

	for _, page := range pages {
		canonical, ok := urlnorm.Canonical(page.url, nil)
		if !ok {
			unusable = append(unusable, page.id)
			continue
		}

		groups[canonical] = append(groups[canonical], page)
	}

	// Link targets that can never be fetched - markup the old parsers took for a
	// URL. A page that was actually fetched stays, whatever its URL looks like.
	if len(unusable) > 0 {
		tag, err := repo.pool.Exec(ctx, `DELETE FROM pages WHERE id = ANY($1) AND status IS NULL`, unusable)
		if err != nil {
			return nil, err
		}

		repo.logger.Infow("Dropped placeholders with unusable URLs", "count", tag.RowsAffected())
	}

	var removed []string

	for canonical, members := range groups {
		if len(members) == 1 && members[0].url == canonical {
			continue
		}

		if err := pgx.BeginFunc(ctx, repo.pool, func(tx pgx.Tx) error {
			return mergeSpellings(ctx, tx, canonical, members)
		}); err != nil {
			return nil, fmt.Errorf("merging spellings of %s: %w", canonical, err)
		}

		for _, member := range members {
			if member.url != canonical {
				removed = append(removed, member.url)
			}
		}
	}

	if err := repo.dropStaleAliases(ctx); err != nil {
		return nil, err
	}

	if _, err := repo.pool.Exec(ctx, `
		INSERT INTO meta (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		metaURLNormVersion, strconv.Itoa(urlnorm.Version)); err != nil {
		return nil, err
	}

	return removed, nil
}

// mergeSpellings keeps the row already spelled canonically, or else the most
// recently fetched one, and folds the others into it. The kept row takes the
// properties of whichever member was fetched last.
func mergeSpellings(ctx context.Context, tx pgx.Tx, canonical string, members []storedPage) error {
	keep := members[0]

	for _, member := range members[1:] {
		switch {
		case keep.url == canonical:
		case member.url == canonical, fetchedLater(member, keep):
			keep = member
		}
	}

	latest := keep
	for _, member := range members {
		if fetchedLater(member, latest) {
			latest = member
		}
	}

	if latest.id != keep.id {
		if _, err := tx.Exec(ctx, `
			UPDATE pages k SET status = o.status, title = o.title, content_type = o.content_type,
				last_run_id = o.last_run_id, fetched_at = o.fetched_at
			FROM pages o WHERE k.id = $1 AND o.id = $2`, keep.id, latest.id); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE pages k SET found_at = (SELECT min(found_at) FROM pages WHERE id = ANY($2))
		WHERE k.id = $1`, keep.id, memberIDs(members)); err != nil {
		return err
	}

	for _, member := range members {
		if member.id != keep.id {
			if err := fold(ctx, tx, member.id, keep.id, true); err != nil {
				return err
			}
		}
	}

	_, err := tx.Exec(ctx, `UPDATE pages SET url = $2, host = $3 WHERE id = $1`, keep.id, canonical, hostOf(canonical))

	return err
}

func fetchedLater(a, b storedPage) bool {
	return a.fetchedAt != nil && (b.fetchedAt == nil || a.fetchedAt.After(*b.fetchedAt))
}

func memberIDs(members []storedPage) []int64 {
	ids := make([]int64, len(members))
	for i, member := range members {
		ids[i] = member.id
	}

	return ids
}

// dropStaleAliases forgets aliases the normalizer would no longer produce. They
// are learned again the next time the old URL is fetched.
func (repo *PagePostgresRepo) dropStaleAliases(ctx context.Context) error {
	rows, err := repo.pool.Query(ctx, `SELECT url FROM aliases`)
	if err != nil {
		return err
	}

	aliases, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}

	var stale []string

	for _, alias := range aliases {
		if canonical, ok := urlnorm.Canonical(alias, nil); !ok || canonical != alias {
			stale = append(stale, alias)
		}
	}

	if len(stale) == 0 {
		return nil
	}

	_, err = repo.pool.Exec(ctx, `DELETE FROM aliases WHERE url = ANY($1)`, stale)

	return err
}

func (repo *PagePostgresRepo) storedNormVersion(ctx context.Context) (int, error) {
	var raw string

	err := repo.pool.QueryRow(ctx, `SELECT value FROM meta WHERE key = $1`, metaURLNormVersion).Scan(&raw)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, err
	}

	return strconv.Atoi(raw)
}

func (repo *PagePostgresRepo) storedPages(ctx context.Context) ([]storedPage, error) {
	rows, err := repo.pool.Query(ctx, `SELECT id, url, fetched_at FROM pages`)
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedPage, error) {
		var page storedPage
		err := row.Scan(&page.id, &page.url, &page.fetchedAt)

		return page, err
	})
}
