package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"web-crawler/internal/labs"

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

const uniqueViolation = "23505"

func (s *PostgresStore) Create(ctx context.Context, ownerID, labID int64, kind Kind, title, request string) (int64, error) {
	var id int64

	err := s.pool.QueryRow(ctx, `
		INSERT INTO artifacts (lab_id, kind, title, request) SELECT id, $3, $4, $5 FROM labs WHERE id = $1 AND owner_id = $2
		RETURNING id`, labID, ownerID, kind, title, request).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, labs.ErrNotFound
	}

	return id, err
}

func (s *PostgresStore) StartVersion(ctx context.Context, artifactID int64, v Version) (int, error) {
	outline, sections, sources, err := encode(v)
	if err != nil {
		return 0, err
	}

	var number int

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// The artifact's row serialises versions of it: the next number is
		// read and taken in one go.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM artifacts WHERE id = $1 FOR UPDATE`, artifactID); err != nil {
			return err
		}

		err := tx.QueryRow(ctx, `
			INSERT INTO artifact_versions (artifact_id, version, status, outline, sections, sources, markdown, model, finished_at)
			SELECT $1, coalesce(max(version), 0) + 1, $2, $3, $4, $5, $6, $7, $8 FROM artifact_versions WHERE artifact_id = $1
			RETURNING version`, artifactID, v.Status, outline, sections, sources, v.Markdown, v.Model, v.FinishedAt).Scan(&number)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `UPDATE artifacts SET updated_at = now() WHERE id = $1`, artifactID)

		return err
	})

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return 0, ErrBuilding
	}

	return number, err
}

func (s *PostgresStore) SaveVersion(ctx context.Context, artifactID int64, v Version) error {
	outline, sections, sources, err := encode(v)
	if err != nil {
		return err
	}

	_, err = s.pool.Exec(ctx, `
		UPDATE artifact_versions SET status = $3, outline = $4, sections = $5, sources = $6, markdown = $7, error = $8,
		       seconds = $9, finished_at = $10
		WHERE artifact_id = $1 AND version = $2`,
		artifactID, v.Number, v.Status, outline, sections, sources, v.Markdown, v.Error, v.Seconds, v.FinishedAt)
	if err != nil {
		return err
	}

	_, err = s.pool.Exec(ctx, `UPDATE artifacts SET updated_at = now() WHERE id = $1`, artifactID)

	return err
}

func encode(v Version) (outline, sections, sources []byte, err error) {
	if outline, err = json.Marshal(v.Outline); err != nil {
		return nil, nil, nil, err
	}

	if sections, err = json.Marshal(nonNil(v.Sections)); err != nil {
		return nil, nil, nil, err
	}

	sources, err = json.Marshal(nonNil(v.Sources))

	return outline, sections, sources, err
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}

	return s
}

const artifactColumns = `a.id, a.lab_id, a.kind, a.title, a.request, a.created_at, a.updated_at`

// The newest version of each, its sections reduced to how far they came.
const latestColumns = `
	v.version, v.status, v.outline, v.error, v.model, v.seconds, v.started_at, v.finished_at,
	coalesce((SELECT jsonb_agg(jsonb_build_object('heading', e->>'heading', 'state', e->>'state'))
	          FROM jsonb_array_elements(v.sections) e), '[]')`

func (s *PostgresStore) List(ctx context.Context, ownerID, labID int64) ([]Artifact, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+artifactColumns+`,`+latestColumns+`
		FROM artifacts a JOIN labs l ON l.id = a.lab_id
		JOIN LATERAL (SELECT * FROM artifact_versions WHERE artifact_id = a.id ORDER BY version DESC LIMIT 1) v ON true
		WHERE a.lab_id = $1 AND l.owner_id = $2
		ORDER BY a.updated_at DESC, a.id DESC`, labID, ownerID)
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, scanListed)
}

func scanListed(row pgx.CollectableRow) (Artifact, error) {
	var (
		a Artifact
		v Version
	)

	err := row.Scan(&a.ID, &a.LabID, &a.Kind, &a.Title, &a.Request, &a.CreatedAt, &a.UpdatedAt,
		&v.Number, &v.Status, &v.Outline, &v.Error, &v.Model, &v.Seconds, &v.StartedAt, &v.FinishedAt, &v.Sections)
	a.Latest = &v

	return a, err
}

func (s *PostgresStore) Get(ctx context.Context, ownerID, artifactID int64) (Artifact, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+artifactColumns+`,`+latestColumns+`
		FROM artifacts a JOIN labs l ON l.id = a.lab_id
		JOIN LATERAL (SELECT * FROM artifact_versions WHERE artifact_id = a.id ORDER BY version DESC LIMIT 1) v ON true
		WHERE a.id = $1 AND l.owner_id = $2`, artifactID, ownerID)
	if err != nil {
		return Artifact{}, err
	}

	a, err := pgx.CollectExactlyOneRow(rows, scanListed)
	if errors.Is(err, pgx.ErrNoRows) {
		return Artifact{}, ErrNotFound
	}

	return a, err
}

func (s *PostgresStore) Version(ctx context.Context, ownerID, artifactID int64, number int) (Version, error) {
	var v Version

	err := s.pool.QueryRow(ctx, `
		SELECT v.version, v.status, v.outline, v.sections, v.sources, v.markdown, v.error, v.model, v.seconds,
		       v.started_at, v.finished_at
		FROM artifact_versions v JOIN artifacts a ON a.id = v.artifact_id JOIN labs l ON l.id = a.lab_id
		WHERE v.artifact_id = $1 AND l.owner_id = $2 AND ($3 = 0 OR v.version = $3)
		ORDER BY v.version DESC LIMIT 1`, artifactID, ownerID, number).
		Scan(&v.Number, &v.Status, &v.Outline, &v.Sections, &v.Sources, &v.Markdown, &v.Error, &v.Model, &v.Seconds,
			&v.StartedAt, &v.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, ErrNotFound
	}

	return v, err
}

func (s *PostgresStore) Versions(ctx context.Context, ownerID, artifactID int64) ([]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT v.version FROM artifact_versions v JOIN artifacts a ON a.id = v.artifact_id JOIN labs l ON l.id = a.lab_id
		WHERE v.artifact_id = $1 AND l.owner_id = $2 ORDER BY v.version`, artifactID, ownerID)
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, pgx.RowTo[int])
}

func (s *PostgresStore) Delete(ctx context.Context, ownerID, artifactID int64) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM artifacts a USING labs l WHERE a.id = $1 AND l.id = a.lab_id AND l.owner_id = $2`, artifactID, ownerID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	return err
}

// Interrupt marks builds left running by a previous process as failed.
func (s *PostgresStore) Interrupt(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE artifact_versions SET status = 'failed', error = 'interrupted by a restart', finished_at = now()
		WHERE status = 'running'`)

	return tag.RowsAffected(), err
}
