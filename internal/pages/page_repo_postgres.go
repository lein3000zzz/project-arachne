package pages

import (
	"context"
	"sync"
	"time"
	"web-crawler/internal/database"
	"web-crawler/internal/domain/data"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.uber.org/zap"
)

const (
	saveTimeout     = 15 * time.Second
	maxSaveAttempts = 4
)

type PagePostgresRepo struct {
	pool   *pgxpool.Pool
	logger *zap.SugaredLogger

	saverChan chan *data.PageData
	savers    sync.WaitGroup
}

func NewPostgresRepo(logger *zap.SugaredLogger, pool *pgxpool.Pool) *PagePostgresRepo {
	return &PagePostgresRepo{
		pool:      pool,
		logger:    logger,
		saverChan: make(chan *data.PageData),
	}
}

func (repo *PagePostgresRepo) EnsureConnectivity() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return repo.pool.Ping(ctx)
}

// SavePage writes the page, replaces its outgoing links and folds its aliases into
// it, all in one transaction. Concurrent savers touch overlapping link targets and
// may deadlock; the loser is simply run again.
func (repo *PagePostgresRepo) SavePage(page *data.PageData) error {
	ctx, cancel := context.WithTimeout(context.Background(), saveTimeout)
	defer cancel()

	ctx, span := otel.Tracer("postgres").Start(ctx, "SavePage")
	defer span.End()

	span.SetAttributes(
		attribute.String("page.url", page.URL),
		attribute.Int("page.status", page.Status),
		attribute.Int("page.links", len(page.Links)),
		attribute.Int("page.aliases", len(page.Aliases)),
	)

	var err error

	for attempt := 1; attempt <= maxSaveAttempts; attempt++ {
		err = pgx.BeginFunc(ctx, repo.pool, func(tx pgx.Tx) error {
			return savePage(ctx, tx, page)
		})
		if err == nil || !database.Retryable(err) {
			break
		}

		span.AddEvent("retrying after a transaction conflict")
	}

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Save failed")

		return err
	}

	span.SetStatus(codes.Ok, "Page saved")

	return nil
}

func (repo *PagePostgresRepo) GetSaverChan() chan<- *data.PageData {
	return repo.saverChan
}

func (repo *PagePostgresRepo) StartSaverWorkers(workers int) {
	repo.savers.Add(workers)

	for i := 0; i < workers; i++ {
		go repo.startSaverWorker()
	}
}

func (repo *PagePostgresRepo) startSaverWorker() {
	defer repo.savers.Done()

	for pageData := range repo.saverChan {
		if err := repo.SavePage(pageData); err != nil {
			repo.logger.Errorw("Failed to save page", "url", pageData.URL, "error", err)
		}
	}
}

// Shutdown runs after the crawl workers - the channel's only senders - have exited.
func (repo *PagePostgresRepo) Shutdown(ctx context.Context) error {
	close(repo.saverChan)

	finished := make(chan struct{})
	go func() {
		repo.savers.Wait()
		close(finished)
	}()

	var err error

	select {
	case <-finished:
	case <-ctx.Done():
		err = ctx.Err()
	}

	repo.pool.Close()

	return err
}
