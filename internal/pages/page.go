package pages

import (
	"context"
	"web-crawler/internal/domain/data"
)

type PageRepo interface {
	SavePage(page *data.PageData) error
	EnsureConnectivity() error
	EnsureSchema(ctx context.Context, resolve Resolver) ([]string, error)
	GetSaverChan() chan<- *data.PageData
	StartSaverWorkers(workers int)
	Shutdown(ctx context.Context) error
}
