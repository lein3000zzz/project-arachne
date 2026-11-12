package searchapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"web-crawler/internal/answer"
	"web-crawler/internal/labs"
)

type fakeLabs struct {
	LabService

	owner   string
	labIDs  []string
	created createLabRequest
	request string
	picks   []labs.Cited
	files   []labs.File
	err     error
}

func (f *fakeLabs) AddUploads(_ context.Context, owner, lab string, files []labs.File) (labs.Detail, error) {
	f.owner, f.labIDs, f.files = owner, []string{lab}, files
	return labs.Detail{Lab: labs.Lab{ID: lab}}, f.err
}

func (f *fakeLabs) References(_ context.Context, owner, lab string) ([]labs.Document, error) {
	f.owner, f.labIDs = owner, []string{lab}
	return []labs.Document{{URL: "https://arxiv.org/pdf/1512.03385", References: []labs.Reference{{URL: "https://arxiv.org/abs/1409.1556"}}}}, f.err
}

func (f *fakeLabs) AddReferences(_ context.Context, owner, lab string, picks []labs.Cited) (labs.Detail, error) {
	f.owner, f.labIDs, f.picks = owner, []string{lab}, picks
	return labs.Detail{Lab: labs.Lab{ID: lab}}, f.err
}

func (f *fakeLabs) List(_ context.Context, owner string) ([]labs.Lab, error) {
	f.owner = owner
	return []labs.Lab{{ID: tid(1), Name: "go"}}, f.err
}

func (f *fakeLabs) Create(_ context.Context, owner, name, topic string, sources []labs.SourceRequest, engines []string) (labs.Detail, error) {
	f.owner, f.created = owner, createLabRequest{Name: name, Topic: topic, Sources: sources, Engines: engines}
	return labs.Detail{Lab: labs.Lab{ID: tid(7), Name: name}}, f.err
}

func (f *fakeLabs) Get(_ context.Context, owner, lab string) (labs.Detail, error) {
	f.owner, f.labIDs = owner, []string{lab}
	return labs.Detail{Lab: labs.Lab{ID: lab}}, f.err
}

func (f *fakeLabs) Rename(_ context.Context, owner, lab, name, topic string) (labs.Lab, error) {
	f.owner, f.labIDs, f.created = owner, []string{lab}, createLabRequest{Name: name, Topic: topic}
	return labs.Lab{ID: lab, Name: name, Topic: topic}, f.err
}

func (f *fakeLabs) Delete(_ context.Context, owner, lab string) error {
	f.owner, f.labIDs = owner, []string{lab}
	return f.err
}

func (f *fakeLabs) Plan(_ context.Context, owner, lab, request string) (labs.Plan, error) {
	f.owner, f.labIDs, f.request = owner, []string{lab}, request
	return labs.Plan{LabID: lab, Name: "Go", Sources: []labs.SourceRequest{}}, f.err
}

func (f *fakeLabs) Refresh(_ context.Context, owner, lab, source string) (labs.SourceStatus, error) {
	f.owner, f.labIDs = owner, []string{lab, source}
	return labs.SourceStatus{Source: labs.Source{ID: source}}, f.err
}

func (f *fakeLabs) Quality(_ context.Context, owner, lab string) (labs.Quality, error) {
	f.owner, f.labIDs = owner, []string{lab}
	return labs.QualityOf([]labs.QualityPage{{URL: "https://go.dev/", Host: "go.dev", Status: 200, Decision: "index", Hash: "1"},
		{URL: "https://go.dev/menu", Host: "go.dev", Status: 200, Decision: "skip", Reasons: []string{"navigation_only"}}}), f.err
}

func (f *fakeLabs) PageURLs(_ context.Context, owner string, ids []string) ([]string, error) {
	f.owner, f.labIDs = owner, ids
	if f.err != nil {
		return nil, f.err
	}

	return []string{"https://go.dev/doc/"}, nil
}

type recordingAnswerer struct {
	got answer.Request
}

func (a *recordingAnswerer) Verifying() bool { return false }

func (a *recordingAnswerer) Verify(context.Context, answer.Result, answer.VerifyEvents) (answer.Verification, error) {
	return answer.Verification{}, nil
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
		t.Errorf("create: %d %s, owner %s, %+v", rec.Code, rec.Body, service.owner, service.created)
	}

	rec = serveLabs(service, http.MethodPost, "/api/labs/plan", `{"request":"a lab on go.dev","lab_id":"`+tid(3)+`"}`)
	if rec.Code != http.StatusOK || service.request != "a lab on go.dev" || service.labIDs[0] != tid(3) {
		t.Errorf("plan: %d %s", rec.Code, rec.Body)
	}

	if rec := serveLabs(service, http.MethodPost, "/api/labs/"+tid(4)+"/sources/"+tid(9)+"/refresh", ""); rec.Code != http.StatusAccepted ||
		!slices.Equal(service.labIDs, []string{tid(4), tid(9)}) {
		t.Errorf("refresh: %d, ids %v", rec.Code, service.labIDs)
	}

	if rec := serveLabs(service, http.MethodGet, "/api/labs/"+tid(4)+"/references", ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"documents":[{"url":"https://arxiv.org/pdf/1512.03385"`) || service.labIDs[0] != tid(4) {
		t.Errorf("references: %d %s", rec.Code, rec.Body)
	}

	rec = serveLabs(service, http.MethodPost, "/api/labs/"+tid(4)+"/references",
		`{"references":[{"document":"https://arxiv.org/pdf/1512.03385","url":"https://arxiv.org/abs/1409.1556"}]}`)
	if rec.Code != http.StatusOK || service.owner != testUser.ID ||
		fmt.Sprint(service.picks) != "[{https://arxiv.org/pdf/1512.03385 https://arxiv.org/abs/1409.1556}]" {
		t.Errorf("add references: %d %s, picks %+v", rec.Code, rec.Body, service.picks)
	}

	if rec := serveLabs(service, http.MethodDelete, "/api/labs/"+tid(4), ""); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Errorf("delete: %d %q", rec.Code, rec.Body)
	}

	if rec := serveLabs(service, http.MethodGet, "/api/labs/7", ""); rec.Code != http.StatusNotFound {
		t.Errorf("a malformed id: %d", rec.Code)
	}

	if rec := serveLabs(service, http.MethodPatch, "/api/labs/"+tid(4), `{"name":"mine now"}`); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("labs are not edited by hand: PATCH gave %d", rec.Code)
	}

	if rec := serveLabs(service, http.MethodPost, "/api/labs", `{"name":"Go","owner_id":2}`); rec.Code != http.StatusBadRequest {
		t.Errorf("an owner in the body: %d, want it refused", rec.Code)
	}
}

func TestALabsQualityIsReported(t *testing.T) {
	f := &fakeLabs{}

	rec := serveLabs(f, http.MethodGet, "/api/labs/"+tid(7)+"/quality", "")
	if rec.Code != http.StatusOK || f.owner != testUser.ID || f.labIDs[0] != tid(7) || !strings.Contains(rec.Body.String(), `"verdict":"mixed"`) ||
		!strings.Contains(rec.Body.String(), "1 menus only") {
		t.Errorf("quality: %d %s", rec.Code, rec.Body)
	}

	if rec := serveLabs(&fakeLabs{err: labs.ErrNotFound}, http.MethodGet, "/api/labs/"+tid(7)+"/quality", ""); rec.Code != http.StatusNotFound {
		t.Errorf("another user's lab: %d", rec.Code)
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

	rec := do(handler, signedIn(httptest.NewRequest(http.MethodGet, "/answer/stream?q=x&labs="+tid(1)+",%20"+strings.ToUpper(tid(2)), nil)))
	if rec.Code != http.StatusOK || fmt.Sprint(answerer.got.LabURLs) != "[https://go.dev/doc/]" ||
		!slices.Equal(service.labIDs, []string{tid(1), tid(2)}) || service.owner != testUser.ID || !strings.Contains(rec.Body.String(), `"scope":"labs"`) {
		t.Errorf("answer: %d %s, urls %v, ids %v", rec.Code, rec.Body, answerer.got.LabURLs, service.labIDs)
	}

	if rec := do(handler, signedIn(httptest.NewRequest(http.MethodGet, "/answer?q=x", nil))); rec.Code != http.StatusOK || answerer.got.LabURLs != nil {
		t.Errorf("no labs must mean the shared base: %v", answerer.got.LabURLs)
	}

	if rec := do(handler, signedIn(httptest.NewRequest(http.MethodGet, "/search?q=x&labs="+tid(5), nil))); rec.Code != http.StatusOK ||
		fmt.Sprint(searcher.got.URLs) != "[https://go.dev/doc/]" {
		t.Errorf("search: %d, urls %v", rec.Code, searcher.got.URLs)
	}

	for target, want := range map[string]int{
		"/answer?q=x&labs=one":                       http.StatusBadRequest,
		"/search?q=x&labs=" + tid(1) + ",," + tid(2): http.StatusBadRequest,
		"/search?q=x&labs=1":                         http.StatusBadRequest,
		"/answer?q=x&labs=" + manyLabs(21):           http.StatusBadRequest,
		"/answer/stream?q=x&labs=" + manyLabs(1):     http.StatusOK,
	} {
		if rec := do(handler, signedIn(httptest.NewRequest(http.MethodGet, target, nil))); rec.Code != want {
			t.Errorf("%s: %d, want %d", target, rec.Code, want)
		}
	}

	theirs := newTestHandler(Services{Labs: &fakeLabs{err: labs.ErrNotFound}, Answerer: answerer, Searcher: searcher}, testConfig())

	if rec := do(theirs, signedIn(httptest.NewRequest(http.MethodGet, "/answer/stream?q=x&labs="+tid(9), nil))); rec.Code != http.StatusNotFound {
		t.Errorf("another user's lab: %d", rec.Code)
	}
}

func manyLabs(n int) string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = tid(i + 1)
	}

	return strings.Join(ids, ",")
}

func TestALabIsRenamed(t *testing.T) {
	f := &fakeLabs{}

	rec := serveLabs(f, http.MethodPut, "/api/labs/"+tid(7), `{"name":"Zig","topic":"comptime"}`)
	if rec.Code != http.StatusOK || f.labIDs[0] != tid(7) || f.created.Name != "Zig" || f.created.Topic != "comptime" || !strings.Contains(rec.Body.String(), `"name":"Zig"`) {
		t.Errorf("%d %s %+v", rec.Code, rec.Body, f)
	}

	f.err = labs.ErrNameTaken
	if rec := serveLabs(f, http.MethodPut, "/api/labs/"+tid(7), `{"name":"Rust"}`); rec.Code != http.StatusConflict {
		t.Errorf("a taken name: %d", rec.Code)
	}

	if rec := serveLabs(&fakeLabs{}, http.MethodPut, "/api/labs/7", `{"name":"Zig"}`); rec.Code != http.StatusNotFound {
		t.Errorf("an old numeric id: %d", rec.Code)
	}
}
