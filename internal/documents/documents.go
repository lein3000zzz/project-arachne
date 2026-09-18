package documents

import (
	"context"
	"time"
)

type Document struct {
	URL         string
	Title       string
	Content     string
	ContentType string
	RunID       string
	FetchedAt   time.Time
}

type Sink interface {
	Submit(ctx context.Context, doc *Document) error
	Shutdown(ctx context.Context) error
}
