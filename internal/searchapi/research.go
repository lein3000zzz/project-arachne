package searchapi

import (
	"context"
	"errors"
	"net/http"
	"web-crawler/internal/research"

	"go.uber.org/zap"
)

// ResearchService is optional: without it, labs are not researched.
type ResearchService interface {
	Start(ctx context.Context, ownerID, labID int64, ask research.Ask) (research.Run, error)
	Map(ctx context.Context, ownerID, labID int64) (research.Map, error)
}

type researchHandler struct {
	logger  *zap.SugaredLogger
	service ResearchService
	labs    *labsHandler
}

type researchRequest struct {
	Request string `json:"request"`
	// Absent takes the server's default; a read of 0 maps without reading.
	Found *int `json:"found"`
	Read  *int `json:"read"`
}

func (h *researchHandler) start(w http.ResponseWriter, r *http.Request) {
	var req researchRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {request?, found?, read?}") {
		return
	}

	h.withLab(w, r, func(ctx context.Context, owner, lab int64) (int, any, error) {
		run, err := h.service.Start(ctx, owner, lab, research.Ask{Request: req.Request, Found: req.Found, Read: req.Read})
		return http.StatusAccepted, run, err
	})
}

// researchMap is the lab's map: subtopics, works, the citations between
// them, how complete it looks, and the latest run.
func (h *researchHandler) researchMap(w http.ResponseWriter, r *http.Request) {
	h.withLab(w, r, func(ctx context.Context, owner, lab int64) (int, any, error) {
		m, err := h.service.Map(ctx, owner, lab)
		return http.StatusOK, m, err
	})
}

func (h *researchHandler) withLab(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, owner, lab int64) (int, any, error)) {
	if h.service == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "research is not available"})
		return
	}

	h.labs.withLab(w, r, func(ctx context.Context, owner, lab int64) (int, any, error) {
		status, body, err := do(ctx, owner, lab)

		switch {
		case errors.Is(err, research.ErrRunning):
			return 0, nil, statusError{http.StatusConflict, err}
		case errors.Is(err, research.ErrNoChannels):
			return 0, nil, statusError{http.StatusBadRequest, err}
		case errors.Is(err, research.ErrUnavailable):
			return 0, nil, statusError{http.StatusServiceUnavailable, err}
		}

		return status, body, err
	})
}
