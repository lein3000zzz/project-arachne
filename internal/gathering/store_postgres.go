package gathering

import (
	"context"
	"encoding/json"
	"errors"

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

func (s *PostgresStore) Start(ctx context.Context, labID int64, request string, engines []string, model string) (int64, error) {
	var id int64

	if engines == nil {
		engines = []string{}
	}

	err := s.pool.QueryRow(ctx, `
		INSERT INTO gatherings (lab_id, request, engines, status, model) VALUES ($1, $2, $3, 'running', $4) RETURNING id`,
		labID, request, engines, model).Scan(&id)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return 0, ErrRunning
	}

	return id, err
}

func (s *PostgresStore) AddStep(ctx context.Context, id int64, step Step) error {
	raw, err := json.Marshal([]Step{step})
	if err != nil {
		return err
	}

	_, err = s.pool.Exec(ctx, `UPDATE gatherings SET steps = steps || $2::jsonb WHERE id = $1`, id, raw)

	return err
}

func (s *PostgresStore) Finish(ctx context.Context, id int64, status, summary, errText string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE gatherings SET status = $2, summary = $3, error = $4, finished_at = now() WHERE id = $1`,
		id, status, summary, errText)

	return err
}

const columns = `id, lab_id, request, engines, status, steps, summary, error, model, started_at, finished_at`

func scan(row pgx.CollectableRow) (Gathering, error) {
	var (
		g     Gathering
		steps []byte
	)

	if err := row.Scan(&g.ID, &g.LabID, &g.Request, &g.Engines, &g.Status, &steps, &g.Summary, &g.Error, &g.Model,
		&g.StartedAt, &g.FinishedAt); err != nil {
		return g, err
	}

	return g, json.Unmarshal(steps, &g.Steps)
}

func (s *PostgresStore) Get(ctx context.Context, id int64) (Gathering, error) {
	return s.one(ctx, `SELECT `+columns+` FROM gatherings WHERE id = $1`, id)
}

func (s *PostgresStore) Latest(ctx context.Context, labID int64) (Gathering, error) {
	return s.one(ctx, `SELECT `+columns+` FROM gatherings WHERE lab_id = $1 ORDER BY started_at DESC, id DESC LIMIT 1`, labID)
}

func (s *PostgresStore) one(ctx context.Context, query string, arg int64) (Gathering, error) {
	rows, err := s.pool.Query(ctx, query, arg)
	if err != nil {
		return Gathering{}, err
	}

	g, err := pgx.CollectExactlyOneRow(rows, scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return Gathering{}, ErrNotFound
	}

	return g, err
}

func (s *PostgresStore) Interrupt(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE gatherings SET status = 'failed', error = 'interrupted by a restart', finished_at = now()
		WHERE status = 'running'`)

	return tag.RowsAffected(), err
}
