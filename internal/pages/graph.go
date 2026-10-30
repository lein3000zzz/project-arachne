package pages

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"web-crawler/internal/domain/data"

	"github.com/jackc/pgx/v5"
)

const (
	kindHyperlink int16 = 1
	kindResource  int16 = 2
)

const (
	upsertPage = `
		INSERT INTO pages (url, host, status, title, content_type, last_run_id, found_at, fetched_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (url) DO UPDATE SET
			host = EXCLUDED.host, status = EXCLUDED.status, title = EXCLUDED.title,
			content_type = EXCLUDED.content_type, last_run_id = EXCLUDED.last_run_id,
			fetched_at = EXCLUDED.fetched_at
		RETURNING id`

	resolveAliases = `
		SELECT a.url, p.url FROM aliases a JOIN pages p ON p.id = a.target_id WHERE a.url = ANY($1)`

	insertPlaceholders = `
		INSERT INTO pages (url, host, found_at)
		SELECT u, h, $3 FROM unnest($1::text[], $2::text[]) AS t (u, h)
		ON CONFLICT (url) DO NOTHING`

	pageIDs = `SELECT url, id FROM pages WHERE url = ANY($1)`

	dropStaleLinks = `DELETE FROM links WHERE src = $1 AND dst <> ALL ($2::bigint[])`

	upsertLinks = `
		INSERT INTO links (src, dst, kind)
		SELECT $1, d, k FROM unnest($2::bigint[], $3::smallint[]) AS t (d, k)
		ON CONFLICT (src, dst) DO UPDATE SET kind = EXCLUDED.kind WHERE links.kind <> EXCLUDED.kind`

	// Links into old become links into keep; a source linking to both keeps one
	// link, a hyperlink if either was.
	moveIncoming = `
		INSERT INTO links (src, dst, kind)
		SELECT src, $2, kind FROM links WHERE dst = $1 AND src <> $2
		ON CONFLICT (src, dst) DO UPDATE SET kind = LEAST(links.kind, EXCLUDED.kind)`

	moveOutgoing = `
		INSERT INTO links (src, dst, kind)
		SELECT $2, dst, kind FROM links WHERE src = $1 AND dst <> $2
		ON CONFLICT (src, dst) DO UPDATE SET kind = LEAST(links.kind, EXCLUDED.kind)`

	repointAliases = `UPDATE aliases SET target_id = $2 WHERE target_id = $1`

	moveMembership = `
		INSERT INTO source_pages (source_id, page_id, added_at)
		SELECT source_id, $2, added_at FROM source_pages WHERE page_id = $1
		ON CONFLICT (source_id, page_id) DO NOTHING`

	// A run submitted for a lab's source brings the page into that lab; other
	// runs have no source, or no row at all, and add nothing.
	recordMembership = `
		INSERT INTO source_pages (source_id, page_id)
		SELECT source_id, $2 FROM runs WHERE id = $1 AND source_id IS NOT NULL
		ON CONFLICT (source_id, page_id) DO NOTHING`

	deletePage = `DELETE FROM pages WHERE id = $1`

	recordAlias = `
		INSERT INTO aliases (url, target_id) VALUES ($1, $2)
		ON CONFLICT (url) DO UPDATE SET target_id = EXCLUDED.target_id`
)

func savePage(ctx context.Context, tx pgx.Tx, page *data.PageData) error {
	var id int64

	if err := tx.QueryRow(ctx, upsertPage, page.URL, hostOf(page.URL), page.Status, page.Title, page.ContentType,
		page.LastRunID, page.FoundAt, page.LastUpdatedAt).Scan(&id); err != nil {
		return err
	}

	// The page was an alias until now: the redirect was reversed or removed.
	if _, err := tx.Exec(ctx, `DELETE FROM aliases WHERE url = $1`, page.URL); err != nil {
		return err
	}

	if err := replaceLinks(ctx, tx, id, page); err != nil {
		return err
	}

	if page.LastRunID != "" {
		if _, err := tx.Exec(ctx, recordMembership, page.LastRunID, id); err != nil {
			return err
		}
	}

	for _, alias := range page.Aliases {
		if alias == page.URL {
			continue
		}

		// The alias's own outgoing links described the same page, fetched earlier
		// under the other name; the links just saved supersede them.
		var oldID int64

		err := tx.QueryRow(ctx, `SELECT id FROM pages WHERE url = $1`, alias).Scan(&oldID)

		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			if err := fold(ctx, tx, oldID, id, false); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, recordAlias, alias, id); err != nil {
			return err
		}
	}

	return nil
}

func replaceLinks(ctx context.Context, tx pgx.Tx, id int64, page *data.PageData) error {
	kinds := make(map[string]int16, len(page.Links))

	for _, link := range page.Links {
		kinds[link] = kindResource
	}

	for _, link := range page.PageLinks {
		kinds[link] = kindHyperlink
	}

	if len(kinds) > 0 {
		resolved, err := resolveThroughAliases(ctx, tx, kinds)
		if err != nil {
			return err
		}

		kinds = resolved
	}

	delete(kinds, page.URL)

	targets := make([]string, 0, len(kinds))
	for link := range kinds {
		targets = append(targets, link)
	}

	// Concurrent savers insert overlapping placeholders; the same order everywhere
	// keeps them from waiting on each other in a cycle.
	sort.Strings(targets)

	ids := make([]int64, 0, len(targets))
	linkKinds := make([]int16, 0, len(targets))

	if len(targets) > 0 {
		hosts := make([]string, len(targets))
		for i, target := range targets {
			hosts[i] = hostOf(target)
		}

		if _, err := tx.Exec(ctx, insertPlaceholders, targets, hosts, page.FoundAt); err != nil {
			return err
		}

		rows, err := tx.Query(ctx, pageIDs, targets)
		if err != nil {
			return err
		}

		byURL := make(map[string]int64, len(targets))

		for rows.Next() {
			var (
				u      string
				target int64
			)

			if err := rows.Scan(&u, &target); err != nil {
				rows.Close()
				return err
			}

			byURL[u] = target
		}

		if err := rows.Err(); err != nil {
			return err
		}

		for _, target := range targets {
			ids = append(ids, byURL[target])
			linkKinds = append(linkKinds, kinds[target])
		}
	}

	if _, err := tx.Exec(ctx, dropStaleLinks, id, ids); err != nil {
		return err
	}

	if len(ids) == 0 {
		return nil
	}

	_, err := tx.Exec(ctx, upsertLinks, id, ids, linkKinds)

	return err
}

// resolveThroughAliases replaces each link to a known alias with a link to the
// page it names. A link that resolves to the same target as another keeps the
// stronger kind.
func resolveThroughAliases(ctx context.Context, tx pgx.Tx, kinds map[string]int16) (map[string]int16, error) {
	links := make([]string, 0, len(kinds))
	for link := range kinds {
		links = append(links, link)
	}

	rows, err := tx.Query(ctx, resolveAliases, links)
	if err != nil {
		return nil, err
	}

	targetOf := make(map[string]string)

	for rows.Next() {
		var alias, target string
		if err := rows.Scan(&alias, &target); err != nil {
			rows.Close()
			return nil, err
		}

		targetOf[alias] = target
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	resolved := make(map[string]int16, len(kinds))

	for link, kind := range kinds {
		if target, ok := targetOf[link]; ok {
			link = target
		}

		if prev, ok := resolved[link]; !ok || kind < prev {
			resolved[link] = kind
		}
	}

	return resolved, nil
}

// fold merges page row old into keep: links into old now point at keep, aliases
// of old name keep, its labs hold keep, and old is deleted. withOutgoing also gives keep old's
// outgoing links, for merging two stored spellings of one URL.
func fold(ctx context.Context, tx pgx.Tx, old, keep int64, withOutgoing bool) error {
	if old == keep {
		return nil
	}

	statements := []string{moveIncoming}
	if withOutgoing {
		statements = append(statements, moveOutgoing)
	}

	statements = append(statements, repointAliases, moveMembership)

	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement, old, keep); err != nil {
			return err
		}
	}

	_, err := tx.Exec(ctx, deletePage, old)

	return err
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}

	return u.Hostname()
}
