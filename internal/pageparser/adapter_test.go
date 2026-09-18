package pageparser

import (
	"context"
	"testing"
	"web-crawler/internal/parser"

	"go.uber.org/zap"
)

func newAdapter(t *testing.T) *LegacyAdapter {
	t.Helper()

	return NewLegacyAdapter(zap.NewNop().Sugar())
}

func TestAdapterMarksEveryLinkFollowable(t *testing.T) {
	const body = `<html><body>
		<a href="/page">page</a>
		<img src="/image.png">
	</body></html>`

	res, err := newAdapter(t).Parse(context.Background(), &parser.ParseParams{
		Body:        []byte(body),
		BaseURL:     "https://example.com/",
		ContentType: "text/html",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(res.Links) == 0 {
		t.Fatal("no links extracted")
	}

	for _, link := range res.Links {
		if link.Kind != parser.LinkPage {
			t.Errorf("link %q has kind %q, want %q", link.URL, link.Kind, parser.LinkPage)
		}
	}

	if len(res.URLsOfKind(parser.LinkPage)) != len(res.Links) {
		t.Error("URLsOfKind disagrees with the links it was given")
	}
}

func TestAdapterYieldsNoContent(t *testing.T) {
	res, err := newAdapter(t).Parse(context.Background(), &parser.ParseParams{
		Body:        []byte(`<html><body><article>some prose worth indexing</article></body></html>`),
		BaseURL:     "https://example.com/",
		ContentType: "text/html",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if res.Content != "" || res.Title != "" {
		t.Errorf("legacy engine extracts no text; got title=%q content=%q", res.Title, res.Content)
	}
}

func TestAdapterDeduplicates(t *testing.T) {
	const body = `<html><body>
		<a href="/same">one</a>
		<a href="/same">two</a>
	</body></html>`

	res, err := newAdapter(t).Parse(context.Background(), &parser.ParseParams{
		Body:    []byte(body),
		BaseURL: "https://example.com/",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	seen := make(map[string]int)
	for _, link := range res.Links {
		seen[link.URL]++
	}

	for url, count := range seen {
		if count > 1 {
			t.Errorf("%q appears %d times", url, count)
		}
	}
}

func TestAdapterRejectsBadInput(t *testing.T) {
	a := newAdapter(t)

	if _, err := a.Parse(context.Background(), nil); err == nil {
		t.Error("expected error for nil params")
	}

	if _, err := a.Parse(context.Background(), &parser.ParseParams{BaseURL: "https://example.com/"}); err == nil {
		t.Error("expected error for empty body")
	}
}
