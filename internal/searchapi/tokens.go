package searchapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"web-crawler/internal/auth"

	"go.uber.org/zap"
)

// TokenService is optional: without it, tokens come from make token alone.
type TokenService interface {
	AddToken(ctx context.Context, userID, name string) (string, error)
	Tokens(ctx context.Context, userID string) ([]auth.Token, error)
	RemoveToken(ctx context.Context, userID, name string) error
}

type tokensHandler struct {
	logger  *zap.SugaredLogger
	service TokenService
	// The MCP server's path, empty when it is off: the page shows how to
	// connect an agent only when one can.
	mcp string
}

type tokenRequest struct {
	Name string `json:"name"`
}

// browser is api for what only a signed-in browser may do: whoever holds a
// token cannot list, make or revoke tokens with it.
func (h *authHandler) browser(next http.HandlerFunc) http.Handler {
	api := h.api(next)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			writeJSON(w, http.StatusForbidden, errorResponse{Error: "tokens are managed from a signed-in browser"})
			return
		}

		api.ServeHTTP(w, r)
	})
}

// agent signs in by API token alone: with the session cookie, any site the
// user visits could have the browser call the tools.
func (h *authHandler) agent(next http.Handler) http.Handler {
	signedIn := h.api(next.ServeHTTP)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="arachne"`)
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "sign in with an API token"})

			return
		}

		signedIn.ServeHTTP(w, r)
	})
}

func (h *tokensHandler) list(w http.ResponseWriter, r *http.Request) {
	h.run(w, r, func(ctx context.Context, user string) (int, any, error) {
		tokens, err := h.service.Tokens(ctx, user)
		if tokens == nil {
			tokens = []auth.Token{}
		}

		return http.StatusOK, map[string]any{"tokens": tokens, "mcp": h.mcp, "max": auth.MaxTokens}, err
	})
}

func (h *tokensHandler) add(w http.ResponseWriter, r *http.Request) {
	var req tokenRequest
	if !decodeBody(w, r, &req, "body must be JSON: {name}") {
		return
	}

	h.run(w, r, func(ctx context.Context, user string) (int, any, error) {
		token, err := h.service.AddToken(ctx, user, req.Name)
		return http.StatusCreated, map[string]string{"name": req.Name, "token": token}, err
	})
}

func (h *tokensHandler) remove(w http.ResponseWriter, r *http.Request) {
	h.run(w, r, func(ctx context.Context, user string) (int, any, error) {
		return http.StatusNoContent, nil, h.service.RemoveToken(ctx, user, r.PathValue("name"))
	})
}

func (h *tokensHandler) run(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, user string) (int, any, error)) {
	if h.service == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "tokens are not available here"})
		return
	}

	user, _ := auth.UserFrom(r.Context())

	ctx, cancel := context.WithTimeout(r.Context(), authTimeout)
	defer cancel()

	status, body, err := do(ctx, user.ID)

	switch {
	case err == nil && status == http.StatusNoContent:
		w.WriteHeader(status)
	case err == nil:
		writeJSON(w, status, body)
	case errors.Is(err, auth.ErrInvalidTokenName):
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
	case errors.Is(err, auth.ErrTokenExists), errors.Is(err, auth.ErrTooManyTokens):
		writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
	case errors.Is(err, auth.ErrTokenNotFound):
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
	default:
		h.logger.Errorw("Token request failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "the token request failed"})
	}
}
