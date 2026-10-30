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
	// EnsureExcluded takes pages a changed exclude list matches out of labs and
	// returns them, for the index to drop.
	EnsureExcluded(ctx context.Context, fingerprint string, excluded func(url string) bool) ([]string, error)
	GetSaverChan() chan<- *data.PageData
	StartSaverWorkers(workers int)
	Shutdown(ctx context.Context) error
}
