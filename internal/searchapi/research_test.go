package searchapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"web-crawler/internal/labs"
	"web-crawler/internal/research"
)

type fakeResearch struct {
	owner, lab int64
	request    string
	ask        research.Ask
	run        *research.Run
	err        error
}

func (f *fakeResearch) Start(_ context.Context, owner, lab int64, ask research.Ask) (research.Run, error) {
	f.owner, f.lab, f.request, f.ask = owner, lab, ask.Request, ask

	budget := research.Budget{Found: 300, Read: 50}
	if ask.Found != nil {
		budget.Found = *ask.Found
	}

	if ask.Read != nil {
		budget.Read = *ask.Read
	}

	return research.Run{ID: 4, LabID: lab, Status: research.StatusRunning, Budget: budget}, f.err
}

func (f *fakeResearch) Map(_ context.Context, owner, lab int64) (research.Map, error) {
	f.owner, f.lab = owner, lab
	return research.Map{Run: f.run, Subtopics: []research.Subtopic{{ID: 1, Name: "Blocks"}},
		Works: []research.Work{{ID: 2, Title: "ResNet", URL: "https://arxiv.org/abs/1512.03385", Kind: research.KindPaper, Verdict: research.VerdictKeep, SubtopicID: 1,
			Finds: []research.Find{{Channel: research.ChannelSearch, Query: "resnet"}}}},
		Links: []research.Link{}, Coverage: research.Coverage{Works: 1, Kept: 1, Subtopics: []research.SubtopicCoverage{{ID: 1, Kept: 1}}}}, f.err
}

func serveResearch(service ResearchService, method, target, body string) *httptest.ResponseRecorder {
	services := Services{Labs: &fakeLabs{}}
	if service != nil {
		services.Research = service
	}

	return do(newTestHandler(services, testConfig()), signedIn(httptest.NewRequest(method, target, strings.NewReader(body))))
}

func TestResearchIsStartedAndMappedForTheOwner(t *testing.T) {
	f := &fakeResearch{}

	rec := serveResearch(f, http.MethodPost, "/api/labs/7/research", `{"request": "newer work", "found": 100, "read": 0}`)
	if rec.Code != http.StatusAccepted || f.owner != testUser.ID || f.lab != 7 || f.request != "newer work" || *f.ask.Found != 100 || *f.ask.Read != 0 {
		t.Errorf("start: %d %s %+v", rec.Code, rec.Body, f)
	}

	if serveResearch(f, http.MethodPost, "/api/labs/7/research", `{}`); f.ask.Found != nil || f.ask.Read != nil {
		t.Errorf("no budget given takes the server's: %+v", f.ask)
	}

	if rec := serveResearch(f, http.MethodGet, "/api/labs/7/research", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"title":"ResNet"`) {
		t.Errorf("map: %d %s", rec.Code, rec.Body)
	}

	if rec := serveResearch(f, http.MethodPost, "/api/labs/7/research", `{"budget": 1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown field: %d", rec.Code)
	}

	for _, c := range []struct {
		err  error
		want int
	}{
		{research.ErrRunning, http.StatusConflict},
		{research.ErrNoChannels, http.StatusBadRequest},
		{research.ErrUnavailable, http.StatusServiceUnavailable},
		{labs.ErrNotFound, http.StatusNotFound},
		{labs.ErrInvalid, http.StatusBadRequest},
		{errors.New("boom"), http.StatusInternalServerError},
	} {
		if rec := serveResearch(&fakeResearch{err: c.err}, http.MethodPost, "/api/labs/7/research", `{}`); rec.Code != c.want || strings.Contains(rec.Body.String(), "boom") {
			t.Errorf("%v: %d %s", c.err, rec.Code, rec.Body)
		}
	}

	if rec := serveResearch(nil, http.MethodGet, "/api/labs/7/research", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("without research: %d", rec.Code)
	}
}
