package pages

import (
	"web-crawler/internal/domain/data"
)

type PageRepo interface {
	SavePage(page *data.PageData) error
	EnsureConnectivity() error
	GetSaverChan() chan<- *data.PageData
	StartSaverWorkers(workers int)
}
