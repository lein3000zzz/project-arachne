package labs

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Every call names the owner, and every query checks it: a lab ID alone gets
// nobody into another user's lab.
type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const uniqueViolation = "23505"

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}

const (
	labColumns = `
		l.id, l.name, l.topic, l.created_at, l.updated_at,
		(SELECT count(*) FROM sources s WHERE s.lab_id = l.id),
		(SELECT count(DISTINCT sp.page_id) FROM sources s
			JOIN source_pages sp ON sp.source_id = s.id
			JOIN pages p ON p.id = sp.page_id
			WHERE s.lab_id = l.id AND p.status BETWEEN 200 AND 299),
		(SELECT count(DISTINCT sp.page_id) FROM sources s
			JOIN source_pages sp ON sp.source_id = s.id
			JOIN pages p ON p.id = sp.page_id
			JOIN page_reviews r ON r.url = p.url
			WHERE s.lab_id = l.id AND p.status BETWEEN 200 AND 299 AND r.decision = 'skip'),
		l.engines`

	sourceColumns = `
		s.id, s.lab_id, s.kind, s.url, s.scope, s.max_depth, s.max_links, s.created_at, s.query, s.engines, s.reason,
		coalesce(s.gathering_id, 0),
		(SELECT count(*) FROM source_pages sp JOIN pages p ON p.id = sp.page_id
			WHERE sp.source_id = s.id AND p.status BETWEEN 200 AND 299),
		coalesce((SELECT r.id FROM runs r WHERE r.source_id = s.id ORDER BY r.submitted_at DESC LIMIT 1), '')`
)

func scanLab(row pgx.CollectableRow) (Lab, error) {
	var l Lab
	err := row.Scan(&l.ID, &l.Name, &l.Topic, &l.CreatedAt, &l.UpdatedAt, &l.Sources, &l.Pages, &l.SetAside, &l.Engines)

	return l, err
}

func scanSource(row pgx.CollectableRow) (Source, error) {
	var s Source
	err := row.Scan(&s.ID, &s.LabID, &s.Kind, &s.URL, &s.Scope, &s.MaxDepth, &s.MaxLinks, &s.CreatedAt, &s.Query, &s.Engines, &s.Reason,
		&s.GatheringID, &s.Pages, &s.LastRunID)

	return s, err
}

func one[T any](rows pgx.Rows, err error, scan pgx.RowToFunc[T], missing error) (T, error) {
	if err != nil {
		var zero T
		return zero, err
	}

	v, err := pgx.CollectExactlyOneRow(rows, scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, missing
	}

	return v, err
}

func (s *PostgresStore) Create(ctx context.Context, ownerID int64, name, topic string, engines []string) (Lab, error) {
	var id int64

	err := s.pool.QueryRow(ctx, `INSERT INTO labs (owner_id, name, topic, engines) VALUES ($1, $2, $3, $4) RETURNING id`,
		ownerID, name, topic, engineList(engines)).Scan(&id)
	if isUniqueViolation(err) {
		return Lab{}, ErrNameTaken
	}

	if err != nil {
		return Lab{}, err
	}

	return s.Get(ctx, ownerID, id)
}

func (s *PostgresStore) Get(ctx context.Context, ownerID, labID int64) (Lab, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+labColumns+` FROM labs l WHERE l.id = $1 AND l.owner_id = $2`, labID, ownerID)
	return one(rows, err, scanLab, ErrNotFound)
}

func (s *PostgresStore) List(ctx context.Context, ownerID int64) ([]Lab, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+labColumns+` FROM labs l WHERE l.owner_id = $1 ORDER BY l.updated_at DESC, l.id DESC`,
		ownerID)
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, scanLab)
}

func (s *PostgresStore) Delete(ctx context.Context, ownerID, labID int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM labs WHERE id = $1 AND owner_id = $2`, labID, ownerID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	return err
}

func (s *PostgresStore) SetEngines(ctx context.Context, ownerID, labID int64, engines []string) (Lab, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE labs SET engines = $3 WHERE id = $1 AND owner_id = $2`, labID, ownerID, engineList(engines))
	if err == nil && tag.RowsAffected() == 0 {
		return Lab{}, ErrNotFound
	}

	if err != nil {
		return Lab{}, err
	}

	return s.Get(ctx, ownerID, labID)
}

// AddSource takes src's URL, scope and limits, and for a search source its
// provenance; the URL must be canonical.
func (s *PostgresStore) AddSource(ctx context.Context, ownerID, labID int64, src Source) (Source, error) {
	var id int64

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO sources (lab_id, kind, url, scope, max_depth, max_links, query, engines, reason, gathering_id)
			SELECT id, $3, $4, $5, $6, $7, $8, $9, $10, nullif($11, 0) FROM labs WHERE id = $1 AND owner_id = $2
			RETURNING id`,
			labID, ownerID, kindOf(src), src.URL, src.Scope, src.MaxDepth, src.MaxLinks, src.Query, engineList(src.Engines), src.Reason,
			src.GatheringID).Scan(&id)

		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNotFound
		case isUniqueViolation(err):
			return ErrSourceExists
		case err != nil:
			return err
		}

		_, err = tx.Exec(ctx, `UPDATE labs SET updated_at = now() WHERE id = $1`, labID)

		return err
	})
	if err != nil {
		return Source{}, err
	}

	return s.Source(ctx, ownerID, labID, id)
}

func (s *PostgresStore) Source(ctx context.Context, ownerID, labID, sourceID int64) (Source, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+sourceColumns+` FROM sources s JOIN labs l ON l.id = s.lab_id
		WHERE s.id = $1 AND s.lab_id = $2 AND l.owner_id = $3`, sourceID, labID, ownerID)

	return one(rows, err, scanSource, ErrNotFound)
}

func (s *PostgresStore) Sources(ctx context.Context, ownerID, labID int64) ([]Source, error) {
	if _, err := s.Get(ctx, ownerID, labID); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `SELECT `+sourceColumns+` FROM sources s WHERE s.lab_id = $1 ORDER BY s.created_at, s.id`, labID)
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, scanSource)
}

func (s *PostgresStore) RemoveSource(ctx context.Context, ownerID, labID, sourceID int64) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM sources s USING labs l
		WHERE s.id = $1 AND s.lab_id = $2 AND l.id = s.lab_id AND l.owner_id = $3`, sourceID, labID, ownerID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	return err
}

// PageURLs lists the successfully fetched pages of the given labs, each once;
// the labs must all be the owner's.
func (s *PostgresStore) PageURLs(ctx context.Context, ownerID int64, labIDs []int64) ([]string, error) {
	var owned int

	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM labs WHERE id = ANY($1) AND owner_id = $2`, labIDs, ownerID).
		Scan(&owned); err != nil {
		return nil, err
	}

	if owned != len(labIDs) {
		return nil, ErrNotFound
	}

	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT p.url FROM sources s
		JOIN source_pages sp ON sp.source_id = s.id
		JOIN pages p ON p.id = sp.page_id
		WHERE s.lab_id = ANY($1) AND p.status BETWEEN 200 AND 299
		ORDER BY p.url`, labIDs)
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Pages lists a lab's pages, the most recently fetched first, failed fetches
// included so a lab can show what did not work.
func (s *PostgresStore) Pages(ctx context.Context, ownerID, labID int64, limit int) ([]Page, error) {
	if _, err := s.Get(ctx, ownerID, labID); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT p.url, coalesce(p.title, ''), coalesce(p.status, 0), p.fetched_at,
		       r.decision, coalesce(r.reasons, '{}'), coalesce(r.summary, ''), coalesce(r.unreviewed, false)
		FROM pages p LEFT JOIN page_reviews r ON r.url = p.url
		WHERE p.id IN (SELECT sp.page_id FROM sources s JOIN source_pages sp ON sp.source_id = s.id WHERE s.lab_id = $1)
		ORDER BY p.fetched_at DESC NULLS LAST, p.url
		LIMIT $2`, labID, limit)
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Page, error) {
		var (
			p        Page
			decision *string
			review   PageReview
		)

		err := row.Scan(&p.URL, &p.Title, &p.Status, &p.FetchedAt, &decision, &review.Reasons, &review.Summary, &review.Unreviewed)
		if decision != nil {
			review.Decision = *decision
			p.Review = &review
		}

		return p, err
	})
}

func kindOf(src Source) string {
	if src.Kind == "" {
		return KindLink
	}

	return src.Kind
}

// engineList keeps NULL out of a NOT NULL array column.
func engineList(engines []string) []string {
	if engines == nil {
		return []string{}
	}

	return engines
}
