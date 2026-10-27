package searchapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
	"web-crawler/internal/runs"

	"go.uber.org/zap"
)

// RunService is optional: without it crawl submission reports 503.
type RunService interface {
	Submit(ctx context.Context, req runs.Request) (runs.Run, error)
	List(ctx context.Context, limit int) ([]runs.Run, error)
	Get(ctx context.Context, id string) (runs.Run, error)
}

const (
	runsTimeout      = 10 * time.Second
	maxRunsListed    = 100
	maxRunBodyBytes  = 4 << 10
	defaultRunsShown = 20
)

type runsHandler struct {
	logger  *zap.SugaredLogger
	service RunService
}

func (h *runsHandler) submit(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) {
		return
	}

	var req runs.Request

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRunBodyBytes))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "body must be JSON: {url, depth?, links?, scope?, use_cache?}"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), runsTimeout)
	defer cancel()

	run, err := h.service.Submit(ctx, req)
	if err != nil {
		h.fail(w, err)
		return
	}

	writeJSON(w, http.StatusAccepted, run)
}

func (h *runsHandler) list(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) {
		return
	}

	limit := defaultRunsShown

	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxRunsListed {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "limit must be an integer in [1, 100]"})
			return
		}

		limit = n
	}

	ctx, cancel := context.WithTimeout(r.Context(), runsTimeout)
	defer cancel()

	list, err := h.service.List(ctx, limit)
	if err != nil {
		h.fail(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"runs": list})
}

func (h *runsHandler) get(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), runsTimeout)
	defer cancel()

	run, err := h.service.Get(ctx, r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}

	writeJSON(w, http.StatusOK, run)
}

func (h *runsHandler) available(w http.ResponseWriter) bool {
	if h.service == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "crawl submission is disabled"})
		return false
	}

	return true
}

func (h *runsHandler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runs.ErrInvalidRequest):
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
	case errors.Is(err, runs.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
	default:
		h.logger.Errorw("Run request failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "run request failed"})
	}
}
