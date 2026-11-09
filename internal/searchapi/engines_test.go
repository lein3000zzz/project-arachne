package searchapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"web-crawler/internal/websearch"
)

type fakeEngines struct{ err error }

func (f fakeEngines) Engines(context.Context) ([]string, error) {
	return []string{"bing", "brave"}, f.err
}

func serveEngines(lister EngineLister, lookUp WebLookup) *httptest.ResponseRecorder {
	cfg := testConfig()
	cfg.DefaultEngines = []string{"brave", "google"}

	services := Services{WebLookup: lookUp}
	if lister != nil {
		services.Engines = lister
	}

	return do(newTestHandler(services, cfg), signedIn(httptest.NewRequest(http.MethodGet, "/api/engines", nil)))
}

func TestEnginesAreThoseThatAnswer(t *testing.T) {
	if rec := serveEngines(fakeEngines{}, nil); rec.Code != http.StatusOK ||
		rec.Body.String() != `{"available":["bing","brave"],"default":["brave"],"look_up":false}`+"\n" {
		t.Errorf("engines: %d %s", rec.Code, rec.Body)
	}

	if rec := serveEngines(fakeEngines{}, &fakeLookup{}); rec.Body.String() != `{"available":["bing","brave"],"default":["brave"],"look_up":true}`+"\n" {
		t.Errorf("with look-ups: %s", rec.Body)
	}

	for _, lister := range []EngineLister{nil, fakeEngines{err: websearch.ErrUnavailable}} {
		if rec := serveEngines(lister, &fakeLookup{}); rec.Code != http.StatusOK ||
			rec.Body.String() != `{"available":[],"default":[],"look_up":false}`+"\n" {
			t.Errorf("without engines: %d %s", rec.Code, rec.Body)
		}
	}
}
