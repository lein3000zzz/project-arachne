package vectorstore

import "context"

type Row struct {
	ID          string
	URL         string
	Host        string
	Ordinal     int32
	Title       string
	Text        string
	ContentHash string
	RunID       string
	IndexedAt   int64
	Vector      []float32
}

type Store interface {
	Collection() string
	// Replace makes the stored rows for url exactly rows; an empty rows removes
	// the URL from the index.
	Replace(ctx context.Context, url string, rows []Row) error
	Close(ctx context.Context) error
}

type Hit struct {
	ID    string
	URL   string
	Host  string
	Title string
	Text  string
	Score float32
}

// Filter narrows a search. The zero value searches everything.
type Filter struct {
	Host string
}

// Searcher returns chunks best first. Dense scores are cosine similarities, text
// scores BM25: the two scales are not comparable.
type Searcher interface {
	SearchDense(ctx context.Context, vector []float32, limit int, filter Filter) ([]Hit, error)
	SearchText(ctx context.Context, query string, limit int, filter Filter) ([]Hit, error)
}
