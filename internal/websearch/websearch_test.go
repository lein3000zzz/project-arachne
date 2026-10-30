package websearch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSearchParsesAndCleansResults(t *testing.T) {
	var got *http.Request

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = w.Write([]byte(`{"results": [
			{"url": "https://Go.dev/doc/#top", "title": "Docs\n Go", "content": "  the  docs ", "engine": "brave", "engines": ["brave", "bing"]},
			{"url": "https://go.dev/doc/", "title": "dup", "engine": "bing"},
			{"url": "magnet:?xt=urn:btih:abc", "title": "torrent", "engine": "bing"},
			{"url": "https://example.com/a?utm_source=x", "title": "A", "engine": "wikipedia"}
		], "unresponsive_engines": [["duckduckgo", "CAPTCHA"], ["qwant", "access denied"]]}`))
	}))
	defer srv.Close()

	resp, err := New(srv.URL+"/", time.Second).Search(context.Background(), "go docs", []string{"brave", "bing"})
	if err != nil {
		t.Fatal(err)
	}

	q := got.URL.Query()
	if got.URL.Path != "/search" || q.Get("q") != "go docs" || q.Get("format") != "json" || q.Get("engines") != "brave,bing" ||
		got.Header.Get("X-Real-IP") == "" {
		t.Errorf("request %s %v", got.URL, got.Header)
	}

	if len(resp.Results) != 2 || resp.Results[0].URL != "https://go.dev/doc/" || resp.Results[0].Title != "Docs Go" ||
		resp.Results[0].Snippet != "the docs" || strings.Join(resp.Results[0].Engines, ",") != "brave,bing" ||
		resp.Results[1].URL != "https://example.com/a" || resp.Results[1].Engines[0] != "wikipedia" {
		t.Errorf("results = %+v: canonical, each URL once, web pages only", resp.Results)
	}

	if resp.Failed["duckduckgo"] != "CAPTCHA" || len(resp.Failed) != 2 {
		t.Errorf("failed = %v", resp.Failed)
	}
}

func TestEnginesAreTheGeneralWebOnes(t *testing.T) {
	calls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"engines": [
			{"name": "wikipedia", "enabled": true, "categories": ["general"]},
			{"name": "brave", "enabled": true, "categories": ["general", "web"]},
			{"name": "lingva", "enabled": true, "categories": ["general", "translate"]},
			{"name": "currency", "enabled": true, "categories": ["currency", "general"]},
			{"name": "mojeek", "enabled": false, "categories": ["general", "web"]},
			{"name": "bing images", "enabled": true, "categories": ["images", "web"]}
		]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, time.Second)

	engines, err := c.Engines(context.Background())
	if err != nil || strings.Join(engines, ",") != "brave,wikipedia" {
		t.Errorf("engines %v, %v", engines, err)
	}

	if _, _ = c.Engines(context.Background()); calls != 1 {
		t.Errorf("%d requests: the list is cached", calls)
	}
}

func TestAnUnreachableInstanceIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	if _, err := New(srv.URL, time.Second).Search(context.Background(), "q", nil); !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v", err)
	}

	if _, err := New("http://127.0.0.1:1", time.Second).Engines(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
}
