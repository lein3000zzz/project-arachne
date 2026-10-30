package searchapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"web-crawler/internal/gathering"
	"web-crawler/internal/labs"
	"web-crawler/internal/websearch"
)

type fakeGatherer struct {
	owner, lab int64
	request    string
	err        error
	enginesErr error
}

func (f *fakeGatherer) Engines(context.Context) ([]string, []string, error) {
	return []string{"bing", "brave"}, []string{"brave"}, f.enginesErr
}

func (f *fakeGatherer) Start(_ context.Context, owner, lab int64, request string) (gathering.Gathering, error) {
	f.owner, f.lab, f.request = owner, lab, request
	return gathering.Gathering{ID: 3, LabID: lab, Status: gathering.StatusRunning}, f.err
}

func (f *fakeGatherer) Latest(_ context.Context, owner, lab int64) (gathering.Gathering, error) {
	f.owner, f.lab = owner, lab
	return gathering.Gathering{ID: 3, LabID: lab, Status: gathering.StatusDone}, f.err
}

func serveGather(g Gatherer, method, target, body string) *httptest.ResponseRecorder {
	services := Services{Labs: &fakeLabs{}}
	if g != nil {
		services.Gatherer = g
	}

	return do(newTestHandler(services, testConfig()), signedIn(httptest.NewRequest(method, target, strings.NewReader(body))))
}

func TestGatheringEndpoints(t *testing.T) {
	g := &fakeGatherer{}

	rec := serveGather(g, http.MethodPost, "/api/labs/7/gather", `{"request":"benchmarks too"}`)
	if rec.Code != http.StatusAccepted || g.owner != testUser.ID || g.lab != 7 || g.request != "benchmarks too" ||
		!strings.Contains(rec.Body.String(), `"status":"running"`) {
		t.Errorf("start: %d %s", rec.Code, rec.Body)
	}

	if rec := serveGather(g, http.MethodGet, "/api/labs/7/gathering", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"done"`) {
		t.Errorf("latest: %d %s", rec.Code, rec.Body)
	}

	if rec := serveGather(g, http.MethodGet, "/api/engines", ""); rec.Code != http.StatusOK ||
		rec.Body.String() != `{"available":["bing","brave"],"default":["brave"]}`+"\n" {
		t.Errorf("engines: %d %s", rec.Code, rec.Body)
	}

	for _, c := range []struct {
		err  error
		want int
	}{
		{gathering.ErrRunning, http.StatusConflict},
		{gathering.ErrNoEngines, http.StatusBadRequest},
		{gathering.ErrNotFound, http.StatusNotFound},
		{gathering.ErrUnavailable, http.StatusServiceUnavailable},
		{websearch.ErrUnavailable, http.StatusServiceUnavailable},
		{labs.ErrNotFound, http.StatusNotFound},
		{errors.New("boom"), http.StatusInternalServerError},
	} {
		if rec := serveGather(&fakeGatherer{err: c.err}, http.MethodPost, "/api/labs/7/gather", `{}`); rec.Code != c.want {
			t.Errorf("%v: %d, want %d", c.err, rec.Code, c.want)
		}
	}
}

func TestWithoutWebSearch(t *testing.T) {
	if rec := serveGather(nil, http.MethodGet, "/api/engines", ""); rec.Code != http.StatusOK ||
		rec.Body.String() != `{"available":[],"default":[]}`+"\n" {
		t.Errorf("engines: %d %s", rec.Code, rec.Body)
	}

	if rec := serveGather(nil, http.MethodPost, "/api/labs/7/gather", `{}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("gather: %d", rec.Code)
	}

	if rec := serveGather(&fakeGatherer{enginesErr: websearch.ErrUnavailable}, http.MethodGet, "/api/engines", ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"available":[]`) {
		t.Errorf("engines down: %d %s", rec.Code, rec.Body)
	}
}
