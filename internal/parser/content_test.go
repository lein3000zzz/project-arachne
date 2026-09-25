package parser

import (
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func TestParseHTMLKeepsBlockStructure(t *testing.T) {
	res := parse(t, newTestParser(t), pageHTML, "https://docs.example.com/widgets/config", "text/html")

	paragraphs := strings.Split(res.Content, "\n\n")
	if len(paragraphs) < 3 {
		t.Fatalf("want paragraphs separated by blank lines, got %d block(s): %q", len(paragraphs), res.Content)
	}

	const wrapped = "Every field is optional and every field has a documented default"
	if !strings.Contains(res.Content, wrapped) {
		t.Errorf("a line wrapped in the source must be joined with a single space: %q", res.Content)
	}

	for _, p := range paragraphs {
		if p != strings.TrimSpace(p) || strings.Contains(p, "  ") {
			t.Errorf("paragraph carries stray whitespace: %q", p)
		}
	}
}

func TestNormalizeBlocks(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"collapses inner whitespace", "a   b\t c", "a b c"},
		{"keeps paragraph break", "one\n\ntwo", "one\n\ntwo"},
		{"keeps list lines", "item one\nitem two", "item one\nitem two"},
		{"collapses runs of blank lines", "one\n\n\n\n  \ntwo", "one\n\ntwo"},
		{"trims edges", "\n\n  one  \n\n", "one"},
		{"crlf", "one\r\n\r\ntwo", "one\n\ntwo"},
		{"empty", "  \n \n ", ""},
		{"cyrillic untouched", "Привет,  мир\n\nВторой абзац", "Привет, мир\n\nВторой абзац"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeBlocks(tc.in); got != tc.want {
				t.Errorf("normalizeBlocks(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFallbackTextKeepsStructureAndDropsChrome(t *testing.T) {
	const page = `<html><head><title>Head title</title></head><body>
		<nav>Site navigation</nav>
		<h1>Heading</h1>
		<p>First paragraph,
		   wrapped in the source.</p>
		<p>Second paragraph.</p>
		<footer>Footer text</footer>
	</body></html>`

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(page))
	if err != nil {
		t.Fatalf("goquery: %v", err)
	}

	got := fallbackText(doc)

	for _, unwanted := range []string{"Site navigation", "Footer text", "Head title"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("fallback kept %q: %q", unwanted, got)
		}
	}

	want := "Heading\n\nFirst paragraph, wrapped in the source.\n\nSecond paragraph."
	if got != want {
		t.Errorf("fallbackText =\n%q\nwant\n%q", got, want)
	}

	if doc.Find("nav").Length() != 1 {
		t.Error("fallbackText mutated the shared document")
	}
}

func TestTitlesAreCapped(t *testing.T) {
	long := strings.Repeat("Заголовок ", 200)

	got := capTitle(long)
	if n := len([]rune(got)); n > maxTitleRunes {
		t.Errorf("title is %d runes, cap is %d", n, maxTitleRunes)
	}

	if capTitle("Short title") != "Short title" {
		t.Error("short titles must pass through unchanged")
	}
}
