// Package tika reads documents - PDFs, Office files, EPUB, scanned pages - with
// an Apache Tika server, as Markdown.
package tika

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

var (
	ErrUnavailable = errors.New("the document reader is unavailable")
	ErrNoText      = errors.New("the document has no readable text")
)

type Client struct {
	base     string
	http     *http.Client
	maxRunes int
}

func New(baseURL string, timeout time.Duration, maxRunes int) *Client {
	return &Client{
		base:     strings.TrimRight(baseURL, "/"),
		http:     &http.Client{Timeout: timeout, Transport: otelhttp.NewTransport(http.DefaultTransport)},
		maxRunes: maxRunes,
	}
}

const errorBodyLimit = 512

// Read returns the document's title and its text as Markdown, cut to the
// configured length at a paragraph end.
func (c *Client) Read(ctx context.Context, body []byte, contentType string) (title, text string, err error) {
	// One request gives text and metadata together: asking twice would OCR a
	// scan twice.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/rmeta/markdown", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}

	req.Header.Set("Accept", "application/json")

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		return "", "", fmt.Errorf("%w: HTTP %d: %s", ErrUnavailable, resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	// One entry per document: the first is the file itself, the rest are files
	// embedded in it - an EPUB's cover image - whose text is not the document's.
	var entries []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrUnavailable, err)
	}

	if len(entries) == 0 {
		return "", "", ErrNoText
	}

	text = clean(field(entries[0], "tk:content"))
	if text == "" {
		return "", "", ErrNoText
	}

	title = oneLine(field(entries[0], "dc:title"))
	if title == "" {
		title = firstLine(text)
	}

	return title, cut(text, c.maxRunes), nil
}

func field(entry map[string]any, key string) string {
	switch v := entry[key].(type) {
	case string:
		return v
	case []any:
		if len(v) > 0 {
			s, _ := v[0].(string)
			return s
		}
	}

	return ""
}

var (
	emptyLink = regexp.MustCompile(`\[\]\([^)]*\)`)
	imageRef  = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	escaped   = regexp.MustCompile(`\\([\[\]()*_#|<>~` + "`" + `])`)
	// Tika writes a paragraph's leading space as an entity so that Markdown
	// keeps it; as text it is noise.
	leadingSpace = regexp.MustCompile(`(?m)^(?:&#32;)+`)
	blankLines   = regexp.MustCompile(`\n{3,}`)
	trailing     = regexp.MustCompile(`[ \t]+\n`)
)

// clean drops what Tika's Markdown carries that is no text to read: empty
// anchor links, image references, escapes before brackets, entities and the
// like.
func clean(text string) string {
	text = imageRef.ReplaceAllString(text, "")
	text = emptyLink.ReplaceAllString(text, "")
	text = escaped.ReplaceAllString(text, "$1")
	text = html.UnescapeString(leadingSpace.ReplaceAllString(text, ""))
	text = trailing.ReplaceAllString(text, "\n")
	text = blankLines.ReplaceAllString(text, "\n\n")

	return strings.TrimSpace(text)
}

// cut keeps at most limit runes, ending at the last paragraph break if there
// is one in the final quarter.
func cut(text string, limit int) string {
	runes := []rune(text)
	if limit <= 0 || len(runes) <= limit {
		return text
	}

	kept := string(runes[:limit])
	if i := strings.LastIndex(kept, "\n\n"); i > len(kept)*3/4 {
		kept = kept[:i]
	}

	return strings.TrimSpace(kept)
}

const maxTitleRunes = 200

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	line = strings.TrimSpace(strings.TrimLeft(line, "# "))

	if utf8.RuneCountInString(line) > maxTitleRunes {
		line = string([]rune(line)[:maxTitleRunes])
	}

	return line
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
