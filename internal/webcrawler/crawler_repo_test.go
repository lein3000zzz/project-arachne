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

func TestSubmitDocumentOnlyIndexesSuccessfulResponses(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{http.StatusOK, true},
		{http.StatusNoContent, true},
		{http.StatusMovedPermanently, false},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
		{http.StatusTooManyRequests, false},
		{http.StatusServiceUnavailable, false},
	}

	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			sink := &recordingSink{}
			repo := &CrawlerRepo{logger: zap.NewNop().Sugar(), documents: sink}

			repo.submitDocument(
				context.Background(),
				&config.Task{URL: "https://example.com/", Run: &config.Run{ID: "run"}},
				&networker.FetchResult{Status: tc.status, ContentType: "text/html"},
				&parser.ParseResult{Title: "T", Content: "Please set a user-agent and respect our robot policy."},
			)

			if got := len(sink.docs) == 1; got != tc.want {
				t.Errorf("status %d: submitted=%v, want %v", tc.status, got, tc.want)
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
