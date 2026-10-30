package searchapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"web-crawler/internal/auth"

	"go.uber.org/zap"
)

const (
	testToken   = "arachne_test-token"
	testSession = "session-1"
	testPass    = "correct horse battery"
)

var testUser = auth.User{ID: 1, Username: "lein"}

type fakeAuth struct {
	renewed   time.Time
	err       error
	loggedOut string
}

func (f *fakeAuth) Login(_ context.Context, username, password, _, _ string) (auth.Session, error) {
	switch {
	case username == "locked":
		return auth.Session{}, &auth.ThrottledError{RetryAfter: 90 * time.Second}
	case username == testUser.Username && password == testPass:
		return auth.Session{Token: testSession, User: testUser, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}

	return auth.Session{}, auth.ErrInvalidCredentials
}

func (f *fakeAuth) Logout(_ context.Context, token string) error {
	f.loggedOut = token
	return nil
}

func (f *fakeAuth) Authenticate(_ context.Context, token string) (auth.User, time.Time, error) {
	if f.err != nil {
		return auth.User{}, time.Time{}, f.err
	}

	if token != testSession {
		return auth.User{}, time.Time{}, auth.ErrUnauthenticated
	}

	return testUser, f.renewed, nil
}

func (f *fakeAuth) AuthenticateToken(_ context.Context, token string) (auth.User, error) {
	if token != testToken {
		return auth.User{}, auth.ErrUnauthenticated
	}

	return testUser, nil
}

func (f *fakeAuth) ChangePassword(_ context.Context, _ auth.User, current, next, _, _ string) (auth.Session, error) {
	if current != testPass {
		return auth.Session{}, auth.ErrInvalidCredentials
	}

	if len(next) < 12 {
		return auth.Session{}, auth.ErrInvalidPassword
	}

	return auth.Session{Token: "session-2", User: testUser, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func newTestHandler(services Services, cfg Config) http.Handler {
	if services.Auth == nil {
		services.Auth = &fakeAuth{}
	}

	if services.Searcher == nil {
		services.Searcher = &fakeSearcher{}
	}

	return NewHandler(zap.NewNop().Sugar(), services, cfg)
}

func signedIn(r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer "+testToken)
	return r
}

func withSession(r *http.Request) *http.Request {
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: testSession})
	return r
}

func do(handler http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)

	return rec
}

func testConfig() Config {
	return Config{DefaultLimit: 10, MaxLimit: 50, RequestTimeout: time.Second, AnswerTimeout: time.Second}
}

func sessionCookieOf(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}

	return nil
}

func TestEverythingButSigningInNeedsAUser(t *testing.T) {
	handler := newTestHandler(Services{}, testConfig())

	for _, target := range []string{
		"GET /search?q=x", "GET /answer?q=x", "GET /answer/stream?q=x",
		"GET /api/runs", "POST /api/runs", "GET /api/runs/r1", "GET /api/me", "POST /api/me/password",
		"GET /api/labs", "POST /api/labs", "POST /api/labs/plan", "GET /api/labs/1", "DELETE /api/labs/1",
		"GET /api/labs/1/pages", "POST /api/labs/1/sources", "POST /api/labs/1/sources/2/refresh", "DELETE /api/labs/1/sources/2",
	} {
		method, path, _ := strings.Cut(target, " ")

		rec := do(handler, httptest.NewRequest(method, path, strings.NewReader("{}")))
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s without a user: %d", target, rec.Code)
		}

		bad := httptest.NewRequest(method, path, strings.NewReader("{}"))
		bad.Header.Set("Authorization", "Bearer arachne_guessed")

		if rec := do(handler, bad); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with a wrong token: %d", target, rec.Code)
		}
	}

	rec := do(handler, httptest.NewRequest(http.MethodGet, "/?q=goroutines", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login?next=%2F%3Fq%3Dgoroutines" {
		t.Errorf("the page without a user: %d → %q", rec.Code, rec.Header().Get("Location"))
	}

	if rec := do(handler, httptest.NewRequest(http.MethodGet, "/labs/3", nil)); rec.Code != http.StatusSeeOther {
		t.Errorf("a lab page without a user: %d", rec.Code)
	}

	for _, open := range []string{"/login", "/healthz", "/favicon.svg", "/static/site.css", "/static/theme.js", "/static/common.js"} {
		if rec := do(handler, httptest.NewRequest(http.MethodGet, open, nil)); rec.Code != http.StatusOK {
			t.Errorf("%s: %d, want it open", open, rec.Code)
		}
	}

	if rec := do(handler, httptest.NewRequest(http.MethodGet, "/static/", nil)); rec.Code != http.StatusNotFound {
		t.Errorf("static directory listing: %d", rec.Code)
	}
}

func TestLoginSetsASessionCookie(t *testing.T) {
	handler := newTestHandler(Services{}, testConfig())

	rec := do(handler, httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"username":"lein","password":"`+testPass+`"}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"username":"lein"`) {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}

	c := sessionCookieOf(rec)
	if c == nil || c.Value != testSession || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Secure || c.Path != "/" {
		t.Errorf("cookie = %+v", c)
	}

	behindTLS := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"lein","password":"`+testPass+`"}`))
	behindTLS.Header.Set("X-Forwarded-Proto", "https")

	if c := sessionCookieOf(do(handler, behindTLS)); c == nil || !c.Secure {
		t.Errorf("behind HTTPS the cookie must be Secure: %+v", c)
	}

	rec = do(handler, withSession(httptest.NewRequest(http.MethodGet, "/api/me", nil)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"username":"lein"`) {
		t.Errorf("me: %d %s", rec.Code, rec.Body)
	}

	if rec := do(handler, withSession(httptest.NewRequest(http.MethodGet, "/", nil))); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("the page with a session: %d, CSP %q", rec.Code, rec.Header().Get("Content-Security-Policy"))
	}
}

func TestLoginFailures(t *testing.T) {
	handler := newTestHandler(Services{}, testConfig())

	cases := []struct {
		body string
		want int
	}{
		{`{"username":"lein","password":"wrong"}`, http.StatusUnauthorized},
		{`{"username":"locked","password":"x"}`, http.StatusTooManyRequests},
		{`username=lein&password=x`, http.StatusBadRequest},
		{`{"username":"lein","password":"x","admin":true}`, http.StatusBadRequest},
	}

	for _, tc := range cases {
		rec := do(handler, httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(tc.body)))
		if rec.Code != tc.want {
			t.Errorf("%s: %d, want %d", tc.body, rec.Code, tc.want)
		}

		if sessionCookieOf(rec) != nil {
			t.Errorf("%s: a failed login set a cookie", tc.body)
		}

		if tc.want == http.StatusTooManyRequests && rec.Header().Get("Retry-After") != "90" {
			t.Errorf("Retry-After = %q", rec.Header().Get("Retry-After"))
		}
	}
}

func TestCrossSiteWritesAreRefused(t *testing.T) {
	handler := newTestHandler(Services{}, testConfig())

	forged := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"lein","password":"`+testPass+`"}`))
	forged.Header.Set("Sec-Fetch-Site", "cross-site")

	if rec := do(handler, forged); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site login: %d", rec.Code)
	}

	old := withSession(httptest.NewRequest(http.MethodPost, "http://arachne.local/api/runs", strings.NewReader(`{"url":"evil.example"}`)))
	old.Header.Set("Origin", "https://evil.example")

	if rec := do(handler, old); rec.Code != http.StatusForbidden {
		t.Errorf("a browser without fetch metadata, other origin: %d", rec.Code)
	}

	same := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"lein","password":"`+testPass+`"}`))
	same.Header.Set("Sec-Fetch-Site", "same-origin")

	if rec := do(handler, same); rec.Code != http.StatusOK {
		t.Errorf("same-origin login: %d", rec.Code)
	}
}

func TestRenewedSessionRefreshesTheCookie(t *testing.T) {
	handler := newTestHandler(Services{Auth: &fakeAuth{renewed: time.Now().Add(30 * 24 * time.Hour)}}, testConfig())

	c := sessionCookieOf(do(handler, withSession(httptest.NewRequest(http.MethodGet, "/api/me", nil))))
	if c == nil || c.Value != testSession || c.MaxAge < 29*24*3600 {
		t.Errorf("cookie = %+v", c)
	}

	if c := sessionCookieOf(do(newTestHandler(Services{}, testConfig()), withSession(httptest.NewRequest(http.MethodGet, "/api/me", nil)))); c != nil {
		t.Errorf("a session not renewed got a cookie: %+v", c)
	}
}

func TestSignInOutageIsNotASignOut(t *testing.T) {
	handler := newTestHandler(Services{Auth: &fakeAuth{err: errors.New("postgres is down")}}, testConfig())

	if rec := do(handler, withSession(httptest.NewRequest(http.MethodGet, "/search?q=x", nil))); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503: a broken store must not send users to the login page", rec.Code)
	}
}

func TestLoginPageSendsSignedInUsersOn(t *testing.T) {
	handler := newTestHandler(Services{}, testConfig())

	cases := map[string]string{
		"/login?next=%2F%3Fq%3Dx":           "/?q=x",
		"/login?next=//evil.example/":       "/",
		`/login?next=/\evil.example/`:       "/",
		"/login?next=https://evil.example/": "/",
		"/login":                            "/",
	}

	for target, want := range cases {
		rec := do(handler, withSession(httptest.NewRequest(http.MethodGet, target, nil)))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
			t.Errorf("%s: %d → %q, want %q", target, rec.Code, rec.Header().Get("Location"), want)
		}
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	fake := &fakeAuth{}
	handler := newTestHandler(Services{Auth: fake}, testConfig())

	rec := do(handler, withSession(httptest.NewRequest(http.MethodPost, "/api/logout", nil)))
	if rec.Code != http.StatusNoContent || fake.loggedOut != testSession {
		t.Errorf("logout: %d, ended %q", rec.Code, fake.loggedOut)
	}

	if c := sessionCookieOf(rec); c == nil || c.MaxAge >= 0 {
		t.Errorf("the cookie was not cleared: %+v", c)
	}
}

func TestChangePassword(t *testing.T) {
	handler := newTestHandler(Services{}, testConfig())

	rec := do(handler, withSession(httptest.NewRequest(http.MethodPost, "/api/me/password",
		strings.NewReader(`{"current":"`+testPass+`","new":"a brand new password"}`))))
	if c := sessionCookieOf(rec); rec.Code != http.StatusNoContent || c == nil || c.Value != "session-2" {
		t.Errorf("change: %d, cookie %+v", rec.Code, c)
	}

	for body, want := range map[string]int{
		`{"current":"wrong","new":"a brand new password"}`: http.StatusUnauthorized,
		`{"current":"` + testPass + `","new":"short"}`:     http.StatusBadRequest,
	} {
		if rec := do(handler, withSession(httptest.NewRequest(http.MethodPost, "/api/me/password", strings.NewReader(body)))); rec.Code != want {
			t.Errorf("%s: %d, want %d", body, rec.Code, want)
		}
	}
}
