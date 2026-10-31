package searchapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"
	"web-crawler/internal/artifacts"
	"web-crawler/internal/auth"
	"web-crawler/internal/labs"

	"go.uber.org/zap"
)

type ArtifactService interface {
	Draft(ctx context.Context, ownerID, labID int64, request string) (artifacts.Outline, error)
	Build(ctx context.Context, ownerID, labID int64, request string, outline artifacts.Outline) (artifacts.Artifact, error)
	Rebuild(ctx context.Context, ownerID, artifactID int64) (artifacts.Artifact, error)
	Rewrite(ctx context.Context, ownerID, artifactID int64, section int) (artifacts.Artifact, error)
	DraftSection(ctx context.Context, ownerID, artifactID int64, request string) (artifacts.Planned, error)
	Extend(ctx context.Context, ownerID, artifactID int64, planned artifacts.Planned) (artifacts.Artifact, error)
	List(ctx context.Context, ownerID, labID int64) ([]artifacts.Artifact, error)
	Get(ctx context.Context, ownerID, artifactID int64, version int) (artifacts.Artifact, artifacts.Version, []int, error)
	Delete(ctx context.Context, ownerID, artifactID int64) error
	SaveAnswer(ctx context.Context, ownerID, labID int64, saved artifacts.SavedAnswer) (artifacts.Artifact, error)
}

type artifactsHandler struct {
	logger  *zap.SugaredLogger
	service ArtifactService
	recent  *recentAnswers
	// Drafting an outline waits on the chat model.
	draftTimeout time.Duration
}

const artifactsTimeout = 15 * time.Second

type outlineRequest struct {
	Request string `json:"request"`
}

type buildRequest struct {
	Request string            `json:"request"`
	Outline artifacts.Outline `json:"outline"`
}

type saveAnswerRequest struct {
	Answer string `json:"answer"`
}

func (h *artifactsHandler) draft(w http.ResponseWriter, r *http.Request) {
	var req outlineRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {request}") {
		return
	}

	h.inLab(w, r, h.draftTimeout, func(ctx context.Context, owner, lab int64) (int, any, error) {
		outline, err := h.service.Draft(ctx, owner, lab, req.Request)
		return http.StatusOK, outline, err
	})
}

func (h *artifactsHandler) build(w http.ResponseWriter, r *http.Request) {
	var req buildRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {request, outline: {title, sections: [{heading, focus}]}}") {
		return
	}

	h.inLab(w, r, artifactsTimeout, func(ctx context.Context, owner, lab int64) (int, any, error) {
		a, err := h.service.Build(ctx, owner, lab, req.Request, req.Outline)
		return http.StatusAccepted, a, err
	})
}

func (h *artifactsHandler) list(w http.ResponseWriter, r *http.Request) {
	h.inLab(w, r, artifactsTimeout, func(ctx context.Context, owner, lab int64) (int, any, error) {
		list, err := h.service.List(ctx, owner, lab)
		return http.StatusOK, map[string]any{"artifacts": list}, err
	})
}

// saveAnswer keeps an answer the server gave lately, by its id: the text is
// the server's own, never what the client holds.
func (h *artifactsHandler) saveAnswer(w http.ResponseWriter, r *http.Request) {
	var req saveAnswerRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {answer: <the answer's id>}") {
		return
	}

	h.inLab(w, r, artifactsTimeout, func(ctx context.Context, owner, lab int64) (int, any, error) {
		kept, ok := h.recent.get(owner, req.Answer)
		if !ok {
			return 0, nil, statusError{http.StatusNotFound, errors.New("that answer is no longer kept: ask again")}
		}

		if !kept.checked {
			return 0, nil, statusError{http.StatusConflict, errors.New("the answer is still being checked")}
		}

		a, err := h.service.SaveAnswer(ctx, owner, lab, artifacts.SavedAnswer{
			Question: kept.query, Text: kept.res.Answer, Sources: kept.res.Sources, Claims: kept.claims, Model: kept.res.Model,
		})

		return http.StatusCreated, a, err
	})
}

// get returns a version ready to show: rendered, with its table of contents.
func (h *artifactsHandler) get(w http.ResponseWriter, r *http.Request) {
	version, ok := versionParam(w, r)
	if !ok {
		return
	}

	h.artifact(w, r, func(ctx context.Context, owner, id int64) (int, any, error) {
		a, v, versions, err := h.service.Get(ctx, owner, id, version)
		if err != nil {
			return 0, nil, err
		}

		body, toc := artifacts.Render(v)
		v.Markdown = ""

		return http.StatusOK, map[string]any{"artifact": a, "version": v, "versions": versions, "html": body, "toc": toc}, nil
	})
}

func (h *artifactsHandler) markdown(w http.ResponseWriter, r *http.Request) {
	version, ok := versionParam(w, r)
	if !ok {
		return
	}

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || h.service == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: artifacts.ErrNotFound.Error()})
		return
	}

	user, _ := auth.UserFrom(r.Context())

	ctx, cancel := context.WithTimeout(r.Context(), artifactsTimeout)
	defer cancel()

	a, v, _, err := h.service.Get(ctx, user.ID, id, version)
	if err != nil {
		status, message := h.failure(err)
		writeJSON(w, status, errorResponse{Error: message})

		return
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="artifact-`+strconv.FormatInt(a.ID, 10)+`-v`+strconv.Itoa(v.Number)+`.md"`)
	_, _ = w.Write([]byte(v.Markdown))
}

func (h *artifactsHandler) rebuild(w http.ResponseWriter, r *http.Request) {
	h.artifact(w, r, func(ctx context.Context, owner, id int64) (int, any, error) {
		a, err := h.service.Rebuild(ctx, owner, id)
		return http.StatusAccepted, a, err
	})
}

func (h *artifactsHandler) rewrite(w http.ResponseWriter, r *http.Request) {
	section, err := strconv.Atoi(r.PathValue("section"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "no such section"})
		return
	}

	h.artifact(w, r, func(ctx context.Context, owner, id int64) (int, any, error) {
		a, err := h.service.Rewrite(ctx, owner, id, section)
		return http.StatusAccepted, a, err
	})
}

func (h *artifactsHandler) draftSection(w http.ResponseWriter, r *http.Request) {
	var req outlineRequest
	if !decodeLabBody(w, r, &req, "body must be JSON: {request}") {
		return
	}

	h.artifactFor(w, r, h.draftTimeout, func(ctx context.Context, owner, id int64) (int, any, error) {
		p, err := h.service.DraftSection(ctx, owner, id, req.Request)
		return http.StatusOK, p, err
	})
}

func (h *artifactsHandler) extend(w http.ResponseWriter, r *http.Request) {
	var planned artifacts.Planned
	if !decodeLabBody(w, r, &planned, "body must be JSON: {heading, focus}") {
		return
	}

	h.artifact(w, r, func(ctx context.Context, owner, id int64) (int, any, error) {
		a, err := h.service.Extend(ctx, owner, id, planned)
		return http.StatusAccepted, a, err
	})
}

func (h *artifactsHandler) delete(w http.ResponseWriter, r *http.Request) {
	h.artifact(w, r, func(ctx context.Context, owner, id int64) (int, any, error) {
		return http.StatusNoContent, nil, h.service.Delete(ctx, owner, id)
	})
}

func versionParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("version")
	if raw == "" {
		return 0, true
	}

	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "version must be a positive integer"})
		return 0, false
	}

	return n, true
}

func (h *artifactsHandler) inLab(w http.ResponseWriter, r *http.Request, timeout time.Duration, do func(ctx context.Context, owner, lab int64) (int, any, error)) {
	lab, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: labs.ErrNotFound.Error()})
		return
	}

	h.run(w, r, timeout, func(ctx context.Context, owner int64) (int, any, error) { return do(ctx, owner, lab) })
}

func (h *artifactsHandler) artifact(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, owner, id int64) (int, any, error)) {
	h.artifactFor(w, r, artifactsTimeout, do)
}

func (h *artifactsHandler) artifactFor(w http.ResponseWriter, r *http.Request, timeout time.Duration, do func(ctx context.Context, owner, id int64) (int, any, error)) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: artifacts.ErrNotFound.Error()})
		return
	}

	h.run(w, r, timeout, func(ctx context.Context, owner int64) (int, any, error) { return do(ctx, owner, id) })
}

func (h *artifactsHandler) run(w http.ResponseWriter, r *http.Request, timeout time.Duration, do func(ctx context.Context, owner int64) (int, any, error)) {
	if h.service == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "artifacts are not available"})
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

func (h *artifactsHandler) failure(err error) (int, string) {
	switch {
	case errors.Is(err, artifacts.ErrNotFound):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, artifacts.ErrInvalid):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, artifacts.ErrBuilding):
		return http.StatusConflict, err.Error()
	case errors.Is(err, artifacts.ErrUnavailable):
		return http.StatusServiceUnavailable, err.Error()
	}

	return labFailure(h.logger, err)
}
