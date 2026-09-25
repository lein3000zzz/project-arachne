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
