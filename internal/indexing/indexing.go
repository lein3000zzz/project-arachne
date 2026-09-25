package indexing

import (
	"context"
	"web-crawler/internal/chunker"
	"web-crawler/internal/documents"
)

// Batch carries every chunk of one document version. Chunks may be empty: the
// page no longer has indexable text, so whatever is indexed for its URL is stale.
type Batch struct {
	Document *documents.Document
	Hash     string
	Chunks   []chunker.Chunk
}

type ChunkSink interface {
	Submit(ctx context.Context, batch *Batch) error
	// Fingerprint identifies where batches end up. It is part of the document hash,
	// so pointing the sink somewhere new re-indexes pages that did not change.
	Fingerprint() string
	Shutdown(ctx context.Context) error
}

type HashStore interface {
	Get(ctx context.Context, url string) (string, error)
	Set(ctx context.Context, url, hash string) error
	Shutdown(ctx context.Context) error
}
