package facts

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// PageFacts is the live facts of the pages given, as an answer reads them:
// each with how many pages among `among` state it (direct restatements, itself
// included), which facts there it is the same as, and which contradict it.
func (s *PostgresStore) PageFacts(ctx context.Context, urls, among []string) ([]LabFact, error) {
	rows, err := s.pool.Query(ctx, `
		WITH mine AS (SELECT * FROM facts WHERE stale_at IS NULL AND url = ANY($1)),
		linked AS (
			SELECT m.id AS fact, CASE WHEN l.a = m.id THEN l.b ELSE l.a END AS other, l.relation
			FROM mine m JOIN fact_links l ON (l.a = m.id OR l.b = m.id) AND l.relation <> 'different'),
		live AS (
			SELECT k.fact, k.other, k.relation, o.url FROM linked k JOIN facts o ON o.id = k.other
			WHERE o.stale_at IS NULL AND o.url = ANY($2))
		SELECT m.id, m.claim, m.about, m.quote, m.url, m.ordinal,
			1 + (SELECT count(DISTINCT v.url) FROM live v WHERE v.fact = m.id AND v.relation = 'same' AND v.url <> m.url),
			coalesce((SELECT array_agg(v.other::text) FROM live v WHERE v.fact = m.id AND v.relation = 'same'), '{}'),
			coalesce((SELECT array_agg(v.other::text) FROM live v WHERE v.fact = m.id AND v.relation = 'contradict'), '{}')
		FROM mine m ORDER BY m.url, m.ordinal, m.id`, urls, among)
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (LabFact, error) {
		var f LabFact
		err := row.Scan(&f.ID, &f.Claim, &f.About, &f.Quote, &f.URL, &f.Ordinal, &f.Agree, &f.Same, &f.Contested)
		f.Group = f.ID

		return f, err
	})
}

// AnswerLedger is the ledger as answers use it: facts to read, and the
// digester to ask for the passages that have none yet.
type AnswerLedger struct {
	store    *PostgresStore
	digester *Digester
}

// NewAnswerLedger takes a nil digester where none runs: answers read the
// facts there are, and nothing is digested for them.
func NewAnswerLedger(store *PostgresStore, digester *Digester) *AnswerLedger {
	return &AnswerLedger{store: store, digester: digester}
}

func (l *AnswerLedger) PageFacts(ctx context.Context, urls, among []string) ([]LabFact, error) {
	return l.store.PageFacts(ctx, urls, among)
}

func (l *AnswerLedger) WantPassages(url string, passages []string) {
	if l.digester != nil {
		l.digester.WantPassages(url, passages)
	}
}
