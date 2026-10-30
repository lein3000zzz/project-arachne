package searchapi

import (
	"context"
	"errors"
	"net/http"
	"time"
	"web-crawler/internal/gathering"
	"web-crawler/internal/websearch"

	"go.uber.org/zap"
)

// Gatherer is optional: without it labs do not search the web.
type Gatherer interface {
	Engines(ctx context.Context) (available, defaults []string, err error)
	Start(ctx context.Context, ownerID, labID int64, request string) (gathering.Gathering, error)
	Latest(ctx context.Context, ownerID, labID int64) (gathering.Gathering, error)
}

const enginesTimeout = 10 * time.Second

type gatherHandler struct {
	logger  *zap.SugaredLogger
	service Gatherer
	labs    *labsHandler
}

type gatherRequest struct {
	Request string `json:"request"`
}

// engines lists what a lab may search; none at all when web search is off.
func (h *gatherHandler) engines(w http.ResponseWriter, r *http.Request) {
	none := map[string][]string{"available": {}, "default": {}}

	if h.service == nil {
		writeJSON(w, http.StatusOK, none)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), enginesTimeout)
	defer cancel()

	available, defaults, err := h.service.Engines(ctx)
	if err != nil {
		h.logger.Warnw("Web search engines unavailable", "error", err)
		writeJSON(w, http.StatusOK, none)

		return
	}

	writeJSON(w, http.StatusOK, map[string][]string{"available": available, "default": defaults})
}

func (h *gatherHandler) start(w http.ResponseWriter, r *http.Request) {
	var req gatherRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {request?}") {
		return
	}

	h.withLab(w, r, func(ctx context.Context, owner, lab int64) (int, any, error) {
		g, err := h.service.Start(ctx, owner, lab, req.Request)
		return http.StatusAccepted, g, err
	})
}

func (h *gatherHandler) latest(w http.ResponseWriter, r *http.Request) {
	h.withLab(w, r, func(ctx context.Context, owner, lab int64) (int, any, error) {
		g, err := h.service.Latest(ctx, owner, lab)
		return http.StatusOK, g, err
	})
}

func (h *gatherHandler) withLab(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, owner, lab int64) (int, any, error)) {
	if h.service == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "web search is not available"})
		return
	}

	h.labs.withLab(w, r, func(ctx context.Context, owner, lab int64) (int, any, error) {
		status, body, err := do(ctx, owner, lab)

		switch {
		case errors.Is(err, gathering.ErrRunning):
			return 0, nil, statusError{http.StatusConflict, err}
		case errors.Is(err, gathering.ErrNoEngines):
			return 0, nil, statusError{http.StatusBadRequest, err}
		case errors.Is(err, gathering.ErrNotFound):
			return 0, nil, statusError{http.StatusNotFound, err}
		case errors.Is(err, gathering.ErrUnavailable), errors.Is(err, websearch.ErrUnavailable):
			return 0, nil, statusError{http.StatusServiceUnavailable, err}
		}

		return status, body, err
	})
}

// statusError carries its own status through the lab handler's error mapping.
type statusError struct {
	status int
	err    error
}

func (e statusError) Error() string { return e.err.Error() }
func (e statusError) Unwrap() error { return e.err }
