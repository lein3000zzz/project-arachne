package searchapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"web-crawler/internal/auth"
	"web-crawler/internal/labs"

	"go.uber.org/zap"
)

// LabService is optional: without it the lab endpoints report 503.
type LabService interface {
	List(ctx context.Context, ownerID string) ([]labs.Lab, error)
	Create(ctx context.Context, ownerID, name, topic string, sources []labs.SourceRequest, engines []string) (labs.Detail, error)
	SetEngines(ctx context.Context, ownerID, labID string, engines []string) (labs.Lab, error)
	Rename(ctx context.Context, ownerID, labID, name, topic string) (labs.Lab, error)
	Get(ctx context.Context, ownerID, labID string) (labs.Detail, error)
	Delete(ctx context.Context, ownerID, labID string) error
	AddSources(ctx context.Context, ownerID, labID string, sources []labs.SourceRequest) (labs.Detail, error)
	Refresh(ctx context.Context, ownerID, labID, sourceID string) (labs.SourceStatus, error)
	RemoveSource(ctx context.Context, ownerID, labID, sourceID string) error
	Pages(ctx context.Context, ownerID, labID string, limit int) ([]labs.Page, error)
	Quality(ctx context.Context, ownerID, labID string) (labs.Quality, error)
	RetryFailed(ctx context.Context, ownerID, labID string) (int64, error)
	References(ctx context.Context, ownerID, labID string) ([]labs.Document, error)
	AddReferences(ctx context.Context, ownerID, labID string, picks []labs.Cited) (labs.Detail, error)
	AddUploads(ctx context.Context, ownerID, labID string, files []labs.File) (labs.Detail, error)
	PageURLs(ctx context.Context, ownerID string, labIDs []string) ([]string, error)
	Plan(ctx context.Context, ownerID, labID, request string) (labs.Plan, error)
}

const (
	labsTimeout      = 15 * time.Second
	maxLabBodyBytes  = 32 << 10
	maxLabsPerQuery  = 20
	defaultLabPages  = 100
	maxLabPagesShown = 1000
)

type labsHandler struct {
	logger  *zap.SugaredLogger
	service LabService
	// Planning waits on the chat model.
	planTimeout time.Duration
}

type createLabRequest struct {
	Name    string               `json:"name"`
	Topic   string               `json:"topic"`
	Sources []labs.SourceRequest `json:"sources"`
	// Absent takes the defaults; [] means never searching the web.
	Engines []string `json:"engines"`
}

type enginesRequest struct {
	Engines []string `json:"engines"`
}

type addSourcesRequest struct {
	Sources []labs.SourceRequest `json:"sources"`
}

type addReferencesRequest struct {
	References []labs.Cited `json:"references"`
}

type planRequest struct {
	Request string `json:"request"`
	LabID   string `json:"lab_id"`
}

func (h *labsHandler) list(w http.ResponseWriter, r *http.Request) {
	h.run(w, r, labsTimeout, func(ctx context.Context, owner string) (int, any, error) {
		list, err := h.service.List(ctx, owner)
		return http.StatusOK, map[string]any{"labs": list}, err
	})
}

func (h *labsHandler) create(w http.ResponseWriter, r *http.Request) {
	var req createLabRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {name, topic, sources: [{url, scope?, depth?, links?}], engines?}") {
		return
	}

	h.run(w, r, labsTimeout, func(ctx context.Context, owner string) (int, any, error) {
		detail, err := h.service.Create(ctx, owner, req.Name, req.Topic, req.Sources, req.Engines)
		return http.StatusCreated, detail, err
	})
}

func (h *labsHandler) setEngines(w http.ResponseWriter, r *http.Request) {
	var req enginesRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {engines: [...]}") {
		return
	}

	h.withLab(w, r, func(ctx context.Context, owner, lab string) (int, any, error) {
		updated, err := h.service.SetEngines(ctx, owner, lab, req.Engines)
		return http.StatusOK, updated, err
	})
}

type renameRequest struct {
	Name  string `json:"name"`
	Topic string `json:"topic"`
}

func (h *labsHandler) rename(w http.ResponseWriter, r *http.Request) {
	var req renameRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {name, topic}") {
		return
	}

	h.withLab(w, r, func(ctx context.Context, owner, lab string) (int, any, error) {
		updated, err := h.service.Rename(ctx, owner, lab, req.Name, req.Topic)
		return http.StatusOK, updated, err
	})
}

func (h *labsHandler) plan(w http.ResponseWriter, r *http.Request) {
	var req planRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {request, lab_id?}") {
		return
	}

	lab, ok := optionalID(req.LabID)
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: labs.ErrNotFound.Error()})
		return
	}

	h.run(w, r, h.planTimeout, func(ctx context.Context, owner string) (int, any, error) {
		plan, err := h.service.Plan(ctx, owner, lab, req.Request)
		return http.StatusOK, plan, err
	})
}

func (h *labsHandler) get(w http.ResponseWriter, r *http.Request) {
	h.withLab(w, r, func(ctx context.Context, owner, lab string) (int, any, error) {
		detail, err := h.service.Get(ctx, owner, lab)
		return http.StatusOK, detail, err
	})
}

func (h *labsHandler) delete(w http.ResponseWriter, r *http.Request) {
	h.withLab(w, r, func(ctx context.Context, owner, lab string) (int, any, error) {
		return http.StatusNoContent, nil, h.service.Delete(ctx, owner, lab)
	})
}

func (h *labsHandler) addSources(w http.ResponseWriter, r *http.Request) {
	var req addSourcesRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {sources: [{url, scope?, depth?, links?}]}") {
		return
	}

	h.withLab(w, r, func(ctx context.Context, owner, lab string) (int, any, error) {
		detail, err := h.service.AddSources(ctx, owner, lab, req.Sources)
		return http.StatusOK, detail, err
	})
}

func (h *labsHandler) references(w http.ResponseWriter, r *http.Request) {
	h.withLab(w, r, func(ctx context.Context, owner, lab string) (int, any, error) {
		docs, err := h.service.References(ctx, owner, lab)
		return http.StatusOK, map[string]any{"documents": docs}, err
	})
}

func (h *labsHandler) addReferences(w http.ResponseWriter, r *http.Request) {
	var req addReferencesRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {references: [{document, url}]}") {
		return
	}

	h.withLab(w, r, func(ctx context.Context, owner, lab string) (int, any, error) {
		detail, err := h.service.AddReferences(ctx, owner, lab, req.References)
		return http.StatusOK, detail, err
	})
}

func (h *labsHandler) refresh(w http.ResponseWriter, r *http.Request) {
	h.withSource(w, r, func(ctx context.Context, owner, lab, source string) (int, any, error) {
		status, err := h.service.Refresh(ctx, owner, lab, source)
		return http.StatusAccepted, status, err
	})
}

func (h *labsHandler) removeSource(w http.ResponseWriter, r *http.Request) {
	h.withSource(w, r, func(ctx context.Context, owner, lab, source string) (int, any, error) {
		return http.StatusNoContent, nil, h.service.RemoveSource(ctx, owner, lab, source)
	})
}

// quality is how good the lab's material is: usable pages, what the rest is.
func (h *labsHandler) quality(w http.ResponseWriter, r *http.Request) {
	h.withLab(w, r, func(ctx context.Context, owner, lab string) (int, any, error) {
		q, err := h.service.Quality(ctx, owner, lab)
		return http.StatusOK, q, err
	})
}

// retryFailed gives the lab's pages that could not be indexed another round
// of tries: after the embedder or the reviewer's model was down a while.
func (h *labsHandler) retryFailed(w http.ResponseWriter, r *http.Request) {
	h.withLab(w, r, func(ctx context.Context, owner, lab string) (int, any, error) {
		n, err := h.service.RetryFailed(ctx, owner, lab)
		return http.StatusOK, map[string]int64{"retried": n}, err
	})
}

func (h *labsHandler) pages(w http.ResponseWriter, r *http.Request) {
	limit := defaultLabPages

	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLabPagesShown {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf("limit must be an integer in [1, %d]", maxLabPagesShown)})
			return
		}

		limit = n
	}

	h.withLab(w, r, func(ctx context.Context, owner, lab string) (int, any, error) {
		list, err := h.service.Pages(ctx, owner, lab, limit)
		return http.StatusOK, map[string]any{"pages": list}, err
	})
}

func (h *labsHandler) withLab(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, owner, lab string) (int, any, error)) {
	h.withLabFor(w, r, labsTimeout, do)
}

func (h *labsHandler) withLabFor(w http.ResponseWriter, r *http.Request, timeout time.Duration, do func(ctx context.Context, owner, lab string) (int, any, error)) {
	lab, ok := parseID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: labs.ErrNotFound.Error()})
		return
	}

	h.run(w, r, timeout, func(ctx context.Context, owner string) (int, any, error) { return do(ctx, owner, lab) })
}

func (h *labsHandler) withSource(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, owner, lab, source string) (int, any, error)) {
	source, ok := parseID(r.PathValue("source"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: labs.ErrNotFound.Error()})
		return
	}

	h.withLab(w, r, func(ctx context.Context, owner, lab string) (int, any, error) { return do(ctx, owner, lab, source) })
}

// run calls do as the signed-in user and writes what it returns.
func (h *labsHandler) run(w http.ResponseWriter, r *http.Request, timeout time.Duration, do func(ctx context.Context, owner string) (int, any, error)) {
	if h.service == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "labs are not available"})
		return
	}

	user, _ := auth.UserFrom(r.Context())

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	status, body, err := do(ctx, user.ID)
	if err != nil {
		status, message := h.failure(err)
		writeJSON(w, status, errorResponse{Error: message})

		return
	}

	if body == nil {
		w.WriteHeader(status)
		return
	}

	writeJSON(w, status, body)
}

func (h *labsHandler) failure(err error) (int, string) {
	return labFailure(h.logger, err)
}

func labFailure(logger *zap.SugaredLogger, err error) (int, string) {
	if se := (statusError{}); errors.As(err, &se) {
		return se.status, se.err.Error()
	}

	switch {
	case errors.Is(err, labs.ErrNotFound):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, labs.ErrNameTaken), errors.Is(err, labs.ErrSourceExists):
		return http.StatusConflict, err.Error()
	case errors.Is(err, labs.ErrInvalid):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, labs.ErrCrawlingDisabled), errors.Is(err, labs.ErrUploadsDisabled):
		return http.StatusServiceUnavailable, err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "the request timed out"
	}

	logger.Errorw("Lab request failed", "error", err)

	return http.StatusInternalServerError, "lab request failed"
}

// labURLs reads ?labs=<id>,<id> and returns those labs' pages, or nil when the
// parameter is absent. The labs must be the signed-in user's.
func labURLs(ctx context.Context, service LabService, r *http.Request) ([]string, error) {
	raw := r.URL.Query().Get("labs")
	if raw == "" {
		return nil, nil
	}

	if service == nil {
		return nil, fmt.Errorf("%w: labs are not available", labs.ErrInvalid)
	}

	parts := strings.Split(raw, ",")
	if len(parts) > maxLabsPerQuery {
		return nil, fmt.Errorf("%w: at most %d labs at a time", labs.ErrInvalid, maxLabsPerQuery)
	}

	ids := make([]string, len(parts))

	for i, part := range parts {
		id, ok := parseID(strings.TrimSpace(part))
		if !ok {
			return nil, fmt.Errorf("%w: labs must be lab IDs separated by commas", labs.ErrInvalid)
		}

		ids[i] = id
	}

	user, _ := auth.UserFrom(r.Context())

	return labPages(ctx, service, user.ID, ids)
}

// labPages is the pages of the owner's labs; a lab without pages gives an
// empty list, which searches nothing.
func labPages(ctx context.Context, service LabService, owner string, ids []string) ([]string, error) {
	switch {
	case service == nil:
		return nil, fmt.Errorf("%w: labs are not available", labs.ErrInvalid)
	case len(ids) > maxLabsPerQuery:
		return nil, fmt.Errorf("%w: at most %d labs at a time", labs.ErrInvalid, maxLabsPerQuery)
	}

	labIDs := make([]string, len(ids))

	for i, raw := range ids {
		id, ok := parseID(raw)
		if !ok {
			return nil, labs.ErrNotFound
		}

		labIDs[i] = id
	}

	urls, err := service.PageURLs(ctx, owner, labIDs)
	if err == nil && urls == nil {
		urls = []string{}
	}

	return urls, err
}

func decodeLabBody(w http.ResponseWriter, r *http.Request, into any, usage string) bool {
	return decodeBodyLimit(w, r, into, usage, maxLabBodyBytes)
}
