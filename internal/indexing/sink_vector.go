package indexing

import (
	"context"
	"fmt"
	"net/url"
	"time"
	"web-crawler/internal/embedding"
	"web-crawler/internal/vectorstore"
)

type VectorSink struct {
	embedder embedding.Embedder
	store    vectorstore.Store
	now      func() time.Time
}

func NewVectorSink(embedder embedding.Embedder, store vectorstore.Store) *VectorSink {
	return &VectorSink{embedder: embedder, store: store, now: time.Now}
}

func (s *VectorSink) Fingerprint() string {
	return s.store.Collection()
}

func (s *VectorSink) Submit(ctx context.Context, batch *Batch) error {
	pageURL := batch.Document.URL
	rows := make([]vectorstore.Row, len(batch.Chunks))

	if len(batch.Chunks) > 0 {
		texts := make([]string, len(batch.Chunks))
		for i, chunk := range batch.Chunks {
			texts[i] = chunk.EmbeddingText()
		}

		vectors, err := s.embedder.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("embedding %d chunks of %s: %w", len(texts), pageURL, err)
		}

		host := hostOf(pageURL)
		indexedAt := s.now().UnixMilli()

		for i, chunk := range batch.Chunks {
			rows[i] = vectorstore.Row{
				ID:          chunk.ID,
				URL:         pageURL,
				Host:        host,
				Ordinal:     int32(chunk.Ordinal),
				Title:       chunk.Title,
				Text:        texts[i],
				ContentHash: batch.Hash,
				RunID:       batch.Document.RunID,
				IndexedAt:   indexedAt,
				Vector:      vectors[i],
			}
		}
	}

	if err := s.store.Replace(ctx, pageURL, rows); err != nil {
		return fmt.Errorf("storing chunks of %s: %w", pageURL, err)
	}

	return nil
}

func (s *VectorSink) Shutdown(ctx context.Context) error {
	return s.store.Close(ctx)
}

func hostOf(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}

	return parsed.Hostname()
}
