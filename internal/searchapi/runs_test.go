package searchapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"web-crawler/internal/runs"
)

type fakeRuns struct {
	got runs.Request
	err error
}

func (f *fakeRuns) Submit(_ context.Context, req runs.Request) (runs.Run, error) {
	f.got = req
	return runs.Run{ID: "r1", StartURL: "https://go.dev/", Status: runs.StatusQueued}, f.err
}

func (f *fakeRuns) List(context.Context, int) ([]runs.Run, error) {
	return []runs.Run{{ID: "r1", Status: runs.StatusRunning, Pages: 7}}, f.err
}

func (f *fakeRuns) Get(_ context.Context, id string) (runs.Run, error) {
	if id != "r1" {
		return runs.Run{}, runs.ErrNotFound
	}

	return runs.Run{ID: "r1"}, f.err
}

func serveRuns(service RunService, method, target, body string) *httptest.ResponseRecorder {
	services := Services{Searcher: &fakeSearcher{}}
	if service != nil {
		services.Runs = service
	}

	handler := newTestHandler(services, Config{DefaultLimit: 10, MaxLimit: 50, RequestTimeout: time.Second})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, signedIn(httptest.NewRequest(method, target, strings.NewReader(body))))

	return rec
}

func TestSubmitRun(t *testing.T) {
	service := &fakeRuns{}

	rec := serveRuns(service, http.MethodPost, "/api/runs", `{"url":"go.dev","depth":2,"links":50,"scope":"section"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	if service.got != (runs.Request{URL: "go.dev", Depth: 2, Links: 50, Scope: "section"}) {
		t.Errorf("request = %+v", service.got)
	}

	var run runs.Run
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil || run.ID != "r1" || run.Status != runs.StatusQueued {
		t.Errorf("run %+v, %v", run, err)
	}
}

func TestRunsAPIErrors(t *testing.T) {
	cases := []struct {
		service      RunService
		method, path string
		body         string
		want         int
	}{
		{nil, http.MethodPost, "/api/runs", `{"url":"go.dev"}`, http.StatusServiceUnavailable},
		{&fakeRuns{}, http.MethodPost, "/api/runs", `not json`, http.StatusBadRequest},
		{&fakeRuns{}, http.MethodPost, "/api/runs", `{"url":"go.dev","depht":2}`, http.StatusBadRequest},
		{&fakeRuns{}, http.MethodPost, "/api/runs", `{"url":"` + strings.Repeat("a", 5000) + `"}`, http.StatusBadRequest},
		{&fakeRuns{err: fmt.Errorf("%w: depth", runs.ErrInvalidRequest)}, http.MethodPost, "/api/runs", `{"url":"go.dev"}`, http.StatusBadRequest},
		{&fakeRuns{err: errors.New("kafka down")}, http.MethodPost, "/api/runs", `{"url":"go.dev"}`, http.StatusInternalServerError},
		{&fakeRuns{}, http.MethodGet, "/api/runs?limit=0", "", http.StatusBadRequest},
		{&fakeRuns{}, http.MethodGet, "/api/runs/nope", "", http.StatusNotFound},
		{&fakeRuns{}, http.MethodGet, "/api/runs/r1", "", http.StatusOK},
	}

	for _, tc := range cases {
		if rec := serveRuns(tc.service, tc.method, tc.path, tc.body); rec.Code != tc.want {
			t.Errorf("%s %s %.40s: status %d, want %d", tc.method, tc.path, tc.body, rec.Code, tc.want)
		}
	}

	rec := serveRuns(&fakeRuns{}, http.MethodGet, "/api/runs", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"pages":7`) {
		t.Errorf("list: %d %s", rec.Code, rec.Body)
	}
}
