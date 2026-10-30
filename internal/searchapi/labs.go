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
	List(ctx context.Context, ownerID int64) ([]labs.Lab, error)
	Create(ctx context.Context, ownerID int64, name, topic string, sources []labs.SourceRequest) (labs.Detail, error)
	Get(ctx context.Context, ownerID, labID int64) (labs.Detail, error)
	Delete(ctx context.Context, ownerID, labID int64) error
	AddSources(ctx context.Context, ownerID, labID int64, sources []labs.SourceRequest) (labs.Detail, error)
	Refresh(ctx context.Context, ownerID, labID, sourceID int64) (labs.SourceStatus, error)
	RemoveSource(ctx context.Context, ownerID, labID, sourceID int64) error
	Pages(ctx context.Context, ownerID, labID int64, limit int) ([]labs.Page, error)
	PageURLs(ctx context.Context, ownerID int64, labIDs []int64) ([]string, error)
	Plan(ctx context.Context, ownerID, labID int64, request string) (labs.Plan, error)
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
}

type addSourcesRequest struct {
	Sources []labs.SourceRequest `json:"sources"`
}

type planRequest struct {
	Request string `json:"request"`
	LabID   int64  `json:"lab_id"`
}

func (h *labsHandler) list(w http.ResponseWriter, r *http.Request) {
	h.run(w, r, labsTimeout, func(ctx context.Context, owner int64) (int, any, error) {
		list, err := h.service.List(ctx, owner)
		return http.StatusOK, map[string]any{"labs": list}, err
	})
}

func (h *labsHandler) create(w http.ResponseWriter, r *http.Request) {
	var req createLabRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {name, topic, sources: [{url, scope?, depth?, links?}]}") {
		return
	}

	h.run(w, r, labsTimeout, func(ctx context.Context, owner int64) (int, any, error) {
		detail, err := h.service.Create(ctx, owner, req.Name, req.Topic, req.Sources)
		return http.StatusCreated, detail, err
	})
}

func (h *labsHandler) plan(w http.ResponseWriter, r *http.Request) {
	var req planRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {request, lab_id?}") {
		return
	}

	h.run(w, r, h.planTimeout, func(ctx context.Context, owner int64) (int, any, error) {
		plan, err := h.service.Plan(ctx, owner, req.LabID, req.Request)
		return http.StatusOK, plan, err
	})
}

func (h *labsHandler) get(w http.ResponseWriter, r *http.Request) {
	h.withLab(w, r, func(ctx context.Context, owner, lab int64) (int, any, error) {
		detail, err := h.service.Get(ctx, owner, lab)
		return http.StatusOK, detail, err
	})
}

func (h *labsHandler) delete(w http.ResponseWriter, r *http.Request) {
	h.withLab(w, r, func(ctx context.Context, owner, lab int64) (int, any, error) {
		return http.StatusNoContent, nil, h.service.Delete(ctx, owner, lab)
	})
}

func (h *labsHandler) addSources(w http.ResponseWriter, r *http.Request) {
	var req addSourcesRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {sources: [{url, scope?, depth?, links?}]}") {
		return
	}

	h.withLab(w, r, func(ctx context.Context, owner, lab int64) (int, any, error) {
		detail, err := h.service.AddSources(ctx, owner, lab, req.Sources)
		return http.StatusOK, detail, err
	})
}

func (h *labsHandler) refresh(w http.ResponseWriter, r *http.Request) {
	h.withSource(w, r, func(ctx context.Context, owner, lab, source int64) (int, any, error) {
		status, err := h.service.Refresh(ctx, owner, lab, source)
		return http.StatusAccepted, status, err
	})
}

func (h *labsHandler) removeSource(w http.ResponseWriter, r *http.Request) {
	h.withSource(w, r, func(ctx context.Context, owner, lab, source int64) (int, any, error) {
		return http.StatusNoContent, nil, h.service.RemoveSource(ctx, owner, lab, source)
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

	h.withLab(w, r, func(ctx context.Context, owner, lab int64) (int, any, error) {
		list, err := h.service.Pages(ctx, owner, lab, limit)
		return http.StatusOK, map[string]any{"pages": list}, err
	})
}

func (h *labsHandler) withLab(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, owner, lab int64) (int, any, error)) {
	lab, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: labs.ErrNotFound.Error()})
		return
	}

	h.run(w, r, labsTimeout, func(ctx context.Context, owner int64) (int, any, error) { return do(ctx, owner, lab) })
}

func (h *labsHandler) withSource(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, owner, lab, source int64) (int, any, error)) {
	source, err := strconv.ParseInt(r.PathValue("source"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: labs.ErrNotFound.Error()})
		return
	}

	h.withLab(w, r, func(ctx context.Context, owner, lab int64) (int, any, error) { return do(ctx, owner, lab, source) })
}

// run calls do as the signed-in user and writes what it returns.
func (h *labsHandler) run(w http.ResponseWriter, r *http.Request, timeout time.Duration, do func(ctx context.Context, owner int64) (int, any, error)) {
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
	switch {
	case errors.Is(err, labs.ErrNotFound):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, labs.ErrNameTaken), errors.Is(err, labs.ErrSourceExists):
		return http.StatusConflict, err.Error()
	case errors.Is(err, labs.ErrInvalid):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, labs.ErrCrawlingDisabled):
		return http.StatusServiceUnavailable, err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "the request timed out"
	}

	logger.Errorw("Lab request failed", "error", err)

	return http.StatusInternalServerError, "lab request failed"
}

// labURLs reads ?labs=1,2 and returns those labs' pages, or nil when the
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

	ids := make([]int64, len(parts))

	for i, part := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: labs must be lab IDs separated by commas", labs.ErrInvalid)
		}

		ids[i] = id
	}

	user, _ := auth.UserFrom(r.Context())

	urls, err := service.PageURLs(ctx, user.ID, ids)
	if err == nil && urls == nil {
		urls = []string{}
	}

	return urls, err
}

func decodeLabBody(w http.ResponseWriter, r *http.Request, into any, usage string) bool {
	return decodeBodyLimit(w, r, into, usage, maxLabBodyBytes)
}
