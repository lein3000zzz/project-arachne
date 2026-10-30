package searchapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"web-crawler/internal/answer"
	"web-crawler/internal/labs"
)

type fakeLabs struct {
	LabService

	owner   int64
	labIDs  []int64
	created createLabRequest
	request string
	err     error
}

func (f *fakeLabs) List(_ context.Context, owner int64) ([]labs.Lab, error) {
	f.owner = owner
	return []labs.Lab{{ID: 1, Name: "go"}}, f.err
}

func (f *fakeLabs) Create(_ context.Context, owner int64, name, topic string, sources []labs.SourceRequest) (labs.Detail, error) {
	f.owner, f.created = owner, createLabRequest{Name: name, Topic: topic, Sources: sources}
	return labs.Detail{Lab: labs.Lab{ID: 7, Name: name}}, f.err
}

func (f *fakeLabs) Get(_ context.Context, owner, lab int64) (labs.Detail, error) {
	f.owner, f.labIDs = owner, []int64{lab}
	return labs.Detail{Lab: labs.Lab{ID: lab}}, f.err
}

func (f *fakeLabs) Delete(_ context.Context, owner, lab int64) error {
	f.owner, f.labIDs = owner, []int64{lab}
	return f.err
}

func (f *fakeLabs) Plan(_ context.Context, owner, lab int64, request string) (labs.Plan, error) {
	f.owner, f.labIDs, f.request = owner, []int64{lab}, request
	return labs.Plan{LabID: lab, Name: "Go", Sources: []labs.SourceRequest{}}, f.err
}

func (f *fakeLabs) Refresh(_ context.Context, owner, lab, source int64) (labs.SourceStatus, error) {
	f.owner, f.labIDs = owner, []int64{lab, source}
	return labs.SourceStatus{Source: labs.Source{ID: source}}, f.err
}

func (f *fakeLabs) PageURLs(_ context.Context, owner int64, ids []int64) ([]string, error) {
	f.owner, f.labIDs = owner, ids
	if f.err != nil {
		return nil, f.err
	}

	return []string{"https://go.dev/doc/"}, nil
}

type recordingAnswerer struct {
	got answer.Request
}

func (a *recordingAnswerer) Answer(_ context.Context, req answer.Request, events answer.Events) (answer.Result, error) {
	a.got = req
	if events.Sources != nil {
		_ = events.Sources(nil)
	}

	return answer.Result{Scope: answer.ScopeLabs, Cited: []int{}}, nil
}

func serveLabs(service LabService, method, target, body string) *httptest.ResponseRecorder {
	handler := newTestHandler(Services{Labs: service}, testConfig())
	return do(handler, signedIn(httptest.NewRequest(method, target, strings.NewReader(body))))
}

func TestLabRequestsActAsTheSignedInUser(t *testing.T) {
	service := &fakeLabs{}

	rec := serveLabs(service, http.MethodPost, "/api/labs",
		`{"name":"Go","topic":"concurrency","sources":[{"url":"go.dev/doc/","scope":"section"}]}`)
	if rec.Code != http.StatusCreated || service.owner != testUser.ID || service.created.Sources[0].Scope != "section" {
		t.Errorf("create: %d %s, owner %d, %+v", rec.Code, rec.Body, service.owner, service.created)
	}

	rec = serveLabs(service, http.MethodPost, "/api/labs/plan", `{"request":"a lab on go.dev","lab_id":3}`)
	if rec.Code != http.StatusOK || service.request != "a lab on go.dev" || service.labIDs[0] != 3 {
		t.Errorf("plan: %d %s", rec.Code, rec.Body)
	}

	if rec := serveLabs(service, http.MethodPost, "/api/labs/4/sources/9/refresh", ""); rec.Code != http.StatusAccepted ||
		fmt.Sprint(service.labIDs) != "[4 9]" {
		t.Errorf("refresh: %d, ids %v", rec.Code, service.labIDs)
	}

	if rec := serveLabs(service, http.MethodDelete, "/api/labs/4", ""); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Errorf("delete: %d %q", rec.Code, rec.Body)
	}

	if rec := serveLabs(service, http.MethodGet, "/api/labs/not-a-number", ""); rec.Code != http.StatusNotFound {
		t.Errorf("a malformed id: %d", rec.Code)
	}

	if rec := serveLabs(service, http.MethodPatch, "/api/labs/4", `{"name":"mine now"}`); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("labs are not edited by hand: PATCH gave %d", rec.Code)
	}

	if rec := serveLabs(service, http.MethodPost, "/api/labs", `{"name":"Go","owner_id":2}`); rec.Code != http.StatusBadRequest {
		t.Errorf("an owner in the body: %d, want it refused", rec.Code)
	}
}

func TestLabErrorsMapToStatuses(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{labs.ErrNotFound, http.StatusNotFound},
		{labs.ErrNameTaken, http.StatusConflict},
		{labs.ErrSourceExists, http.StatusConflict},
		{fmt.Errorf("%w: bad", labs.ErrInvalid), http.StatusBadRequest},
		{labs.ErrCrawlingDisabled, http.StatusServiceUnavailable},
		{context.DeadlineExceeded, http.StatusGatewayTimeout},
		{errors.New("postgres is down"), http.StatusInternalServerError},
	}

	for _, tc := range cases {
		if rec := serveLabs(&fakeLabs{err: tc.err}, http.MethodGet, "/api/labs", ""); rec.Code != tc.want {
			t.Errorf("%v: %d, want %d", tc.err, rec.Code, tc.want)
		}
	}

	if rec := serveLabs(nil, http.MethodGet, "/api/labs", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("without labs: %d", rec.Code)
	}
}

func TestAnswersAndSearchesFromLabs(t *testing.T) {
	service, answerer, searcher := &fakeLabs{}, &recordingAnswerer{}, &fakeSearcher{}
	handler := newTestHandler(Services{Labs: service, Answerer: answerer, Searcher: searcher}, testConfig())

	rec := do(handler, signedIn(httptest.NewRequest(http.MethodGet, "/answer/stream?q=x&labs=1,%202", nil)))
	if rec.Code != http.StatusOK || fmt.Sprint(answerer.got.LabURLs) != "[https://go.dev/doc/]" ||
		fmt.Sprint(service.labIDs) != "[1 2]" || service.owner != testUser.ID || !strings.Contains(rec.Body.String(), `"scope":"labs"`) {
		t.Errorf("answer: %d %s, urls %v, ids %v", rec.Code, rec.Body, answerer.got.LabURLs, service.labIDs)
	}

	if rec := do(handler, signedIn(httptest.NewRequest(http.MethodGet, "/answer?q=x", nil))); rec.Code != http.StatusOK || answerer.got.LabURLs != nil {
		t.Errorf("no labs must mean the shared base: %v", answerer.got.LabURLs)
	}

	if rec := do(handler, signedIn(httptest.NewRequest(http.MethodGet, "/search?q=x&labs=5", nil))); rec.Code != http.StatusOK ||
		fmt.Sprint(searcher.got.URLs) != "[https://go.dev/doc/]" {
		t.Errorf("search: %d, urls %v", rec.Code, searcher.got.URLs)
	}

	for target, want := range map[string]int{
		"/answer?q=x&labs=one":                   http.StatusBadRequest,
		"/search?q=x&labs=1,,2":                  http.StatusBadRequest,
		"/answer?q=x&labs=" + manyLabs(21):       http.StatusBadRequest,
		"/answer/stream?q=x&labs=" + manyLabs(1): http.StatusOK,
	} {
		if rec := do(handler, signedIn(httptest.NewRequest(http.MethodGet, target, nil))); rec.Code != want {
			t.Errorf("%s: %d, want %d", target, rec.Code, want)
		}
	}

	theirs := newTestHandler(Services{Labs: &fakeLabs{err: labs.ErrNotFound}, Answerer: answerer, Searcher: searcher}, testConfig())

	if rec := do(theirs, signedIn(httptest.NewRequest(http.MethodGet, "/answer/stream?q=x&labs=9", nil))); rec.Code != http.StatusNotFound {
		t.Errorf("another user's lab: %d", rec.Code)
	}
}

func manyLabs(n int) string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprint(i + 1)
	}

	return strings.Join(ids, ",")
}
