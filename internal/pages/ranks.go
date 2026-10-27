package pages

import (
	"context"
	"fmt"
	"time"
	"web-crawler/internal/rank"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GraphStore is the ranking side of the page graph: it hands the hyperlink graph
// to the ranker and serves the scores back.
type GraphStore struct {
	pool *pgxpool.Pool
}

func NewGraphStore(pool *pgxpool.Pool) *GraphStore {
	return &GraphStore{pool: pool}
}

// LinkGraph numbers pages and hosts densely from 0, in the order the ranker wants.
type LinkGraph struct {
	PageIDs []int64
	Hosts   []string
	HostOf  []int32
	Links   []rank.Edge
}

// LoadLinkGraph reads every page and every hyperlink in one snapshot, so a link
// never points at a page saved after the pages were read.
func (s *GraphStore) LoadLinkGraph(ctx context.Context) (*LinkGraph, error) {
	graph := &LinkGraph{}

	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		index, err := graph.loadPages(ctx, tx)
		if err != nil {
			return err
		}

		return graph.loadLinks(ctx, tx, index)
	})
	if err != nil {
		return nil, fmt.Errorf("loading the link graph: %w", err)
	}

	return graph, nil
}

func (g *LinkGraph) loadPages(ctx context.Context, tx pgx.Tx) (map[int64]int32, error) {
	rows, err := tx.Query(ctx, `SELECT id, host FROM pages ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	index := make(map[int64]int32)
	hostIndex := make(map[string]int32)

	for rows.Next() {
		var (
			id   int64
			host string
		)

		if err := rows.Scan(&id, &host); err != nil {
			return nil, err
		}

		h, ok := hostIndex[host]
		if !ok {
			h = int32(len(g.Hosts))
			hostIndex[host] = h
			g.Hosts = append(g.Hosts, host)
		}

		index[id] = int32(len(g.PageIDs))
		g.PageIDs = append(g.PageIDs, id)
		g.HostOf = append(g.HostOf, h)
	}

	return index, rows.Err()
}

func (g *LinkGraph) loadLinks(ctx context.Context, tx pgx.Tx, index map[int64]int32) error {
	rows, err := tx.Query(ctx, `SELECT src, dst FROM links WHERE kind = $1`, kindHyperlink)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var src, dst int64
		if err := rows.Scan(&src, &dst); err != nil {
			return err
		}

		g.Links = append(g.Links, rank.Edge{From: index[src], To: index[dst]})
	}

	return rows.Err()
}

type RankRun struct {
	Pages, Links, Hosts, HostLinks int
	Iterations                     int
	Residual                       float64
}

// SaveRanks replaces all scores in one transaction: readers see the previous run
// or this one, never a mix. A page merged away since the graph was loaded is
// skipped rather than failing the run.
func (s *GraphStore) SaveRanks(ctx context.Context, run RankRun, pageIDs []int64, pageScores []float64, hosts []string, hostScores []float64) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE new_page_ranks (page_id bigint, score double precision) ON COMMIT DROP`); err != nil {
			return err
		}

		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"new_page_ranks"}, []string{"page_id", "score"},
			pgx.CopyFromSlice(len(pageIDs), func(i int) ([]any, error) { return []any{pageIDs[i], pageScores[i]}, nil })); err != nil {
			return err
		}

		statements := []string{
			`DELETE FROM page_ranks`,
			`INSERT INTO page_ranks (page_id, score) SELECT n.page_id, n.score FROM new_page_ranks n JOIN pages p ON p.id = n.page_id`,
			`DELETE FROM host_ranks`,
		}

		for _, statement := range statements {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return err
			}
		}

		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"host_ranks"}, []string{"host", "score"},
			pgx.CopyFromSlice(len(hosts), func(i int) ([]any, error) { return []any{hosts[i], hostScores[i]}, nil })); err != nil {
			return err
		}

		_, err := tx.Exec(ctx, `
			INSERT INTO rank_runs (pages, links, hosts, host_links, iterations, residual)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			run.Pages, run.Links, run.Hosts, run.HostLinks, run.Iterations, run.Residual)

		return err
	})
}

// LookupRanks returns the scores of the given URLs. A URL missing from the result
// is not in the graph, or was added after the last ranking run.
func (s *GraphStore) LookupRanks(ctx context.Context, urls []string) (map[string]rank.Scores, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.url, r.score, coalesce(h.score, 0)
		FROM pages p
		JOIN page_ranks r ON r.page_id = p.id
		LEFT JOIN host_ranks h ON h.host = p.host
		WHERE p.url = ANY($1)`, urls)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]rank.Scores, len(urls))

	for rows.Next() {
		var (
			u     string
			ranks rank.Scores
		)

		if err := rows.Scan(&u, &ranks.Page, &ranks.Site); err != nil {
			return nil, err
		}

		out[u] = ranks
	}

	return out, rows.Err()
}

// Twin is a page never fetched whose URL differs from fetched pages' only by
// scheme or a trailing slash. Links to it most likely mean one of those pages,
// but only the server can say which, if any.
type Twin struct {
	Placeholder string
	Fetched     []string
}

// PlaceholderTwins lists the twins nobody has asked the server about yet.
func (s *GraphStore) PlaceholderTwins(ctx context.Context, limit int) ([]Twin, error) {
	rows, err := s.pool.Query(ctx, `
		WITH k AS (
			SELECT id, url, status, identity_checked_at,
			       host || regexp_replace(regexp_replace(url, '^https?://[^/]+', ''), '/(\?|$)', '\1') AS key
			FROM pages
		)
		SELECT p.url, array_agg(c.url ORDER BY c.url)
		FROM k p JOIN k c ON c.key = p.key AND c.id <> p.id
		WHERE p.status IS NULL AND c.status IS NOT NULL AND p.identity_checked_at IS NULL
		GROUP BY p.url
		ORDER BY p.url
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Twin, error) {
		var twin Twin
		err := row.Scan(&twin.Placeholder, &twin.Fetched)

		return twin, err
	})
}

// ResolveTwin records the server's answer about a placeholder. A different
// identity makes the placeholder an alias of that page - created if new - and its
// incoming links move there; the same identity only marks it as checked.
func (s *GraphStore) ResolveTwin(ctx context.Context, placeholder, identity string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if identity == "" || identity == placeholder {
			_, err := tx.Exec(ctx, `UPDATE pages SET identity_checked_at = now() WHERE url = $1`, placeholder)
			return err
		}

		if _, err := tx.Exec(ctx, insertPlaceholders, []string{identity}, []string{hostOf(identity)}, time.Now()); err != nil {
			return err
		}

		var (
			oldID    *int64
			targetID int64
		)

		// The placeholder may have been crawled meanwhile, and then the crawler
		// already knows its identity better.
		if err := tx.QueryRow(ctx, `
			SELECT (SELECT id FROM pages WHERE url = $1 AND status IS NULL), (SELECT id FROM pages WHERE url = $2)`,
			placeholder, identity).Scan(&oldID, &targetID); err != nil {
			return err
		}

		if oldID == nil {
			return nil
		}

		if err := fold(ctx, tx, *oldID, targetID, false); err != nil {
			return err
		}

		_, err := tx.Exec(ctx, recordAlias, placeholder, targetID)

		return err
	})
}
