package webcrawler

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
	"web-crawler/internal/appconfig"
	"web-crawler/internal/documents"
	"web-crawler/internal/networker"
	"web-crawler/internal/parser"

	"go.uber.org/zap"
)

type recordingSink struct {
	mu        sync.Mutex
	docs      []*documents.Document
	shutdowns int
}

func (r *recordingSink) Submit(_ context.Context, doc *documents.Document) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.docs = append(r.docs, doc)

	return nil
}

func (r *recordingSink) Shutdown(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.shutdowns++

	return nil
}

func (r *recordingSink) shutdownCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.shutdowns
}

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
				"https://example.com/", "run",
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
		"https://example.com/", "run",
		&networker.FetchResult{Status: http.StatusOK},
		&parser.ParseResult{Content: " \n\n "},
	)

	if len(sink.docs) != 0 {
		t.Errorf("a page with no text must not reach the sink, got %d", len(sink.docs))
	}
}

func TestSubmitDocumentCutsLongText(t *testing.T) {
	sink := &recordingSink{}

	settings := appconfig.Default()
	settings.Indexer.MaxRunes = 100

	repo := &CrawlerRepo{logger: zap.NewNop().Sugar(), documents: sink, settings: settings}

	text := strings.Repeat("ж", 80) + "\n\n" + strings.Repeat("ы", 500)

	repo.submitDocument(context.Background(), "https://example.com/", "run",
		&networker.FetchResult{Status: http.StatusOK}, &parser.ParseResult{Content: text})

	if got := sink.docs[0].Content; got != strings.Repeat("ж", 80) {
		t.Errorf("kept %d runes, want the 80 before the paragraph break", utf8.RuneCountInString(got))
	}

	if got := cutRunes(strings.Repeat("ы", 500), 100); utf8.RuneCountInString(got) != 100 {
		t.Errorf("with no break near the end: %d runes", utf8.RuneCountInString(got))
	}

	if got := cutRunes("short", 100); got != "short" {
		t.Errorf("short text: %q", got)
	}
}
