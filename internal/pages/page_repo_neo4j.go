package pages

import (
	"context"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"go.uber.org/zap"
)

type PageDataNeo4jRepo struct {
	driver neo4j.DriverWithContext
	logger *zap.SugaredLogger
}

func NewNeo4jRepo(logger *zap.SugaredLogger, driver neo4j.DriverWithContext) *PageDataNeo4jRepo {
	return &PageDataNeo4jRepo{
		driver: driver,
		logger: logger,
	}
}

func (repo *PageDataNeo4jRepo) EnsureConnectivity() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var err error
	err = repo.driver.VerifyConnectivity(ctx)

	return err
}

func (repo *PageDataNeo4jRepo) SavePage(page *PageData) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	params, errParams := page.toParams()
	if errParams != nil {
		repo.logger.Errorf("failed to parse params: %v", errParams)
		return errParams
	}

	queryRes, errQuery := neo4j.ExecuteQuery(ctx, repo.driver, `
		MERGE (p:Page {url: $url})
		ON CREATE SET p.foundAt = $foundAt, p.lastUpdatedAt = $lastUpdatedAt, p.status = $status, p.links = $links, p.lastRunID = $lastRunID, p.contentType = $contentType 
		ON MATCH SET p.lastUpdatedAt = $lastUpdatedAt, p.status = $status, p.links = $links, p.lastRunID = $lastRunID, p.contentType = $contentType
		WITH p
		UNWIND $links AS linkUrl
		MERGE (l:Page {url: linkUrl})
		ON CREATE SET l.foundAt = $foundAt
		MERGE (p)-[:LINKS_TO]->(l)
	`,
		params,
		neo4j.EagerResultTransformer,
		neo4j.ExecuteQueryWithDatabase("pages"),
	)

	if errQuery != nil {
		repo.logger.Errorf("failed to execute query: %v", errQuery)
		return errQuery
	}

	repo.logger.Debugw("page saved pages number: %d", queryRes.Summary.ResultAvailableAfter())

	return nil
}
