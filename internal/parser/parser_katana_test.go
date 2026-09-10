package parser

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func newTestParser(t *testing.T) *KatanaParser {
	t.Helper()

	p, err := NewKatanaParser(zap.NewNop().Sugar())
	if err != nil {
		t.Fatalf("NewKatanaParser: %v", err)
	}

	return p
}

func parse(t *testing.T, p *KatanaParser, body, base, contentType string) *ParseResult {
	t.Helper()

	res, err := p.Parse(context.Background(), &ParseParams{
		Body:        []byte(body),
		BaseURL:     base,
		ContentType: contentType,
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	return res
}

func kindOf(res *ParseResult, url string) (LinkKind, bool) {
	for _, link := range res.Links {
		if link.URL == url {
			return link.Kind, true
		}
	}

	return "", false
}

const pageHTML = `<!doctype html>
<html>
<head><title>Widget documentation</title></head>
<body>
  <nav><a href="/nav/index.html">Navigation</a></nav>
  <article>
    <h1>Configuring the widget</h1>
    <p>The widget accepts a configuration object at construction time. Every field
    is optional and every field has a documented default, so an empty object is a
    valid configuration and produces a widget in its default state.</p>
    <p>Pass the timeout in milliseconds. Values below ten milliseconds are clamped
    because the underlying transport cannot deliver a response faster than that
    under any circumstances, and pretending otherwise only produces confusing
    timeouts that appear to fire before the request was even sent.</p>
    <p>See the <a href="/guide/advanced">advanced guide</a> for retry policy,
    connection pooling and the interaction between the two.</p>
  </article>
  <img src="/images/diagram.png">
  <a href="mailto:support@example.com">Email us</a>
  <a href="javascript:void(0)">Nothing</a>
  <a href="#section">Anchor</a>
  <a href="https://external.example.org/reference">External reference</a>
  <script>var endpoint = "/api/v1/widgets";</script>
  <footer>Copyright notice, all rights reserved, not indexable boilerplate.</footer>
</body>
</html>`

func TestParseHTMLClassifiesLinks(t *testing.T) {
	res := parse(t, newTestParser(t), pageHTML, "https://docs.example.com/widgets/config", "text/html; charset=utf-8")

	cases := []struct {
		name string
		url  string
		want LinkKind
	}{
		{"relative hyperlink resolves against base", "https://docs.example.com/guide/advanced", LinkPage},
		{"nav hyperlink is still a page", "https://docs.example.com/nav/index.html", LinkPage},
		{"absolute hyperlink", "https://external.example.org/reference", LinkPage},
		{"image subresource is not followable", "https://docs.example.com/images/diagram.png", LinkOther},
		{"script endpoint is not followable", "https://docs.example.com/api/v1/widgets", LinkOther},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := kindOf(res, tc.url)
			if !found {
				t.Fatalf("link %q not extracted; got %v", tc.url, res.Links)
			}

			if got != tc.want {
				t.Errorf("link %q: kind = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

func TestParseHTMLDropsNonCrawlableSchemes(t *testing.T) {
	res := parse(t, newTestParser(t), pageHTML, "https://docs.example.com/widgets/config", "text/html")

	for _, link := range res.Links {
		if strings.HasPrefix(link.URL, "mailto:") ||
			strings.HasPrefix(link.URL, "javascript:") ||
			strings.Contains(link.URL, "#") {
			t.Errorf("non-crawlable URL survived normalization: %q", link.URL)
		}
	}
}

func TestParseHTMLExtractsReadableContent(t *testing.T) {
	res := parse(t, newTestParser(t), pageHTML, "https://docs.example.com/widgets/config", "text/html")

	if res.Title != "Widget documentation" {
		t.Errorf("Title = %q, want %q", res.Title, "Widget documentation")
	}

	if !strings.Contains(res.Content, "configuration object at construction time") {
		t.Errorf("article body missing from content: %q", res.Content)
	}

	if strings.Contains(res.Content, "not indexable boilerplate") {
		t.Errorf("footer boilerplate leaked into content: %q", res.Content)
	}

	if strings.Contains(res.Content, "var endpoint") {
		t.Errorf("script source leaked into content: %q", res.Content)
	}
}

func TestParseUpgradesSubresourceToPage(t *testing.T) {
	const body = `<html><body>
		<img src="/shared/thing">
		<a href="/shared/thing">Also a page</a>
	</body></html>`

	res := parse(t, newTestParser(t), body, "https://example.com/", "text/html")

	got, found := kindOf(res, "https://example.com/shared/thing")
	if !found {
		t.Fatalf("link not extracted; got %v", res.Links)
	}

	if got != LinkPage {
		t.Errorf("kind = %q, want %q: a URL seen as a hyperlink is a page regardless of order", got, LinkPage)
	}
}

func TestParseDeduplicates(t *testing.T) {
	const body = `<html><body>
		<a href="/same">one</a>
		<a href="/same">two</a>
		<a href="/same#fragment">three</a>
	</body></html>`

	res := parse(t, newTestParser(t), body, "https://example.com/", "text/html")

	var count int
	for _, link := range res.Links {
		if link.URL == "https://example.com/same" {
			count++
		}
	}

	if count != 1 {
		t.Errorf("URL appears %d times, want 1: %v", count, res.Links)
	}
}

func TestParseJSONFindsURLShapedStrings(t *testing.T) {
	const body = `{
		"next": "/api/v1/page/2",
		"docs": "https://docs.example.com/reference",
		"title": "a plain string that is not a link",
		"nested": {"items": [{"href": "/api/v1/item/7"}]}
	}`

	res := parse(t, newTestParser(t), body, "https://api.example.com/v1/page/1", "application/json")

	want := []string{
		"https://api.example.com/api/v1/page/2",
		"https://docs.example.com/reference",
		"https://api.example.com/api/v1/item/7",
	}

	for _, url := range want {
		if _, found := kindOf(res, url); !found {
			t.Errorf("expected %q in %v", url, res.Links)
		}
	}

	for _, link := range res.Links {
		if strings.Contains(link.URL, "plain%20string") || strings.Contains(link.URL, "not+a+link") {
			t.Errorf("plain prose was treated as a link: %q", link.URL)
		}
	}

	if res.Content != "" {
		t.Errorf("JSON should yield no indexable content, got %q", res.Content)
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	p := newTestParser(t)

	if _, err := p.Parse(context.Background(), nil); err == nil {
		t.Error("expected error for nil params")
	}

	if _, err := p.Parse(context.Background(), &ParseParams{Body: nil}); err == nil {
		t.Error("expected error for empty body")
	}
}

func TestClassifyBodySniffsWhenTypeIsWrong(t *testing.T) {
	cases := []struct {
		name      string
		mediaType string
		body      string
		want      bodyKind
	}{
		{"declared html", "text/html", "<p>hi</p>", kindHTML},
		{"declared json", "application/json", `{"a":1}`, kindJSON},
		{"vendor json suffix", "application/vnd.api+json", `{"a":1}`, kindJSON},
		{"declared js", "application/javascript", "var a = 1;", kindJS},
		{"undeclared html sniffed", "", "<!doctype html><html><body>x</body></html>", kindHTML},
		{"undeclared json sniffed", "", `{"a":1}`, kindJSON},
		{"undeclared plain text", "", "just some words", kindText},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyBody(tc.mediaType, []byte(tc.body)); got != tc.want {
				t.Errorf("classifyBody(%q) = %v, want %v", tc.mediaType, got, tc.want)
			}
		})
	}
}

func TestParseJavaScriptRecoversEndpoints(t *testing.T) {
	const body = `
		const API = "/api/v2/session";
		fetch("/api/v2/items?page=1").then(r => r.json());
		const cdn = "https://cdn.example.net/bundle/main.js";
		xhr.open("GET", "/legacy/endpoint");
	`

	res := parse(t, newTestParser(t), body, "https://app.example.com/static/app.js", "application/javascript")

	want := []string{
		"https://app.example.com/api/v2/session",
		"https://cdn.example.net/bundle/main.js",
	}

	for _, url := range want {
		if _, found := kindOf(res, url); !found {
			t.Errorf("expected %q in %v", url, res.Links)
		}
	}

	for _, link := range res.Links {
		if link.Kind != LinkOther {
			t.Errorf("JS-derived URL %q has kind %q, want %q", link.URL, link.Kind, LinkOther)
		}
	}

	if res.Content != "" {
		t.Errorf("JS should yield no indexable content, got %q", res.Content)
	}
}
