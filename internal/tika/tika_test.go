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

	title, text, err := New(srv.URL+"/", time.Second, 1000).Read(context.Background(), []byte("PK..."), "application/epub+zip")
	if err != nil {
		t.Fatal(err)
	}

	if got.Method != http.MethodPut || got.URL.Path != "/rmeta/markdown" || got.Header.Get("Content-Type") != "application/epub+zip" ||
		got.Header.Get("Accept") != "application/json" || string(sent) != "PK..." {
		t.Errorf("request %s %s %v, body %q", got.Method, got.URL.Path, got.Header, sent)
	}

	if title != "The Art of War" || text != "## Chapter I\n\nSun Tzŭ said: [1] The art of war & peace..." {
		t.Errorf("title %q, text %q", title, text)
	}
}

func TestTitleFallsBackToTheFirstLine(t *testing.T) {
	var (
		got  http.Request
		sent []byte
	)

	srv := server(t, http.StatusOK, `[{"tk:content": ["# Deep Residual Learning\n\nAbstract."]}]`, &got, &sent)

	title, _, err := New(srv.URL, time.Second, 1000).Read(context.Background(), []byte("%PDF"), "application/pdf")
	if err != nil || title != "Deep Residual Learning" {
		t.Errorf("title %q, %v", title, err)
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
		if _, _, err := New(srv.URL, time.Second, 100).Read(context.Background(), nil, ""); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", body, err, want)
		}
	}

	srv := server(t, http.StatusUnprocessableEntity, "encrypted document", &got, &sent)
	if _, _, err := New(srv.URL, time.Second, 100).Read(context.Background(), nil, ""); !errors.Is(err, ErrUnavailable) ||
		!strings.Contains(err.Error(), "encrypted") {
		t.Errorf("422: %v", err)
	}

	if _, _, err := New("http://127.0.0.1:1", time.Second, 100).Read(context.Background(), nil, ""); !errors.Is(err, ErrUnavailable) {
		t.Errorf("down: %v", err)
	}
}
