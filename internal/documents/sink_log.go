package documents

import (
	"context"
	"sync/atomic"

	"go.uber.org/zap"
)

// Placeholder sink: extracted text has nowhere to go until the chunker and the
// vector index exist. Keeps the crawler's output path final-shaped until then.
type LogSink struct {
	logger    *zap.SugaredLogger
	submitted atomic.Int64
}

func NewLogSink(logger *zap.SugaredLogger) *LogSink {
	return &LogSink{logger: logger}
}

func (s *LogSink) Submit(_ context.Context, doc *Document) error {
	s.submitted.Add(1)

	s.logger.Infow("document extracted",
		"url", doc.URL,
		"title", doc.Title,
		"chars", len(doc.Content),
	)

	return nil
}

func (s *LogSink) Shutdown(_ context.Context) error {
	s.logger.Infow("document sink stopped", "submitted", s.submitted.Load())
	return nil
}
