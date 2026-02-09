package networker

type FetchResult struct {
	Body        []byte
	Status      int
	ContentType string
}

type Networker interface {
	Stop()
	Fetch(url string) (*FetchResult, error)
}
