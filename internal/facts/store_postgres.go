package facts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Due is a lab page whose reviewed version has not been digested in full.
type Due struct {
	URL        string
	ReviewHash string
	ReviewedAt time.Time
}

// ChunkText is one chunk of a page's indexed version.
type ChunkText struct {
	Ordinal int
	Text    string
}

// Digested is what one chunk gave.
type Digested struct {
	Ordinal  int
	TextHash string
	Facts    []Fact
	Rejected int
	Seconds  float64
	Model    string
	Error    string
}

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// TextHash names a chunk's text, white space aside.
func TextHash(text string) string {
	sum := sha256.Sum256([]byte(Normalize(text)))
	return hex.EncodeToString(sum[:16])
}

// Due lists lab pages, kept on review, whose reviewed version is not the one
// digested or whose planned chunks are not all done; oldest review first.
func (s *PostgresStore) Due(ctx context.Context, limit int) ([]Due, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.url, r.content_hash, r.reviewed_at FROM page_reviews r
		JOIN pages p ON p.url = r.url
		LEFT JOIN fact_pages f ON f.url = r.url
		WHERE r.decision = 'index'
		  AND EXISTS (SELECT 1 FROM source_pages sp WHERE sp.page_id = p.id)
		  AND (f.url IS NULL OR f.review_hash <> r.content_hash
		       OR NOT f.planned <@ coalesce((SELECT array_agg(c.ordinal) FROM fact_chunks c
		                                     WHERE c.url = f.url AND c.index_hash = f.index_hash), '{}'))
		ORDER BY r.reviewed_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Due, error) {
		var d Due
		err := row.Scan(&d.URL, &d.ReviewHash, &d.ReviewedAt)

		return d, err
	})
}

// Version records the indexed version of a page about to be digested.
// Facts of an earlier version whose quote the page still holds carry over,
// to the chunk that holds it; the rest go stale. A chunk with the same text
// as one digested before is not digested again: its record carries over.
func (s *PostgresStore) Version(ctx context.Context, url, reviewHash, indexHash string, planned []int, chunks []ChunkText) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM fact_pages WHERE url = $1 FOR UPDATE`, url); err != nil {
			return err
		}

		rows, err := tx.Query(ctx, `SELECT id, quote FROM facts WHERE url = $1 AND stale_at IS NULL AND index_hash <> $2`, url, indexHash)
		if err != nil {
			return err
		}

		type old struct{ id, quote string }

		olds, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (old, error) {
			var o old
			err := row.Scan(&o.id, &o.quote)

			return o, err
		})
		if err != nil {
			return err
		}

		texts := make([]string, len(chunks))
		for i, c := range chunks {
			texts[i] = Normalize(c.Text)
		}

		for _, o := range olds {
			at := -1

			for i := range chunks {
				if strings.Contains(texts[i], o.quote) {
					at = chunks[i].Ordinal
					break
				}
			}

			if at < 0 {
				_, err = tx.Exec(ctx, `UPDATE facts SET stale_at = now() WHERE id = $1`, o.id)
			} else {
				_, err = tx.Exec(ctx, `UPDATE facts SET index_hash = $2, ordinal = $3 WHERE id = $1`, o.id, indexHash, at)
			}

			if err != nil {
				return err
			}
		}

		for _, c := range chunks {
			if _, err := tx.Exec(ctx, `
				INSERT INTO fact_chunks (url, index_hash, ordinal, text_hash, facts, rejected, seconds, model, error)
				SELECT url, $2, $3, text_hash, facts, rejected, 0, 'carried over', error FROM fact_chunks
				WHERE url = $1 AND text_hash = $4 AND index_hash <> $2
				ORDER BY digested_at DESC LIMIT 1
				ON CONFLICT DO NOTHING`, url, indexHash, c.Ordinal, TextHash(c.Text)); err != nil {
				return err
			}
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO fact_pages (url, review_hash, index_hash, planned) VALUES ($1, $2, $3, $4)
			ON CONFLICT (url) DO UPDATE SET review_hash = $2, index_hash = $3, planned = $4, updated_at = now()`,
			url, reviewHash, indexHash, planned)

		return err
	})
}

// Undigested is those of the ordinals the version has no record of.
func (s *PostgresStore) Undigested(ctx context.Context, url, indexHash string, ordinals []int) ([]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT o FROM unnest($3::integer[]) o
		WHERE NOT EXISTS (SELECT 1 FROM fact_chunks c WHERE c.url = $1 AND c.index_hash = $2 AND c.ordinal = o)
		ORDER BY o`, url, indexHash, ordinals)
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, pgx.RowTo[int])
}

// SaveChunk records a digested chunk and its facts; a fact whose quote the
// page has a live fact for already - carried over from an earlier version -
// is not added twice. Saving the same chunk again changes nothing.
func (s *PostgresStore) SaveChunk(ctx context.Context, url, indexHash string, d Digested) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO fact_chunks (url, index_hash, ordinal, text_hash, facts, rejected, seconds, model, error)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT DO NOTHING`,
			url, indexHash, d.Ordinal, d.TextHash, len(d.Facts), d.Rejected, d.Seconds, d.Model, d.Error)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}

		for _, f := range d.Facts {
			if _, err := tx.Exec(ctx, `
				INSERT INTO facts (url, index_hash, ordinal, claim, about, quote, model)
				SELECT $1, $2, $3, $4, $5, $6, $7
				WHERE NOT EXISTS (SELECT 1 FROM facts WHERE url = $1 AND quote = $6 AND stale_at IS NULL)`,
				url, indexHash, d.Ordinal, f.Claim, f.About, f.Quote, d.Model); err != nil {
				return err
			}
		}

		return nil
	})
}

// Any constant nothing else locks on: one digester at a time per database,
// however many search servers run.
const digestLockKey = 0x66616374

// Lead holds the digesting lock until release; ok is false when another
// process holds it.
func (s *PostgresStore) Lead(ctx context.Context) (release func(), ok bool, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}

	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, digestLockKey).Scan(&ok); err != nil || !ok {
		conn.Release()
		return nil, false, err
	}

	return func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, digestLockKey)
		conn.Release()
	}, true, nil
}
