package pages

import (
	"context"
	"time"
	"web-crawler/internal/domain/data"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.uber.org/zap"
)

type PageNeo4jRepo struct {
	driver   neo4j.DriverWithContext
	database string
	logger   *zap.SugaredLogger

	saverChan chan *data.PageData
}

func NewNeo4jRepo(logger *zap.SugaredLogger, driver neo4j.DriverWithContext) *PageNeo4jRepo {
	return &PageNeo4jRepo{
		driver:    driver,
		database:  "pages",
		logger:    logger,
		saverChan: make(chan *data.PageData),
	}
}

func (repo *PageNeo4jRepo) EnsureConnectivity() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var err error
	err = repo.driver.VerifyConnectivity(ctx)

	return err
}

func (repo *PageNeo4jRepo) Shutdown(ctx context.Context) error {
	close(repo.saverChan)
	return repo.driver.Close(ctx)
}

func (repo *PageNeo4jRepo) SavePage(page *data.PageData) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tracer := otel.Tracer("neo4j")
	_, span := tracer.Start(ctx, "SavePage")
	defer span.End()

	span.SetAttributes(
		attribute.String("page.url", page.URL),
		attribute.Int("page.lastRunID", len(page.LastRunID)),
		attribute.Int("page.status", page.Status),
		attribute.Int("page.linksNum", len(page.Links)),
	)

	params, errParams := page.ToParams()
	if errParams != nil {
		repo.logger.Errorf("failed to parse params: %v", errParams)
		return errParams
	}

	if page.Aliases == nil {
		params["aliases"] = []string{}
	}

	if page.Links == nil {
		params["links"] = []string{}
	}

	_, errQuery := repo.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return nil, savePage(ctx, tx, params, page.Aliases)
	})
	if errQuery != nil {
		span.RecordError(errQuery)
		span.SetStatus(codes.Error, "Save failed")
		repo.logger.Errorf("failed to execute query: %v", errQuery)
		return errQuery
	}

	span.SetStatus(codes.Ok, "Page saved")

	return nil
}

func (repo *PageNeo4jRepo) write(ctx context.Context, work neo4j.ManagedTransactionWork) (any, error) {
	session := repo.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: repo.database, AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)

	return session.ExecuteWrite(ctx, work)
}

func (repo *PageNeo4jRepo) GetSaverChan() chan<- *data.PageData {
	return repo.saverChan
}

func (repo *PageNeo4jRepo) StartSaverWorkers(workers int) {
	for i := 0; i < workers; i++ {
		// TODO переписать под логику батчей.
		go repo.startSaverWorker()
	}
}

func (repo *PageNeo4jRepo) startSaverWorker() {
	for pageData := range repo.saverChan {
		err := repo.SavePage(pageData)

		if err != nil {
			repo.logger.Errorf("failed to save page: %v", err)
			continue
		}
	}
}
