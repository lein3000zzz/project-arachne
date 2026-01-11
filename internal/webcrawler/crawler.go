package webcrawler

import (
	"web-crawler/internal/config"
)

type Crawler interface {
	StartCrawler(tChan <-chan *config.Task, workersNum int)
}
