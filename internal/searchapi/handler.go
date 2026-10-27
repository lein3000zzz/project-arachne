// Package searchapi serves retrieval over HTTP.
package searchapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"web-crawler/internal/answer"
	"web-crawler/internal/retrieval"

	"go.uber.org/zap"
)

type Searcher interface {
	Search(ctx context.Context, req retrieval.Request) ([]retrieval.Result, error)
}

// Answerer is optional: without a chat model the answer endpoints report 503.
type Answerer interface {
	Answer(ctx context.Context, req answer.Request, events answer.Events) (answer.Result, error)
}

type Config struct {
	DefaultLimit   int
	MaxLimit       int
	RequestTimeout time.Duration
	AnswerTimeout  time.Duration
}

type Response struct {
	Query   string             `json:"query"`
	Mode    retrieval.Mode     `json:"mode"`
	Host    string             `json:"host,omitempty"`
	TookMS  int64              `json:"took_ms"`
	Results []retrieval.Result `json:"results"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// Services are what the API serves; Answerer and Runs may be nil.
type Services struct {
	Searcher Searcher
	Answerer Answerer
	Runs     RunService
}

func NewHandler(logger *zap.SugaredLogger, services Services, cfg Config) http.Handler {
	mux := http.NewServeMux()
	searcher := services.Searcher

	answers := &answerHandler{logger: logger, answerer: services.Answerer, timeout: cfg.AnswerTimeout}
	mux.HandleFunc("GET /answer", answers.serveJSON)
	mux.HandleFunc("GET /answer/stream", answers.serveStream)

	runsAPI := &runsHandler{logger: logger, service: services.Runs}
	mux.HandleFunc("POST /api/runs", runsAPI.submit)
	mux.HandleFunc("GET /api/runs", runsAPI.list)
	mux.HandleFunc("GET /api/runs/{id}", runsAPI.get)

	mux.HandleFunc("GET /search", func(w http.ResponseWriter, r *http.Request) {
		req, err := parseRequest(r, cfg)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), cfg.RequestTimeout)
		defer cancel()

		started := time.Now()

		results, err := searcher.Search(ctx, req)

		switch {
		case err == nil:
		case errors.Is(err, retrieval.ErrEmptyQuery), errors.Is(err, retrieval.ErrInvalidRequest), errors.Is(err, retrieval.ErrUnknownMode),
			errors.Is(err, retrieval.ErrNoReranker):
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		case errors.Is(err, context.DeadlineExceeded):
			writeJSON(w, http.StatusGatewayTimeout, errorResponse{Error: "search timed out"})
			return
		default:
			logger.Errorw("Search failed", "query", req.Query, "mode", req.Mode, "error", err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "search failed"})
			return
		}

		writeJSON(w, http.StatusOK, Response{
			Query:   req.Query,
			Mode:    req.Mode,
			Host:    req.Host,
			TookMS:  time.Since(started).Milliseconds(),
			Results: results,
		})
	})

	mux.HandleFunc("GET /{$}", serveIndex)
	mux.HandleFunc("GET /favicon.svg", serveFavicon)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	return mux
}

func parseRequest(r *http.Request, cfg Config) (retrieval.Request, error) {
	q := r.URL.Query()

	req := retrieval.Request{
		Query: q.Get("q"),
		Host:  q.Get("host"),
		Mode:  retrieval.ModeHybrid,
		Limit: cfg.DefaultLimit,
	}

	if mode := q.Get("mode"); mode != "" {
		req.Mode = retrieval.Mode(mode)
	}

	if raw := q.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > cfg.MaxLimit {
			return req, fmt.Errorf("limit must be an integer in [1, %d]", cfg.MaxLimit)
		}

		req.Limit = limit
	}

	var err error

	req.PageRankWeight, req.SiteRankWeight, req.Rerank, err = rankingOptions(q)

	return req, err
}

// rankingOptions reads the optional pagerank and siterank weights and the rerank
// switch; an absent parameter leaves the configured default.
func rankingOptions(q url.Values) (pageWeight, siteWeight *float64, rerank *bool, err error) {
	weight := func(name string) (*float64, error) {
		raw := q.Get(name)
		if raw == "" {
			return nil, nil
		}

		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) {
			return nil, fmt.Errorf("%s must be a number >= 0", name)
		}

		return &v, nil
	}

	if pageWeight, err = weight("pagerank"); err != nil {
		return nil, nil, nil, err
	}

	if siteWeight, err = weight("siterank"); err != nil {
		return nil, nil, nil, err
	}

	if raw := q.Get("rerank"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("rerank must be true or false")
		}

		rerank = &v
	}

	return pageWeight, siteWeight, rerank, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
