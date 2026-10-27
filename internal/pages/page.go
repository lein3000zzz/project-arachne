package pages

import (
	"context"
	"web-crawler/internal/domain/data"
)

type PageRepo interface {
	SavePage(page *data.PageData) error
	EnsureConnectivity() error
	// EnsureCanonical re-canonicalizes stored URLs after urlnorm.Version changed and
	// returns the spellings that stopped being pages.
	EnsureCanonical(ctx context.Context) ([]string, error)
	GetSaverChan() chan<- *data.PageData
	StartSaverWorkers(workers int)
	Shutdown(ctx context.Context) error
}
