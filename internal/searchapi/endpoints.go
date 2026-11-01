package searchapi

import (
	"context"
	"errors"
	"net/http"
	"time"
	"web-crawler/internal/auth"
	"web-crawler/internal/endpoints"

	"go.uber.org/zap"
)

type EndpointService interface {
	Get(ctx context.Context, userID int64) (endpoints.Endpoint, error)
	Set(ctx context.Context, userID int64, in endpoints.Settings) (endpoints.Endpoint, error)
	Delete(ctx context.Context, userID int64) error
	Test(ctx context.Context, userID int64, in endpoints.Settings) (endpoints.TestResult, error)
}

// ServerModel is the chat model the server runs, which answers users who set
// none of their own. A zero value: the server runs none.
type ServerModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type endpointsHandler struct {
	logger  *zap.SugaredLogger
	service EndpointService
	server  ServerModel
}

const endpointsTimeout = 15 * time.Second

func (h *endpointsHandler) get(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())

	body := map[string]any{"available": h.service != nil, "own": nil, "server": nil}
	if h.server.Model != "" {
		body["server"] = h.server
	}

	if h.service != nil {
		ctx, cancel := context.WithTimeout(r.Context(), endpointsTimeout)
		defer cancel()

		e, err := h.service.Get(ctx, user.ID)

		switch {
		case err == nil:
			body["own"] = e
		case !errors.Is(err, endpoints.ErrNotSet):
			h.fail(w, err)
			return
		}
	}

	writeJSON(w, http.StatusOK, body)
}

func (h *endpointsHandler) set(w http.ResponseWriter, r *http.Request) {
	h.withSettings(w, r, func(ctx context.Context, user int64, in endpoints.Settings) (int, any, error) {
		e, err := h.service.Set(ctx, user, in)
		return http.StatusOK, e, err
	})
}

func (h *endpointsHandler) test(w http.ResponseWriter, r *http.Request) {
	h.withSettings(w, r, func(ctx context.Context, user int64, in endpoints.Settings) (int, any, error) {
		res, err := h.service.Test(ctx, user, in)
		return http.StatusOK, res, err
	})
}

func (h *endpointsHandler) delete(w http.ResponseWriter, r *http.Request) {
	if h.service == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: endpoints.ErrUnavailable.Error()})
		return
	}

	user, _ := auth.UserFrom(r.Context())

	ctx, cancel := context.WithTimeout(r.Context(), endpointsTimeout)
	defer cancel()

	if err := h.service.Delete(ctx, user.ID); err != nil {
		h.fail(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *endpointsHandler) withSettings(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, user int64, in endpoints.Settings) (int, any, error)) {
	if h.service == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: endpoints.ErrUnavailable.Error()})
		return
	}

	var in endpoints.Settings
	if !decodeLabBody(w, r, &in, "body must be JSON: {provider, base_url, model, api_key?}") {
		return
	}

	user, _ := auth.UserFrom(r.Context())

	// A test waits on the endpoint itself; its own timeout bounds that.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	status, body, err := do(ctx, user.ID, in)
	if err != nil {
		h.fail(w, err)
		return
	}

	writeJSON(w, status, body)
}

func (h *endpointsHandler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, endpoints.ErrInvalid):
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
	case errors.Is(err, endpoints.ErrTest):
		// The endpoint's own words: it is the user's, and outside the server.
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: err.Error()})
	case errors.Is(err, context.DeadlineExceeded):
		writeJSON(w, http.StatusGatewayTimeout, errorResponse{Error: "the endpoint took too long"})
	default:
		h.logger.Errorw("Endpoint request failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "the endpoint could not be saved or read"})
	}
}
