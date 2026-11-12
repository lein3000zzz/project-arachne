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
	Start(ctx context.Context, ownerID, labID string, ask research.Ask) (research.Run, error)
	Map(ctx context.Context, ownerID, labID string) (research.Map, error)
	SetVerdict(ctx context.Context, ownerID, labID, workID, verdict, subtopicID string) (research.Work, error)
	ReadWork(ctx context.Context, ownerID, labID, workID string) (research.Work, error)
	RenameSubtopic(ctx context.Context, ownerID, labID, subtopicID, name, focus string) (research.Subtopic, error)
	MergeSubtopics(ctx context.Context, ownerID, labID, from, into string) error
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
	Work     string `json:"work"`
	Subtopic string `json:"subtopic"`
}

type verdictRequest struct {
	Verdict  string `json:"verdict"`
	Subtopic string `json:"subtopic"`
}

func (h *researchHandler) start(w http.ResponseWriter, r *http.Request) {
	var req researchRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {request?, found?, read?, work?, subtopic?}") {
		return
	}

	work, workOK := optionalID(req.Work)
	subtopic, subtopicOK := optionalID(req.Subtopic)

	h.withLab(w, r, func(ctx context.Context, owner, lab string) (int, any, error) {
		switch {
		case !workOK:
			return 0, nil, research.ErrWorkNotFound
		case !subtopicOK:
			return 0, nil, research.ErrSubtopicNotFound
		}

		run, err := h.service.Start(ctx, owner, lab, research.Ask{Request: req.Request, Found: req.Found, Read: req.Read,
			Focus: research.Focus{Work: work, Subtopic: subtopic}})

		return http.StatusAccepted, run, err
	})
}

// setVerdict is the user keeping, putting aside or dropping a work.
func (h *researchHandler) setVerdict(w http.ResponseWriter, r *http.Request) {
	var req verdictRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {verdict: keep|maybe|drop, subtopic?}") {
		return
	}

	subtopic, subtopicOK := optionalID(req.Subtopic)

	h.withWork(w, r, func(ctx context.Context, owner, lab, work string) (int, any, error) {
		if !subtopicOK {
			return 0, nil, research.ErrSubtopicNotFound
		}

		updated, err := h.service.SetVerdict(ctx, owner, lab, work, req.Verdict, subtopic)
		return http.StatusOK, updated, err
	})
}

type subtopicRequest struct {
	Name  string `json:"name"`
	Focus string `json:"focus"`
}

type mergeRequest struct {
	Into string `json:"into"`
}

// renameSubtopic is the user naming a subtopic the model drafted.
func (h *researchHandler) renameSubtopic(w http.ResponseWriter, r *http.Request) {
	var req subtopicRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {name, focus?}") {
		return
	}

	h.withSubtopic(w, r, func(ctx context.Context, owner, lab, subtopic string) (int, any, error) {
		sub, err := h.service.RenameSubtopic(ctx, owner, lab, subtopic, req.Name, req.Focus)
		return http.StatusOK, sub, err
	})
}

// mergeSubtopic folds the subtopic of the path into another.
func (h *researchHandler) mergeSubtopic(w http.ResponseWriter, r *http.Request) {
	var req mergeRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {into}") {
		return
	}

	into, ok := parseID(req.Into)

	h.withSubtopic(w, r, func(ctx context.Context, owner, lab, subtopic string) (int, any, error) {
		if !ok {
			return 0, nil, research.ErrSubtopicNotFound
		}

		return http.StatusOK, map[string]string{"into": into}, h.service.MergeSubtopics(ctx, owner, lab, subtopic, into)
	})
}

func (h *researchHandler) withSubtopic(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, owner, lab, subtopic string) (int, any, error)) {
	subtopic, ok := parseID(r.PathValue("subtopic"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: research.ErrSubtopicNotFound.Error()})
		return
	}

	h.withLab(w, r, func(ctx context.Context, owner, lab string) (int, any, error) { return do(ctx, owner, lab, subtopic) })
}

// readWork reads a work into the lab now.
func (h *researchHandler) readWork(w http.ResponseWriter, r *http.Request) {
	h.withWork(w, r, func(ctx context.Context, owner, lab, work string) (int, any, error) {
		read, err := h.service.ReadWork(ctx, owner, lab, work)
		return http.StatusOK, read, err
	})
}

func (h *researchHandler) withWork(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, owner, lab, work string) (int, any, error)) {
	work, ok := parseID(r.PathValue("work"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: research.ErrWorkNotFound.Error()})
		return
	}

	h.withLab(w, r, func(ctx context.Context, owner, lab string) (int, any, error) { return do(ctx, owner, lab, work) })
}

// researchMap is the lab's map: subtopics, works, the citations between
// them, how complete it looks, and the latest run.
func (h *researchHandler) researchMap(w http.ResponseWriter, r *http.Request) {
	h.withLab(w, r, func(ctx context.Context, owner, lab string) (int, any, error) {
		m, err := h.service.Map(ctx, owner, lab)
		return http.StatusOK, m, err
	})
}

func (h *researchHandler) withLab(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, owner, lab string) (int, any, error)) {
	if h.service == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "research is not available"})
		return
	}

	h.labs.withLab(w, r, func(ctx context.Context, owner, lab string) (int, any, error) {
		status, body, err := do(ctx, owner, lab)

		switch {
		case errors.Is(err, research.ErrRunning), errors.Is(err, research.ErrSubtopicTaken):
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
