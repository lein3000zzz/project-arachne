package searchapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"web-crawler/internal/endpoints"
	"web-crawler/internal/llm"
)

type fakeEndpoints struct {
	own    *endpoints.Endpoint
	got    endpoints.Settings
	user   string
	err    error
	tested bool
}

func (f *fakeEndpoints) Get(_ context.Context, user string) (endpoints.Endpoint, error) {
	f.user = user
	if f.own == nil {
		return endpoints.Endpoint{}, endpoints.ErrNotSet
	}

	return *f.own, nil
}

func (f *fakeEndpoints) Set(_ context.Context, user string, in endpoints.Settings) (endpoints.Endpoint, error) {
	f.user, f.got = user, in
	return endpoints.Endpoint{Provider: in.Provider, BaseURL: in.BaseURL, Model: in.Model, KeySet: in.APIKey != nil}, f.err
}

func (f *fakeEndpoints) Delete(_ context.Context, user string) error {
	f.user, f.own = user, nil
	return f.err
}

func (f *fakeEndpoints) Test(_ context.Context, user string, in endpoints.Settings) (endpoints.TestResult, error) {
	f.user, f.got, f.tested = user, in, true
	return endpoints.TestResult{Model: in.Model, Reply: "OK", Seconds: 0.4}, f.err
}

func serveSettingsAPI(services Services, server ServerModel, method, target, body string) *httptest.ResponseRecorder {
	handler := newTestHandler(services, Config{DefaultLimit: 10, MaxLimit: 50, RequestTimeout: time.Second, AnswerTimeout: time.Second, ServerModel: server})
	return do(handler, signedIn(httptest.NewRequest(method, target, strings.NewReader(body))))
}

func TestAUserSetsAndTestsTheirOwnModel(t *testing.T) {
	own := &fakeEndpoints{}
	server := ServerModel{Provider: "openai-compatible", Model: "qwen3:4b-instruct"}

	rec := serveSettingsAPI(Services{Endpoints: own}, server, http.MethodGet, "/api/me/model", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"available":true`) || !strings.Contains(rec.Body.String(), `"own":null`) ||
		!strings.Contains(rec.Body.String(), `"model":"qwen3:4b-instruct"`) {
		t.Errorf("get: %d %s", rec.Code, rec.Body)
	}

	body := `{"provider":"anthropic","base_url":"https://api.anthropic.com/v1","model":"claude-haiku-4-5","api_key":"sk-ant-1"}`

	if rec := serveSettingsAPI(Services{Endpoints: own}, server, http.MethodPost, "/api/me/model/test", body); rec.Code != http.StatusOK ||
		!own.tested || own.user != testUser.ID || !strings.Contains(rec.Body.String(), `"reply":"OK"`) {
		t.Errorf("test: %d %s", rec.Code, rec.Body)
	}

	rec = serveSettingsAPI(Services{Endpoints: own}, server, http.MethodPut, "/api/me/model", body)
	if rec.Code != http.StatusOK || own.got.Model != "claude-haiku-4-5" || *own.got.APIKey != "sk-ant-1" || strings.Contains(rec.Body.String(), "sk-ant-1") {
		t.Errorf("set: %d %s; the key must never come back", rec.Code, rec.Body)
	}

	own.own = &endpoints.Endpoint{Provider: "anthropic", Model: "claude-haiku-4-5", KeySet: true, KeyHint: "…ant1"}
	if rec := serveSettingsAPI(Services{Endpoints: own}, server, http.MethodGet, "/api/me/model", ""); !strings.Contains(rec.Body.String(), `"key_hint":"…ant1"`) {
		t.Errorf("get with one set: %s", rec.Body)
	}

	if rec := serveSettingsAPI(Services{Endpoints: own}, server, http.MethodDelete, "/api/me/model", ""); rec.Code != http.StatusNoContent || own.own != nil {
		t.Errorf("delete: %d", rec.Code)
	}

	for err, want := range map[error]int{
		fmt.Errorf("%w: use https", endpoints.ErrInvalid):    http.StatusBadRequest,
		fmt.Errorf("%w: 401 invalid key", endpoints.ErrTest): http.StatusBadGateway,
		errors.New("postgres down"):                          http.StatusInternalServerError,
	} {
		if rec := serveSettingsAPI(Services{Endpoints: &fakeEndpoints{err: err}}, server, http.MethodPost, "/api/me/model/test", body); rec.Code != want {
			t.Errorf("%v: %d, want %d", err, rec.Code, want)
		}
	}

	if rec := serveSettingsAPI(Services{}, ServerModel{}, http.MethodGet, "/api/me/model", ""); !strings.Contains(rec.Body.String(), `"available":false`) ||
		!strings.Contains(rec.Body.String(), `"server":null`) {
		t.Errorf("without own endpoints or a server model: %s", rec.Body)
	}

	if rec := serveSettingsAPI(Services{}, server, http.MethodPut, "/api/me/model", body); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("set without own endpoints: %d", rec.Code)
	}
}

func TestNoModelAtAllIsSaidPlainly(t *testing.T) {
	rec := serveAnswer(&fakeAnswerer{err: llm.ErrNoModel}, "/answer?q=goroutines")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "none is set in your settings") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}
