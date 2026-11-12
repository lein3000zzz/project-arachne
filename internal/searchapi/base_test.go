package searchapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"web-crawler/internal/pages"
)

type fakeBase struct {
	stats pages.Stats
	err   error
}

func (f fakeBase) Stats(context.Context) (pages.Stats, error) { return f.stats, f.err }

func TestTheSharedBaseIsCounted(t *testing.T) {
	get := func(services Services) *httptest.ResponseRecorder {
		return do(newTestHandler(services, testConfig()), signedIn(httptest.NewRequest(http.MethodGet, "/api/base", nil)))
	}

	rec := get(Services{Base: fakeBase{stats: pages.Stats{Pages: 1200, Sites: 40, SetAside: 3, Waiting: 7}}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"pages":1200`) || !strings.Contains(rec.Body.String(), `"waiting":7`) {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}

	if rec := get(Services{}); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("without stats: %d", rec.Code)
	}

	if rec := get(Services{Base: fakeBase{err: errors.New("db down")}}); rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "db down") {
		t.Errorf("a failure: %d %s", rec.Code, rec.Body)
	}

	if rec := do(newTestHandler(Services{Base: fakeBase{}}, testConfig()), httptest.NewRequest(http.MethodGet, "/api/base", nil)); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d", rec.Code)
	}
}
