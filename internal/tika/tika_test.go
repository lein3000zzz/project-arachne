package tika

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func server(t *testing.T, status int, body string, got *http.Request, sent *[]byte) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = *r
		*sent, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return srv
}

func TestReadTakesTheDocumentNotWhatIsEmbedded(t *testing.T) {
	var (
		got  http.Request
		sent []byte
	)

	srv := server(t, http.StatusOK, `[
		{"Content-Type": "application/epub+zip", "dc:title": "The  Art\nof War",
		 "tk:content": "[]()\n\n![](cover.jpg)\n\n## Chapter I\n\n\n\n&#32;Sun Tzŭ said: \\[1\\] The art of war &amp; peace...   \n"},
		{"Content-Type": "image/jpeg", "tk:content": "text OCR'd from the cover"}
	]`, &got, &sent)

	doc, err := New(srv.URL+"/", time.Second, 1000, 2).Read(context.Background(), []byte("PK..."), "application/epub+zip")
	if err != nil {
		t.Fatal(err)
	}

	if got.Method != http.MethodPut || got.URL.Path != "/rmeta/markdown" || got.Header.Get("Content-Type") != "application/epub+zip" ||
		got.Header.Get("Accept") != "application/json" || string(sent) != "PK..." {
		t.Errorf("request %s %s %v, body %q", got.Method, got.URL.Path, got.Header, sent)
	}

	if doc.Title != "The Art of War" || doc.Text != "## Chapter I\n\nSun Tzŭ said: [1] The art of war & peace..." {
		t.Errorf("title %q, text %q", doc.Title, doc.Text)
	}
}

func TestTitleFallsBackToTheFirstLine(t *testing.T) {
	var (
		got  http.Request
		sent []byte
	)

	srv := server(t, http.StatusOK, `[{"tk:content": ["# Deep Residual Learning\n\nAbstract."]}]`, &got, &sent)

	doc, err := New(srv.URL, time.Second, 1000, 2).Read(context.Background(), []byte("%PDF"), "application/pdf")
	if err != nil || doc.Title != "Deep Residual Learning" {
		t.Errorf("title %q, %v", doc.Title, err)
	}
}

func TestLongDocumentsAreCutAtAParagraph(t *testing.T) {
	text := strings.Repeat("word ", 180) + "\n\n" + strings.Repeat("more ", 30)

	if got := cut(text, 1000); !strings.HasSuffix(got, "word") || len([]rune(got)) > 1000 {
		t.Errorf("cut to %d runes ending %q", len([]rune(got)), got[len(got)-10:])
	}

	if got := cut("short", 1000); got != "short" {
		t.Errorf("short text changed: %q", got)
	}
}

func TestFailures(t *testing.T) {
	var (
		got  http.Request
		sent []byte
	)

	for body, want := range map[string]error{
		`[{"tk:content": "  \n\n "}]`: ErrNoText,
		`[]`:                          ErrNoText,
		`not json`:                    ErrUnavailable,
	} {
		srv := server(t, http.StatusOK, body, &got, &sent)
		if _, err := New(srv.URL, time.Second, 100, 2).Read(context.Background(), nil, ""); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", body, err, want)
		}
	}

	srv := server(t, http.StatusUnprocessableEntity, "encrypted document", &got, &sent)
	if _, err := New(srv.URL, time.Second, 100, 2).Read(context.Background(), nil, ""); !errors.Is(err, ErrUnavailable) ||
		!strings.Contains(err.Error(), "encrypted") {
		t.Errorf("422: %v", err)
	}

	if _, err := New("http://127.0.0.1:1", time.Second, 100, 2).Read(context.Background(), nil, ""); !errors.Is(err, ErrUnavailable) {
		t.Errorf("down: %v", err)
	}
}

// A reply too long to hold is refused, not read; the crawler leaves the
// document unread instead of running out of memory.
func TestAnOverlongReplyIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"tk:content": "`))

		chunk := []byte(strings.Repeat("a", 1<<20))
		for range maxReplyBytes>>20 + 2 {
			_, _ = w.Write(chunk)
		}

		_, _ = w.Write([]byte(`"}]`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, 10*time.Second, 100, 2).Read(context.Background(), []byte("%PDF"), "application/pdf"); !errors.Is(err, ErrTooLong) {
		t.Errorf("err = %v", err)
	}
}

// Documents past the concurrent limit wait for a slot, and give up with their
// context.
func TestReadsWaitForASlot(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(`[{"tk:content": "text of the document"}]`))
	}))
	defer srv.Close()
	defer close(release)

	c := New(srv.URL, 10*time.Second, 100, 1)

	go func() { _, _ = c.Read(context.Background(), []byte("%PDF"), "application/pdf") }()

	for len(c.slots) == 0 {
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := c.Read(ctx, []byte("%PDF"), "application/pdf"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a second read while the only slot is taken: %v", err)
	}
}

func TestCutWalksRunes(t *testing.T) {
	text := strings.Repeat("ж", 80) + "\n\n" + strings.Repeat("ы", 500)

	if got := cut(text, 100); got != strings.Repeat("ж", 80) {
		t.Errorf("kept %d runes", utf8.RuneCountInString(got))
	}

	if got := cut("short", 100); got != "short" {
		t.Errorf("short: %q", got)
	}
}
