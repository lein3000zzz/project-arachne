package searchapi

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"web-crawler/internal/auth"

	"go.uber.org/zap"
)

// Authenticator is required: everything but signing in, health and static
// files needs a user.
type Authenticator interface {
	Login(ctx context.Context, username, password, addr, userAgent string) (auth.Session, error)
	Logout(ctx context.Context, token string) error
	Authenticate(ctx context.Context, token string) (auth.User, time.Time, error)
	AuthenticateToken(ctx context.Context, token string) (auth.User, error)
	ChangePassword(ctx context.Context, user auth.User, current, next, addr, userAgent string) (auth.Session, error)
}

const (
	sessionCookie = "arachne_session"
	// Includes waiting for a free hashing slot.
	authTimeout      = 10 * time.Second
	maxAuthBodyBytes = 4 << 10
)

type authHandler struct {
	logger *zap.SugaredLogger
	auth   Authenticator
}

func (h *authHandler) api(next http.HandlerFunc) http.Handler  { return h.require(false, next) }
func (h *authHandler) page(next http.HandlerFunc) http.Handler { return h.require(true, next) }

func (h *authHandler) require(page bool, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := h.identify(w, r)

		switch {
		case err == nil:
			next(w, r.WithContext(auth.WithUser(r.Context(), user)))
		case !errors.Is(err, auth.ErrUnauthenticated):
			h.logger.Errorw("Authentication failed", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "signing in is unavailable"})
		case page:
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="arachne"`)
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "sign in first"})
		}
	})
}

// identify takes an API token from the Authorization header, otherwise the
// session cookie, which it refreshes when the session was renewed.
func (h *authHandler) identify(w http.ResponseWriter, r *http.Request) (auth.User, error) {
	if header := r.Header.Get("Authorization"); header != "" {
		token, ok := strings.CutPrefix(header, "Bearer ")
		if !ok {
			return auth.User{}, auth.ErrUnauthenticated
		}

		return h.auth.AuthenticateToken(r.Context(), token)
	}

	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return auth.User{}, auth.ErrUnauthenticated
	}

	user, renewed, err := h.auth.Authenticate(r.Context(), cookie.Value)
	if err == nil && !renewed.IsZero() {
		setSessionCookie(w, r, cookie.Value, renewed)
	}

	return user, err
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *authHandler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeBody(w, r, &req, "body must be JSON: {username, password}") {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), authTimeout)
	defer cancel()

	session, err := h.auth.Login(ctx, req.Username, req.Password, clientAddr(r), r.UserAgent())
	if err != nil {
		h.fail(w, err)
		return
	}

	setSessionCookie(w, r, session.Token, session.ExpiresAt)
	writeJSON(w, http.StatusOK, session.User)
}

func (h *authHandler) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), authTimeout)
		defer cancel()

		if err := h.auth.Logout(ctx, cookie.Value); err != nil {
			h.logger.Warnw("Failed to end a session", "error", err)
		}
	}

	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure(r)})
	w.WriteHeader(http.StatusNoContent)
}

func (h *authHandler) me(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	writeJSON(w, http.StatusOK, user)
}

type passwordRequest struct {
	Current string `json:"current"`
	New     string `json:"new"`
}

func (h *authHandler) changePassword(w http.ResponseWriter, r *http.Request) {
	var req passwordRequest
	if !decodeBody(w, r, &req, "body must be JSON: {current, new}") {
		return
	}

	user, _ := auth.UserFrom(r.Context())

	ctx, cancel := context.WithTimeout(r.Context(), authTimeout)
	defer cancel()

	session, err := h.auth.ChangePassword(ctx, user, req.Current, req.New, clientAddr(r), r.UserAgent())
	if err != nil {
		h.fail(w, err)
		return
	}

	setSessionCookie(w, r, session.Token, session.ExpiresAt)
	w.WriteHeader(http.StatusNoContent)
}

// loginPage sends a user who is signed in already where they were going.
func (h *authHandler) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, err := h.identify(w, r); err == nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}

	servePage(w, loginHTML)
}

func (h *authHandler) fail(w http.ResponseWriter, err error) {
	var throttled *auth.ThrottledError

	switch {
	case errors.As(err, &throttled):
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(throttled.RetryAfter.Seconds()))))
		writeJSON(w, http.StatusTooManyRequests, errorResponse{Error: err.Error()})
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: err.Error()})
	case errors.Is(err, auth.ErrInvalidPassword):
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
	case errors.Is(err, context.DeadlineExceeded):
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "signing in is busy; try again"})
	default:
		h.logger.Errorw("Sign-in request failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "signing in failed"})
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, into any, usage string) bool {
	return decodeBodyLimit(w, r, into, usage, maxAuthBodyBytes)
}

func decodeBodyLimit(w http.ResponseWriter, r *http.Request, into any, usage string, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()

	if err := dec.Decode(into); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: usage})
		return false
	}

	return true
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   max(int(time.Until(expires).Seconds()), 1),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure(r),
	})
}

// Behind a TLS-terminating proxy the request itself arrives over plain HTTP.
func secure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// clientAddr is the peer's address. X-Forwarded-For is not trusted: anyone
// could set it to escape the per-address login limit.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}

// safeNext keeps the post-login redirect on this site: "//evil.example" and
// "/\evil.example" are other hosts to a browser.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, `/\`) {
		return "/"
	}

	return next
}
