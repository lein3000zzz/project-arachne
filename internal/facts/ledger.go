package facts

import (
	"context"
	"errors"
	"time"
	"web-crawler/internal/labs"

	"github.com/jackc/pgx/v5"
)

// LabFact is a fact of one of a lab's pages, as the lab shows it.
type LabFact struct {
	ID      string `json:"id"`
	Claim   string `json:"claim"`
	About   string `json:"about"`
	Quote   string `json:"quote"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	Ordinal int    `json:"ordinal"`
	Stale   bool   `json:"stale"`
}

// Ledger is a lab's facts and how far digesting its pages has come.
type Ledger struct {
	Facts []LabFact `json:"facts"`
	// The lab's pages kept on review, and those digested as far as planned.
	Pages    int `json:"pages"`
	Digested int `json:"digested"`
	Chunks   int `json:"chunks"`
	// Offered by the model and not borne out by the sentences it named.
	Rejected int     `json:"rejected"`
	Seconds  float64 `json:"seconds_per_chunk"`
}

// Most facts a lab shows at once.
const maxLabFacts = 5000

// Ledger is the lab's facts, its owner's alone.
func (s *PostgresStore) Ledger(ctx context.Context, ownerID, labID string) (Ledger, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM labs WHERE id = $1 AND owner_id = $2)`, labID, ownerID).Scan(&exists); err != nil {
		return Ledger{}, err
	}

	if !exists {
		return Ledger{}, labs.ErrNotFound
	}

	const labPages = `
		SELECT DISTINCT p.url, coalesce(p.title, '') AS title FROM pages p
		JOIN source_pages sp ON sp.page_id = p.id
		JOIN sources s ON s.id = sp.source_id
		WHERE s.lab_id = $1`

	rows, err := s.pool.Query(ctx, `
		WITH lp AS (`+labPages+`)
		SELECT f.id, f.claim, f.about, f.quote, f.url, lp.title, f.ordinal, f.stale_at IS NOT NULL
		FROM facts f JOIN lp ON lp.url = f.url
		ORDER BY f.stale_at IS NOT NULL, f.url, f.ordinal, f.id
		LIMIT $2`, labID, maxLabFacts)
	if err != nil {
		return Ledger{}, err
	}

	l := Ledger{}

	l.Facts, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (LabFact, error) {
		var f LabFact
		err := row.Scan(&f.ID, &f.Claim, &f.About, &f.Quote, &f.URL, &f.Title, &f.Ordinal, &f.Stale)

		return f, err
	})
	if err != nil {
		return Ledger{}, err
	}

	var seconds *float64

	err = s.pool.QueryRow(ctx, `
		WITH lp AS (`+labPages+`),
		kept AS (SELECT lp.url FROM lp JOIN page_reviews r ON r.url = lp.url AND r.decision = 'index'),
		done AS (
			SELECT f.url FROM fact_pages f JOIN kept USING (url) JOIN page_reviews r ON r.url = f.url AND r.content_hash = f.review_hash
			WHERE f.planned <@ coalesce((SELECT array_agg(c.ordinal) FROM fact_chunks c WHERE c.url = f.url AND c.index_hash = f.index_hash), '{}')),
		chunks AS (
			SELECT c.* FROM fact_chunks c JOIN fact_pages f ON f.url = c.url AND f.index_hash = c.index_hash JOIN kept ON kept.url = c.url)
		SELECT (SELECT count(*) FROM kept), (SELECT count(*) FROM done), (SELECT count(*) FROM chunks),
			(SELECT coalesce(sum(rejected), 0) FROM chunks), (SELECT avg(seconds) FROM chunks WHERE model <> 'carried over')`,
		labID).Scan(&l.Pages, &l.Digested, &l.Chunks, &l.Rejected, &seconds)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Ledger{}, err
	}

	if seconds != nil {
		l.Seconds = *seconds
	}

	return l, nil
}

// LedgerService gives labs their ledgers.
type LedgerService struct {
	store *PostgresStore
}

func NewLedgerService(store *PostgresStore) *LedgerService {
	return &LedgerService{store: store}
}

func (s *LedgerService) Ledger(ctx context.Context, ownerID, labID string) (Ledger, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	return s.store.Ledger(ctx, ownerID, labID)
}
