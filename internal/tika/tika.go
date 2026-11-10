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
	"web-crawler/internal/domain/data"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

var (
	ErrUnavailable = errors.New("the document reader is unavailable")
	ErrNoText      = errors.New("the document has no readable text")
	ErrTooLong     = fmt.Errorf("the document's text is over %d MiB: too long to read", maxReplyBytes>>20)
)

type Client struct {
	base     string
	http     *http.Client
	maxRunes int
	// One per document read at once: its reply is held, and copied as it is
	// cleaned, while the crawl workers that wait hold only their own files.
	slots chan struct{}
}

// New reads at most concurrent documents at once.
func New(baseURL string, timeout time.Duration, maxRunes, concurrent int) *Client {
	return &Client{
		base:     strings.TrimRight(baseURL, "/"),
		http:     &http.Client{Timeout: timeout, Transport: otelhttp.NewTransport(http.DefaultTransport)},
		maxRunes: maxRunes,
		slots:    make(chan struct{}, max(concurrent, 1)),
	}
}

const (
	errorBodyLimit = 512
	// The whole text is read for its references, so this bounds it: a reply
	// past it is no document anyone reads in full.
	maxReplyBytes = 64 << 20
)

type Document struct {
	Title string
	// Markdown, cut to the configured length at a paragraph end.
	Text string
	// Found in the whole text: a long book's reference list is past the cut.
	References []data.Reference
}

func (c *Client) Read(ctx context.Context, body []byte, contentType string) (Document, error) {
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return Document{}, ctx.Err()
	}

	// One request gives text and metadata together: asking twice would OCR a
	// scan twice.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/rmeta/markdown", bytes.NewReader(body))
	if err != nil {
		return Document{}, err
	}

	req.Header.Set("Accept", "application/json")

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Document{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		return Document{}, fmt.Errorf("%w: HTTP %d: %s", ErrUnavailable, resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	// One entry per document: the first is the file itself, the rest are files
	// embedded in it - an EPUB's cover image - whose text is not the document's.
	reply := &io.LimitedReader{R: resp.Body, N: maxReplyBytes + 1}

	var entries []map[string]any
	if err := json.NewDecoder(reply).Decode(&entries); err != nil {
		if reply.N <= 0 {
			return Document{}, ErrTooLong
		}

		return Document{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}

	if len(entries) == 0 {
		return Document{}, ErrNoText
	}

	text := clean(field(entries[0], "tk:content"))
	if text == "" {
		return Document{}, ErrNoText
	}

	title := oneLine(field(entries[0], "dc:title"))
	if title == "" {
		title = firstLine(text)
	}

	return Document{Title: title, Text: cut(text, c.maxRunes), References: references(text)}, nil
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
// is one in the final quarter. It walks the text rather than converting it:
// a book's text is megabytes.
func cut(text string, limit int) string {
	if limit <= 0 {
		return text
	}

	n := 0

	for i := range text {
		if n == limit {
			kept := text[:i]
			if j := strings.LastIndex(kept, "\n\n"); j > len(kept)*3/4 {
				kept = kept[:j]
			}

			return strings.TrimSpace(kept)
		}

		n++
	}

	return text
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
