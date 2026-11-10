package searchapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"web-crawler/internal/research"

	"go.uber.org/zap"
)

// ResearchService is optional: without it, labs are not researched.
type ResearchService interface {
	Start(ctx context.Context, ownerID, labID int64, ask research.Ask) (research.Run, error)
	Map(ctx context.Context, ownerID, labID int64) (research.Map, error)
	SetVerdict(ctx context.Context, ownerID, labID, workID int64, verdict string, subtopicID int64) (research.Work, error)
	ReadWork(ctx context.Context, ownerID, labID, workID int64) (research.Work, error)
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
	// Follow this work's citations alone, or research this subtopic alone.
	Work     int64 `json:"work"`
	Subtopic int64 `json:"subtopic"`
}

type verdictRequest struct {
	Verdict  string `json:"verdict"`
	Subtopic int64  `json:"subtopic"`
}

func (h *researchHandler) start(w http.ResponseWriter, r *http.Request) {
	var req researchRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {request?, found?, read?, work?, subtopic?}") {
		return
	}

	h.withLab(w, r, func(ctx context.Context, owner, lab int64) (int, any, error) {
		run, err := h.service.Start(ctx, owner, lab, research.Ask{Request: req.Request, Found: req.Found, Read: req.Read,
			Focus: research.Focus{Work: req.Work, Subtopic: req.Subtopic}})

		return http.StatusAccepted, run, err
	})
}

// setVerdict is the user keeping, putting aside or dropping a work.
func (h *researchHandler) setVerdict(w http.ResponseWriter, r *http.Request) {
	var req verdictRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {verdict: keep|maybe|drop, subtopic?}") {
		return
	}

	h.withWork(w, r, func(ctx context.Context, owner, lab, work int64) (int, any, error) {
		updated, err := h.service.SetVerdict(ctx, owner, lab, work, req.Verdict, req.Subtopic)
		return http.StatusOK, updated, err
	})
}

// readWork reads a work into the lab now.
func (h *researchHandler) readWork(w http.ResponseWriter, r *http.Request) {
	h.withWork(w, r, func(ctx context.Context, owner, lab, work int64) (int, any, error) {
		read, err := h.service.ReadWork(ctx, owner, lab, work)
		return http.StatusOK, read, err
	})
}

func (h *researchHandler) withWork(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, owner, lab, work int64) (int, any, error)) {
	work, err := strconv.ParseInt(r.PathValue("work"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: research.ErrWorkNotFound.Error()})
		return
	}

	h.withLab(w, r, func(ctx context.Context, owner, lab int64) (int, any, error) { return do(ctx, owner, lab, work) })
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
		case errors.Is(err, research.ErrNoChannels), errors.Is(err, research.ErrNothingToFollow):
			return 0, nil, statusError{http.StatusBadRequest, err}
		case errors.Is(err, research.ErrWorkNotFound), errors.Is(err, research.ErrSubtopicNotFound):
			return 0, nil, statusError{http.StatusNotFound, err}
		case errors.Is(err, research.ErrUnavailable):
			return 0, nil, statusError{http.StatusServiceUnavailable, err}
		}

		return status, body, err
	})
}
