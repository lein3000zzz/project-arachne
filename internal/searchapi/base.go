package searchapi

import (
	"context"
	"net/http"
	"time"
	"web-crawler/internal/pages"

	"go.uber.org/zap"
)

// BaseStats counts the shared base; optional, as a deployment without
// Postgres-backed pages has nothing to count.
type BaseStats interface {
	Stats(ctx context.Context) (pages.Stats, error)
}

const baseTimeout = 5 * time.Second

type baseHandler struct {
	logger *zap.SugaredLogger
	stats  BaseStats
}

func (h *baseHandler) get(w http.ResponseWriter, r *http.Request) {
	if h.stats == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "no stats of the shared base here"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), baseTimeout)
	defer cancel()

	s, err := h.stats.Stats(ctx)
	if err != nil {
		h.logger.Warnw("Failed to count the shared base", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "the shared base could not be counted"})

		return
	}

	writeJSON(w, http.StatusOK, s)
}
