package pages

import (
	"context"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"go.uber.org/zap"
)

type PageDataNeo4jRepo struct {
	neo4j  neo4j.DriverWithContext
	logger *zap.SugaredLogger
}

func NewNeo4jRepo(logger *zap.SugaredLogger, neo4j neo4j.DriverWithContext) *PageDataNeo4jRepo {
	return &PageDataNeo4jRepo{
		neo4j:  neo4j,
		logger: logger,
	}
}

func (repo *PageDataNeo4jRepo) EnsureConnectivity() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var err error
	err = repo.neo4j.VerifyConnectivity(ctx)

	return err
}
