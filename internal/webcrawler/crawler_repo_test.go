package webcrawler

import (
	"context"
	"net/http"
	"testing"
	"web-crawler/internal/documents"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/networker"
	"web-crawler/internal/parser"

	"go.uber.org/zap"
)

type recordingSink struct {
	docs []*documents.Document
}

func (r *recordingSink) Submit(_ context.Context, doc *documents.Document) error {
	r.docs = append(r.docs, doc)
	return nil
}

func (r *recordingSink) Shutdown(context.Context) error { return nil }

func TestSubmitDocumentByStatus(t *testing.T) {
	cases := []struct {
		status    int
		submitted bool
		removal   bool
	}{
		{http.StatusOK, true, false},
		{http.StatusNoContent, true, false},
		{http.StatusNotFound, true, true},
		{http.StatusGone, true, true},
		{http.StatusMovedPermanently, false, false},
		{http.StatusForbidden, false, false},
		{http.StatusTooManyRequests, false, false},
		{http.StatusServiceUnavailable, false, false},
	}

	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			sink := &recordingSink{}
			repo := &CrawlerRepo{logger: zap.NewNop().Sugar(), documents: sink}

			repo.submitDocument(
				context.Background(),
				&config.Task{URL: "https://example.com/", Run: &config.Run{ID: "run"}},
				&networker.FetchResult{Status: tc.status, ContentType: "text/html"},
				&parser.ParseResult{Title: "Not Found", Content: "The page you requested does not exist."},
			)

			if got := len(sink.docs) == 1; got != tc.submitted {
				t.Fatalf("status %d: submitted=%v, want %v", tc.status, got, tc.submitted)
			}

			if !tc.submitted {
				return
			}

			doc := sink.docs[0]
			if isRemoval := doc.Content == "" && doc.Title == ""; isRemoval != tc.removal {
				t.Errorf("status %d: sent as removal=%v, want %v (error page text must never be indexed)",
					tc.status, isRemoval, tc.removal)
			}
		})
	}
}

func TestSubmitDocumentSkipsEmptyContent(t *testing.T) {
	sink := &recordingSink{}
	repo := &CrawlerRepo{logger: zap.NewNop().Sugar(), documents: sink}

	repo.submitDocument(
		context.Background(),
		&config.Task{URL: "https://example.com/", Run: &config.Run{ID: "run"}},
		&networker.FetchResult{Status: http.StatusOK},
		&parser.ParseResult{Content: " \n\n "},
	)

	if len(sink.docs) != 0 {
		t.Errorf("a page with no text must not reach the sink, got %d", len(sink.docs))
	}
}
