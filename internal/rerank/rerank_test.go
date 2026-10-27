package rerank

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func client(t *testing.T, url string) *Client {
	t.Helper()

	c, err := New(Config{BaseURL: url + "/v1/", APIKey: "k", Model: "bge", RequestTimeout: time.Second, MaxRetries: 2})
	if err != nil {
		t.Fatal(err)
	}

	c.retryDelay = time.Millisecond

	return c
}

func TestRerankReturnsScoresInDocumentOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req request
		_ = json.NewDecoder(r.Body).Decode(&req)

		if r.URL.Path != "/v1/rerank" || r.Header.Get("Authorization") != "Bearer k" || req.Query != "q" || req.TopN != 3 {
			t.Errorf("path %s auth %q body %+v", r.URL.Path, r.Header.Get("Authorization"), req)
		}

		_, _ = w.Write([]byte(`{"results":[{"index":2,"relevance_score":4.5},{"index":0,"relevance_score":-1.25},{"index":1,"relevance_score":-7}]}`))
	}))
	defer srv.Close()

	scores, err := client(t, srv.URL).Rerank(context.Background(), "q", []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(scores, []float64{-1.25, -7, 4.5}) {
		t.Errorf("scores = %v", scores)
	}
}

func TestRerankRejectsIncompleteAnswers(t *testing.T) {
	for _, body := range []string{
		`{"results":[{"index":0,"relevance_score":1}]}`,
		`{"results":[{"index":0,"relevance_score":1},{"index":0,"relevance_score":2}]}`,
		`{"results":[{"index":0,"relevance_score":1},{"index":5,"relevance_score":2}]}`,
		`{"results":[{"index":0,"relevance_score":1},{"index":1}]}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))

		if _, err := client(t, srv.URL).Rerank(context.Background(), "q", []string{"a", "b"}); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v", body, err)
		}

		srv.Close()
	}
}

func TestRerankRetriesServerErrors(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "loading model", http.StatusServiceUnavailable)
			return
		}

		_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":1}]}`))
	}))
	defer srv.Close()

	if _, err := client(t, srv.URL).Rerank(context.Background(), "q", []string{"a"}); err != nil || calls.Load() != 3 {
		t.Errorf("err %v after %d calls", err, calls.Load())
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "bad", http.StatusBadRequest)
	}))
	defer bad.Close()

	calls.Store(0)

	if _, err := client(t, bad.URL).Rerank(context.Background(), "q", []string{"a"}); !errors.Is(err, ErrRequest) || calls.Load() != 1 {
		t.Errorf("a 400 must not be retried: %v after %d calls", err, calls.Load())
	}
}
