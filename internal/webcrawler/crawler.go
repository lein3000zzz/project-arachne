package webcrawler

const (
	ConcurrentTasksLimit = 20
	ConcurrentRunsLimit  = 1
)

type Crawler interface {
	StartCrawler() error
}
