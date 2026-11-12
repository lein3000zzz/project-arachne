package research

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"web-crawler/internal/copies"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (s *PostgresStore) StartRun(ctx context.Context, labID, request string, budget Budget, focus Focus, model string) (string, error) {
	var id string

	err := s.pool.QueryRow(ctx, `
		INSERT INTO research_runs (lab_id, request, found_budget, read_budget, focus_work, focus_subtopic, status, model)
		VALUES ($1, $2, $3, $4, nullif($5, '')::uuid, nullif($6, '')::uuid, 'running', $7)
		RETURNING id`, labID, request, budget.Found, budget.Read, focus.Work, focus.Subtopic, model).Scan(&id)
	if isUniqueViolation(err) {
		return "", ErrRunning
	}

	return id, err
}

func (s *PostgresStore) SaveRounds(ctx context.Context, id string, rounds []Round) error {
	// A run that ended before its first round has none: an empty list, never null.
	if rounds == nil {
		rounds = []Round{}
	}

	raw, err := json.Marshal(rounds)
	if err != nil {
		return err
	}

	_, err = s.pool.Exec(ctx, `UPDATE research_runs SET rounds = $2 WHERE id = $1`, id, raw)

	return err
}

func (s *PostgresStore) FinishRun(ctx context.Context, id string, status, stopped, errText string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE research_runs SET status = $2, stopped = $3, error = $4, finished_at = now() WHERE id = $1`, id, status, stopped, errText)

	return err
}

const runColumns = `id, lab_id, request, found_budget, read_budget, coalesce(focus_work::text, ''), coalesce(focus_subtopic::text, ''),
	status, stopped, rounds, error, model, started_at, finished_at`

func scanRun(row pgx.CollectableRow) (Run, error) {
	var (
		r      Run
		rounds []byte
	)

	err := row.Scan(&r.ID, &r.LabID, &r.Request, &r.Budget.Found, &r.Budget.Read, &r.Focus.Work, &r.Focus.Subtopic, &r.Status, &r.Stopped,
		&rounds, &r.Error, &r.Model, &r.StartedAt, &r.FinishedAt)
	if err != nil {
		return Run{}, err
	}

	if err := json.Unmarshal(rounds, &r.Rounds); err != nil {
		return Run{}, err
	}

	// Runs saved as null before that was fixed.
	if r.Rounds == nil {
		r.Rounds = []Round{}
	}

	return r, nil
}

func (s *PostgresStore) Run(ctx context.Context, id string) (Run, error) {
	return s.oneRun(ctx, `SELECT `+runColumns+` FROM research_runs WHERE id = $1`, id)
}

func (s *PostgresStore) LatestRun(ctx context.Context, labID string) (Run, error) {
	return s.oneRun(ctx, `SELECT `+runColumns+` FROM research_runs WHERE lab_id = $1 ORDER BY started_at DESC, id DESC LIMIT 1`, labID)
}

func (s *PostgresStore) oneRun(ctx context.Context, query, arg string) (Run, error) {
	rows, err := s.pool.Query(ctx, query, arg)
	if err != nil {
		return Run{}, err
	}

	r, err := pgx.CollectExactlyOneRow(rows, scanRun)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}

	return r, err
}

func (s *PostgresStore) Interrupt(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE research_runs SET status = 'failed', stopped = 'interrupted', error = 'interrupted by a restart', finished_at = now()
		WHERE status = 'running'`)

	return tag.RowsAffected(), err
}

func (s *PostgresStore) Subtopics(ctx context.Context, labID string) ([]Subtopic, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, focus, queries FROM lab_subtopics WHERE lab_id = $1 ORDER BY position, id`, labID)
	if err != nil {
		return nil, err
	}

	subs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Subtopic, error) {
		var sub Subtopic
		err := row.Scan(&sub.ID, &sub.Name, &sub.Focus, &sub.Queries)

		return sub, err
	})
	if subs == nil {
		subs = []Subtopic{}
	}

	return subs, err
}

func (s *PostgresStore) AddSubtopics(ctx context.Context, labID string, subs []Subtopic) ([]Subtopic, error) {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var next int
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(position) + 1, 0) FROM lab_subtopics WHERE lab_id = $1`, labID).Scan(&next); err != nil {
			return err
		}

		for i, sub := range subs {
			queries := sub.Queries
			if queries == nil {
				queries = []string{}
			}

			_, err := tx.Exec(ctx, `
				INSERT INTO lab_subtopics (lab_id, position, name, focus, queries) VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT DO NOTHING`, labID, next+i, sub.Name, sub.Focus, queries)
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return s.Subtopics(ctx, labID)
}

func (s *PostgresStore) RenameSubtopic(ctx context.Context, labID, subtopicID, name, focus string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE lab_subtopics SET name = $3, focus = $4 WHERE id = $2 AND lab_id = $1`, labID, subtopicID, name, focus)
	if isUniqueViolation(err) {
		return ErrSubtopicTaken
	}

	if err == nil && tag.RowsAffected() == 0 {
		return ErrSubtopicNotFound
	}

	return err
}

func (s *PostgresStore) MergeSubtopics(ctx context.Context, labID, from, into string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var found int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM lab_subtopics WHERE lab_id = $1 AND id IN ($2, $3)`, labID, from, into).Scan(&found); err != nil {
			return err
		}

		if found != 2 {
			return ErrSubtopicNotFound
		}

		steps := []struct {
			sql  string
			args []any
		}{
			{`UPDATE works SET subtopic_id = $3 WHERE lab_id = $1 AND subtopic_id = $2`, []any{labID, from, into}},
			{`UPDATE research_runs SET focus_subtopic = $3 WHERE lab_id = $1 AND focus_subtopic = $2`, []any{labID, from, into}},
			// Its searches go with it, so researching the merged subtopic
			// further searches both.
			{`UPDATE lab_subtopics i SET queries = ARRAY(SELECT DISTINCT unnest(i.queries || f.queries))
				FROM lab_subtopics f WHERE i.id = $3 AND f.id = $2 AND i.lab_id = $1`, []any{labID, from, into}},
			{`DELETE FROM lab_subtopics WHERE lab_id = $1 AND id = $2`, []any{labID, from}},
		}

		for _, step := range steps {
			if _, err := tx.Exec(ctx, step.sql, step.args...); err != nil {
				return err
			}
		}

		return nil
	})
}

func (s *PostgresStore) Find(ctx context.Context, labID, runID string, w Work, f Find) (string, bool, error) {
	var (
		id    string
		added bool
	)

	verdict := w.Verdict
	if verdict == "" {
		verdict = VerdictPending
	}

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT id FROM works
			WHERE lab_id = $1 AND (url = $2 OR ($3 <> '' AND doi = $3) OR ($4 <> '' AND openalex_id = $4))
			ORDER BY (url = $2) DESC, id LIMIT 1
			FOR UPDATE`, labID, w.URL, w.DOI, w.OpenAlexID).Scan(&id)

		switch {
		case errors.Is(err, pgx.ErrNoRows):
			added = true
			err = tx.QueryRow(ctx, `
				INSERT INTO works (lab_id, url, kind, title, snippet, site, year, doi, openalex_id, cited_by, verdict, source_id, first_run, first_round)
				VALUES ($1, $2, $3, $4, $5, $6, nullif($7, 0), $8, $9, $10, $11, nullif($12, '')::uuid, nullif($13, '')::uuid, $14)
				RETURNING id`,
				labID, w.URL, w.Kind, w.Title, w.Snippet, w.Site, w.Year, w.DOI, w.OpenAlexID, w.CitedBy, verdict, w.SourceID, runID, f.Round).Scan(&id)
		case err == nil:
			// Fills in what the work lacked; a DOI or ID another work holds
			// stays with it.
			_, err = tx.Exec(ctx, `
				UPDATE works SET
					kind = CASE WHEN $2 = 'paper' THEN 'paper' ELSE kind END,
					title = CASE WHEN title = '' THEN $3 ELSE title END,
					snippet = CASE WHEN snippet = '' THEN $4 ELSE snippet END,
					site = CASE WHEN site = '' THEN $5 ELSE site END,
					year = coalesce(year, nullif($6, 0)),
					cited_by = coalesce($9, cited_by),
					doi = CASE WHEN doi = '' AND $7 <> '' AND NOT EXISTS (SELECT 1 FROM works o WHERE o.lab_id = works.lab_id AND o.doi = $7)
						THEN $7 ELSE doi END,
					openalex_id = CASE WHEN openalex_id = '' AND $8 <> ''
						AND NOT EXISTS (SELECT 1 FROM works o WHERE o.lab_id = works.lab_id AND o.openalex_id = $8) THEN $8 ELSE openalex_id END
				WHERE id = $1`,
				id, w.Kind, w.Title, w.Snippet, w.Site, w.Year, w.DOI, w.OpenAlexID, w.CitedBy)
		}

		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO work_finds (work_id, channel, query, via_work, run_id, round) VALUES ($1, $2, $3, nullif($4, '')::uuid, nullif($5, '')::uuid, $6)
			ON CONFLICT DO NOTHING`, id, f.Channel, f.Query, f.Via, runID, f.Round)

		return err
	})

	return id, added, err
}

// Works reads every work of the lab with its finds. A work read says how far
// it has come: its crawl's progress and its pages' review.
func (s *PostgresStore) Works(ctx context.Context, labID string) ([]Work, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT w.id, w.url, w.kind, w.title, w.snippet, w.site, coalesce(w.year, 0), w.doi, w.openalex_id, w.cited_by,
		       coalesce(w.subtopic_id::text, ''), w.verdict, w.reason, w.by_user, coalesce(w.source_id::text, ''), w.references_done, w.citing_done,
		       CASE
		           WHEN w.source_id IS NULL THEN ''
		           WHEN rev.indexed THEN 'in'
		           WHEN rev.skipped THEN 'set_aside'
		           WHEN run.submitted_at IS NULL THEN 'failed'
		           WHEN (run.finished_at IS NULL OR run.docs_queued > run.docs_indexed + run.docs_failed + run.docs_rejected)
		                AND run.submitted_at > now() - interval '1 hour' THEN 'reading'
		           WHEN run.docs_indexed > 0 THEN 'in'
		           WHEN run.docs_failed > 0 THEN 'failed'
		           WHEN run.note LIKE 'robots.txt%' THEN 'blocked'
		           WHEN rev.ok = 0 AND rev.refused > 0 THEN 'blocked'
		           WHEN rev.ok = 0 AND rev.gone > 0 THEN 'gone'
		           WHEN rev.ok > 0 THEN 'empty'
		           ELSE 'failed'
		       END,
		       coalesce((SELECT json_agg(json_build_object('channel', f.channel, 'query', f.query, 'via', coalesce(f.via_work::text, ''), 'round', f.round,
		                                                   'run', coalesce(f.run_id::text, ''))
		                                 ORDER BY f.id)
		                 FROM work_finds f WHERE f.work_id = w.id), '[]')
		FROM works w
		LEFT JOIN LATERAL (
			SELECT r.submitted_at, r.finished_at, r.docs_queued, r.docs_indexed, r.docs_failed, r.docs_rejected, r.note
			FROM runs r WHERE r.source_id = w.source_id ORDER BY r.submitted_at DESC LIMIT 1) run ON true
		LEFT JOIN LATERAL (
			SELECT coalesce(bool_or(pr.decision = 'index'), false) AS indexed, coalesce(bool_or(pr.decision = 'skip'), false) AS skipped,
			       count(*) FILTER (WHERE p.status BETWEEN 200 AND 299) AS ok,
			       count(*) FILTER (WHERE p.status IN (401, 403, 429, 451)) AS refused,
			       count(*) FILTER (WHERE p.status IN (404, 410)) AS gone
			FROM source_pages sp JOIN pages p ON p.id = sp.page_id LEFT JOIN page_reviews pr ON pr.url = p.url
			WHERE sp.source_id = w.source_id) rev ON true
		WHERE w.lab_id = $1
		ORDER BY w.id`, labID)
	if err != nil {
		return nil, err
	}

	works, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Work, error) {
		var (
			w     Work
			finds []byte
		)

		err := row.Scan(&w.ID, &w.URL, &w.Kind, &w.Title, &w.Snippet, &w.Site, &w.Year, &w.DOI, &w.OpenAlexID, &w.CitedBy,
			&w.SubtopicID, &w.Verdict, &w.Reason, &w.ByUser, &w.SourceID, &w.ReferencesDone, &w.CitingDone, &w.Read, &finds)
		if err != nil {
			return Work{}, err
		}

		return w, json.Unmarshal(finds, &w.Finds)
	})
	if err != nil {
		return nil, err
	}

	if works == nil {
		works = []Work{}
	}

	if slices.ContainsFunc(works, func(w Work) bool { return w.Read == ReadIn }) {
		pages, err := s.labPages(ctx, labID)
		if err != nil {
			return nil, err
		}

		markCopies(works, pages)
	}

	return works, nil
}

// labPages are the lab's pages kept on review, by the source that brought
// each in, oldest source first.
func (s *PostgresStore) labPages(ctx context.Context, labID string) ([]LabPage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sp.source_id, p.url, pr.content_hash, pr.sketch
		FROM sources src
		JOIN source_pages sp ON sp.source_id = src.id
		JOIN pages p ON p.id = sp.page_id
		JOIN page_reviews pr ON pr.url = p.url
		WHERE src.lab_id = $1 AND pr.decision = 'index'
		ORDER BY sp.source_id, p.url`, labID)
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (LabPage, error) {
		var (
			p      LabPage
			sketch []byte
		)

		err := row.Scan(&p.SourceID, &p.URL, &p.Hash, &sketch)
		p.Sketch = copies.SketchFrom(sketch)

		return p, err
	})
}

func (s *PostgresStore) Judge(ctx context.Context, workID, verdict, reason, subtopicID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE works SET verdict = $2, reason = $3, subtopic_id = coalesce(nullif($4, '')::uuid, subtopic_id)
		WHERE id = $1 AND NOT by_user`, workID, verdict, reason, subtopicID)

	return err
}

func (s *PostgresStore) SetVerdict(ctx context.Context, labID, workID, verdict, subtopicID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE works SET verdict = $3, by_user = true, subtopic_id = coalesce(nullif($4, '')::uuid, subtopic_id)
		WHERE id = $2 AND lab_id = $1`, labID, workID, verdict, subtopicID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrWorkNotFound
	}

	return err
}

func (s *PostgresStore) SetSource(ctx context.Context, workID, sourceID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE works SET source_id = $2 WHERE id = $1`, workID, sourceID)
	return err
}

func (s *PostgresStore) Expanded(ctx context.Context, workID string, references, citing bool) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE works SET references_done = references_done OR $2, citing_done = citing_done OR $3 WHERE id = $1`, workID, references, citing)

	return err
}

func (s *PostgresStore) Link(ctx context.Context, labID string, l Link) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO work_links (lab_id, from_work, to_work) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, labID, l.From, l.To)

	return err
}

func (s *PostgresStore) Links(ctx context.Context, labID string) ([]Link, error) {
	rows, err := s.pool.Query(ctx, `SELECT from_work, to_work FROM work_links WHERE lab_id = $1 ORDER BY from_work, to_work`, labID)
	if err != nil {
		return nil, err
	}

	links, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Link, error) {
		var l Link
		err := row.Scan(&l.From, &l.To)

		return l, err
	})
	if links == nil {
		links = []Link{}
	}

	return links, err
}

func (s *PostgresStore) ReferencesOf(ctx context.Context, workIDs []string) ([]Reference, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT w.id, r.url, r.label
		FROM works w
		JOIN source_pages sp ON sp.source_id = w.source_id
		JOIN page_references r ON r.page_id = sp.page_id
		JOIN pages p ON p.id = sp.page_id
		LEFT JOIN page_reviews v ON v.url = p.url
		WHERE w.id = ANY($1) AND v.decision IS DISTINCT FROM 'skip'
		ORDER BY w.id, r.position`, workIDs)
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Reference, error) {
		var r Reference
		err := row.Scan(&r.Work, &r.URL, &r.Label)

		return r, err
	})
}
