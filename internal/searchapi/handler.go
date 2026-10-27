// Package searchapi serves retrieval over HTTP.
package searchapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
	"web-crawler/internal/retrieval"

	"go.uber.org/zap"
)

type Searcher interface {
	Search(ctx context.Context, req retrieval.Request) ([]retrieval.Result, error)
}

type Config struct {
	DefaultLimit   int
	MaxLimit       int
	RequestTimeout time.Duration
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

func NewHandler(logger *zap.SugaredLogger, searcher Searcher, cfg Config) http.Handler {
	mux := http.NewServeMux()

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
		case errors.Is(err, retrieval.ErrEmptyQuery), errors.Is(err, retrieval.ErrInvalidRequest), errors.Is(err, retrieval.ErrUnknownMode):
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

	return req, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
