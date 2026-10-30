package searchapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
	"web-crawler/internal/retrieval"
)

type fakeSearcher struct {
	got retrieval.Request
	err error
}

func (f *fakeSearcher) Search(_ context.Context, req retrieval.Request) ([]retrieval.Result, error) {
	f.got = req
	if f.err != nil {
		return nil, f.err
	}

	return []retrieval.Result{{URL: "https://example.com/", Score: 0.5, Dense: &retrieval.Signal{Rank: 1, Score: 0.8}}}, nil
}

func serve(t *testing.T, searcher Searcher, target string) *httptest.ResponseRecorder {
	t.Helper()

	handler := newTestHandler(Services{Searcher: searcher}, Config{DefaultLimit: 10, MaxLimit: 50, RequestTimeout: time.Second})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, signedIn(httptest.NewRequest(http.MethodGet, target, nil)))

	return rec
}

func TestSearchPassesTheRequestThrough(t *testing.T) {
	searcher := &fakeSearcher{}

	rec := serve(t, searcher, "/search?q=goroutines+leak&limit=5&host=go.dev&mode=bm25")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	want := retrieval.Request{Query: "goroutines leak", Limit: 5, Host: "go.dev", Mode: retrieval.ModeBM25}
	if !reflect.DeepEqual(searcher.got, want) {
		t.Errorf("request = %+v, want %+v", searcher.got, want)
	}

	var resp Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	if len(resp.Results) != 1 || resp.Results[0].Dense.Score != 0.8 || resp.Results[0].BM25 != nil {
		t.Errorf("results = %+v", resp.Results)
	}
}

func TestSearchDefaults(t *testing.T) {
	searcher := &fakeSearcher{}

	if rec := serve(t, searcher, "/search?q=x"); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	if searcher.got.Mode != retrieval.ModeHybrid || searcher.got.Limit != 10 {
		t.Errorf("defaults = %+v", searcher.got)
	}
}

func TestSearchStatusCodes(t *testing.T) {
	cases := []struct {
		target string
		err    error
		want   int
	}{
		{"/search?q=x&limit=0", nil, http.StatusBadRequest},
		{"/search?q=x&limit=51", nil, http.StatusBadRequest},
		{"/search?q=x&limit=ten", nil, http.StatusBadRequest},
		{"/search?q=x", retrieval.ErrEmptyQuery, http.StatusBadRequest},
		{"/search?q=x&mode=magic", fmt.Errorf("%w: magic", retrieval.ErrUnknownMode), http.StatusBadRequest},
		{"/search?q=x", context.DeadlineExceeded, http.StatusGatewayTimeout},
		{"/search?q=x", errors.New("milvus down"), http.StatusInternalServerError},
	}

	for _, tc := range cases {
		if rec := serve(t, &fakeSearcher{err: tc.err}, tc.target); rec.Code != tc.want {
			t.Errorf("%s with %v: status %d, want %d", tc.target, tc.err, rec.Code, tc.want)
		}
	}

	if rec := serve(t, &fakeSearcher{}, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("healthz: %d", rec.Code)
	}
}

func TestSearchRankingOptions(t *testing.T) {
	searcher := &fakeSearcher{}

	if rec := serve(t, searcher, "/search?q=x&pagerank=0.5&siterank=0&rerank=false"); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	got := searcher.got
	if got.PageRankWeight == nil || *got.PageRankWeight != 0.5 || got.SiteRankWeight == nil || *got.SiteRankWeight != 0 ||
		got.Rerank == nil || *got.Rerank {
		t.Errorf("request = %+v", got)
	}

	if rec := serve(t, searcher, "/search?q=x"); rec.Code != http.StatusOK || searcher.got.PageRankWeight != nil || searcher.got.Rerank != nil {
		t.Errorf("absent options must stay unset, so the configured defaults apply: %+v", searcher.got)
	}

	for _, bad := range []string{"pagerank=-1", "pagerank=NaN", "siterank=lots", "rerank=maybe"} {
		if rec := serve(t, searcher, "/search?q=x&"+bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", bad, rec.Code)
		}
	}

	if rec := serve(t, &fakeSearcher{err: retrieval.ErrNoReranker}, "/search?q=x&rerank=true"); rec.Code != http.StatusBadRequest {
		t.Errorf("rerank without a reranker: %d", rec.Code)
	}
}
