package facts

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// labFacts is a lab's live facts embedded by the model given.
const labFacts = `
	SELECT DISTINCT s.lab_id, f.id, f.url, f.claim, f.embedding FROM facts f
	JOIN pages p ON p.url = f.url
	JOIN source_pages sp ON sp.page_id = p.id
	JOIN sources s ON s.id = sp.source_id
	WHERE f.stale_at IS NULL AND f.embed_model = $1 AND f.embedding IS NOT NULL`

func (s *PostgresStore) Unembedded(ctx context.Context, model string, limit int) ([]Claim, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, url, claim FROM facts WHERE stale_at IS NULL AND (embedding IS NULL OR embed_model <> $1)
		ORDER BY id LIMIT $2`, model, limit)
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Claim, error) {
		var c Claim
		err := row.Scan(&c.ID, &c.URL, &c.Text)

		return c, err
	})
}

func (s *PostgresStore) SaveEmbeddings(ctx context.Context, model string, claims []Claim) error {
	batch := &pgx.Batch{}
	for _, c := range claims {
		batch.Queue(`UPDATE facts SET embedding = $2, embed_model = $3 WHERE id = $1`, c.ID, c.Vector, model)
	}

	return s.pool.SendBatch(ctx, batch).Close()
}

func (s *PostgresStore) LabsToRelate(ctx context.Context, model string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `
		WITH lf AS (`+labFacts+`),
		now AS (SELECT lab_id, count(DISTINCT id) || ':' || max(id::text) AS signature FROM lf GROUP BY lab_id)
		SELECT now.lab_id, now.signature FROM now
		LEFT JOIN fact_lab_state st ON st.lab_id = now.lab_id
		WHERE st.signature IS DISTINCT FROM now.signature`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}

	for rows.Next() {
		var lab, signature string
		if err := rows.Scan(&lab, &signature); err != nil {
			return nil, err
		}

		out[lab] = signature
	}

	return out, rows.Err()
}

func (s *PostgresStore) LabClaims(ctx context.Context, labID, model string) ([]Claim, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, url, claim, embedding FROM (`+labFacts+`) lf WHERE lab_id = $2 ORDER BY id`, model, labID)
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Claim, error) {
		var c Claim
		err := row.Scan(&c.ID, &c.URL, &c.Text, &c.Vector)

		return c, err
	})
}

func (s *PostgresStore) Compared(ctx context.Context, ids []string) (map[[2]string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT a, b FROM fact_links WHERE a = ANY($1) AND b = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[[2]string]bool{}

	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}

		out[[2]string{a, b}] = true
	}

	return out, rows.Err()
}

func (s *PostgresStore) SaveLink(ctx context.Context, a, b, relation string, similarity float64, model string) error {
	k := key(a, b)

	_, err := s.pool.Exec(ctx, `
		INSERT INTO fact_links (a, b, relation, similarity, model) VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`,
		k[0], k[1], relation, similarity, model)

	return err
}

func (s *PostgresStore) SetLabState(ctx context.Context, labID, signature string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO fact_lab_state (lab_id, signature) VALUES ($1, $2)
		ON CONFLICT (lab_id) DO UPDATE SET signature = $2, related_at = now()`, labID, signature)

	return err
}
