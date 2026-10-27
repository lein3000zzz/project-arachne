package searchapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"web-crawler/internal/answer"
	"web-crawler/internal/llm"
	"web-crawler/internal/retrieval"

	"go.uber.org/zap"
)

type fakeAnswerer struct {
	err error
	got answer.Request
}

func (f *fakeAnswerer) Answer(_ context.Context, req answer.Request, events answer.Events) (answer.Result, error) {
	f.got = req

	sources := []answer.Source{{N: 1, Result: retrieval.Result{URL: "https://go.dev/", Title: "Go"}}}
	if events.Sources != nil {
		_ = events.Sources(sources)
	}

	if f.err != nil {
		return answer.Result{}, f.err
	}

	for _, d := range []string{"Use ", "channels\n", "[1]."} {
		if events.Delta != nil {
			_ = events.Delta(d)
		}
	}

	return answer.Result{Answer: "Use channels\n[1].", Sources: sources, Cited: []int{1}, Model: "m", Usage: llm.Usage{OutputTokens: 3}}, nil
}

func serveAnswer(answerer Answerer, target string) *httptest.ResponseRecorder {
	services := Services{Searcher: &fakeSearcher{}}
	if answerer != nil {
		services.Answerer = answerer
	}

	handler := NewHandler(zap.NewNop().Sugar(), services,
		Config{DefaultLimit: 10, MaxLimit: 50, RequestTimeout: time.Second, AnswerTimeout: time.Second})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))

	return rec
}

func TestAnswerJSON(t *testing.T) {
	answerer := &fakeAnswerer{}

	rec := serveAnswer(answerer, "/answer?q=goroutines&host=go.dev&mode=bm25")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	if answerer.got != (answer.Request{Query: "goroutines", Host: "go.dev", Mode: retrieval.ModeBM25}) {
		t.Errorf("request = %+v", answerer.got)
	}

	var res answer.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.Answer != "Use channels\n[1]." || len(res.Sources) != 1 {
		t.Errorf("result %+v, %v", res, err)
	}
}

func TestAnswerStream(t *testing.T) {
	rec := serveAnswer(&fakeAnswerer{}, "/answer/stream?q=goroutines")

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}

	body := rec.Body.String()

	var events []string
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		events = append(events, strings.TrimPrefix(strings.SplitN(block, "\n", 2)[0], "event: "))
	}

	if strings.Join(events, ",") != "sources,delta,delta,delta,done" {
		t.Errorf("events = %v\n%s", events, body)
	}

	if !strings.Contains(body, `data: {"text":"channels\n"}`) {
		t.Errorf("a newline inside a delta must be JSON-escaped, not break the event:\n%s", body)
	}
}

func TestAnswerFailures(t *testing.T) {
	if rec := serveAnswer(nil, "/answer?q=x"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no chat model: %d", rec.Code)
	}

	if rec := serveAnswer(&fakeAnswerer{}, "/answer?q="); rec.Code != http.StatusBadRequest {
		t.Errorf("empty query: %d", rec.Code)
	}

	if rec := serveAnswer(&fakeAnswerer{err: answer.ErrNoSources}, "/answer?q=x"); rec.Code != http.StatusNotFound {
		t.Errorf("nothing found: %d", rec.Code)
	}

	rec := serveAnswer(&fakeAnswerer{err: errors.New("ollama down")}, "/answer/stream?q=x")
	if body := rec.Body.String(); !strings.Contains(body, "event: sources") || !strings.Contains(body, "event: error\ndata: {\"error\":\"answering failed\"}") {
		t.Errorf("stream failure:\n%s", body)
	}
}

func TestIndexPageIsServed(t *testing.T) {
	rec := serveAnswer(nil, "/")

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>arachne</title>") {
		t.Errorf("status %d", rec.Code)
	}

	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}

	if rec := serveAnswer(nil, "/favicon.svg"); rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/svg+xml" {
		t.Errorf("favicon: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}

	if rec := serveAnswer(nil, "/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path: %d", rec.Code)
	}
}
