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
	owner, lab string
	request    string
	ask        research.Ask
	run        *research.Run
	err        error
}

func (f *fakeResearch) Start(_ context.Context, owner, lab string, ask research.Ask) (research.Run, error) {
	f.owner, f.lab, f.request, f.ask = owner, lab, ask.Request, ask

	budget := research.Budget{Found: 300, Read: 50}
	if ask.Found != nil {
		budget.Found = *ask.Found
	}

	if ask.Read != nil {
		budget.Read = *ask.Read
	}

	return research.Run{ID: tid(4), LabID: lab, Status: research.StatusRunning, Budget: budget}, f.err
}

func (f *fakeResearch) Map(_ context.Context, owner, lab string) (research.Map, error) {
	f.owner, f.lab = owner, lab
	return research.Map{Run: f.run, Subtopics: []research.Subtopic{{ID: tid(1), Name: "Blocks"}},
		Works: []research.Work{{ID: tid(2), Title: "ResNet", URL: "https://arxiv.org/abs/1512.03385", Kind: research.KindPaper, Verdict: research.VerdictKeep, SubtopicID: tid(1),
			Finds: []research.Find{{Channel: research.ChannelSearch, Query: "resnet"}}}},
		Links: []research.Link{}, Coverage: research.Coverage{Works: 1, Kept: 1, Subtopics: []research.SubtopicCoverage{{ID: tid(1), Kept: 1}}}}, f.err
}

func (f *fakeResearch) SetVerdict(_ context.Context, owner, lab, work, verdict, subtopic string) (research.Work, error) {
	f.owner, f.lab = owner, lab
	return research.Work{ID: work, Verdict: verdict, SubtopicID: subtopic, ByUser: true}, f.err
}

func (f *fakeResearch) ReadWork(_ context.Context, owner, lab, work string) (research.Work, error) {
	f.owner, f.lab = owner, lab
	return research.Work{ID: work, Verdict: research.VerdictKeep, SourceID: tid(5), ByUser: true}, f.err
}

func (f *fakeResearch) RenameSubtopic(_ context.Context, owner, lab, subtopic, name, focus string) (research.Subtopic, error) {
	f.owner, f.lab, f.request = owner, lab, subtopic+" "+name+" "+focus
	return research.Subtopic{ID: subtopic, Name: name, Focus: focus}, f.err
}

func (f *fakeResearch) MergeSubtopics(_ context.Context, owner, lab, from, into string) error {
	f.owner, f.lab, f.request = owner, lab, from+" into "+into
	return f.err
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

	rec := serveResearch(f, http.MethodPost, "/api/labs/"+tid(7)+"/research", `{"request": "newer work", "found": 100, "read": 0}`)
	if rec.Code != http.StatusAccepted || f.owner != testUser.ID || f.lab != tid(7) || f.request != "newer work" || *f.ask.Found != 100 || *f.ask.Read != 0 {
		t.Errorf("start: %d %s %+v", rec.Code, rec.Body, f)
	}

	if serveResearch(f, http.MethodPost, "/api/labs/"+tid(7)+"/research", `{}`); f.ask.Found != nil || f.ask.Read != nil {
		t.Errorf("no budget given takes the server's: %+v", f.ask)
	}

	if rec := serveResearch(f, http.MethodGet, "/api/labs/"+tid(7)+"/research", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"title":"ResNet"`) {
		t.Errorf("map: %d %s", rec.Code, rec.Body)
	}

	if rec := serveResearch(f, http.MethodPost, "/api/labs/"+tid(7)+"/research", `{"budget": 1}`); rec.Code != http.StatusBadRequest {
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
		if rec := serveResearch(&fakeResearch{err: c.err}, http.MethodPost, "/api/labs/"+tid(7)+"/research", `{}`); rec.Code != c.want || strings.Contains(rec.Body.String(), "boom") {
			t.Errorf("%v: %d %s", c.err, rec.Code, rec.Body)
		}
	}

	if rec := serveResearch(f, http.MethodPost, "/api/labs/"+tid(7)+"/research", `{"subtopic": "`+tid(3)+`"}`); rec.Code != http.StatusAccepted || f.ask.Focus.Subtopic != tid(3) {
		t.Errorf("one subtopic: %d %+v", rec.Code, f.ask)
	}

	if rec := serveResearch(f, http.MethodPost, "/api/labs/"+tid(7)+"/research", `{"work": "12"}`); rec.Code != http.StatusNotFound {
		t.Errorf("a malformed work: %d %s", rec.Code, rec.Body)
	}

	if rec := serveResearch(f, http.MethodPut, "/api/labs/"+tid(7)+"/research/works/"+tid(12), `{"verdict": "drop"}`); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"verdict":"drop"`) {
		t.Errorf("verdict: %d %s", rec.Code, rec.Body)
	}

	if rec := serveResearch(f, http.MethodPost, "/api/labs/"+tid(7)+"/research/works/"+tid(12)+"/read", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"source_id":"`+tid(5)+`"`) {
		t.Errorf("read: %d %s", rec.Code, rec.Body)
	}

	if rec := serveResearch(f, http.MethodPut, "/api/labs/"+tid(7)+"/research/works/x", `{"verdict": "drop"}`); rec.Code != http.StatusNotFound {
		t.Errorf("a work that is no number: %d", rec.Code)
	}

	for _, c := range []struct {
		err  error
		want int
	}{{research.ErrWorkNotFound, http.StatusNotFound}, {research.ErrSubtopicNotFound, http.StatusNotFound}, {research.ErrNothingToFollow, http.StatusBadRequest}} {
		if rec := serveResearch(&fakeResearch{err: c.err}, http.MethodPut, "/api/labs/"+tid(7)+"/research/works/"+tid(1), `{"verdict": "keep"}`); rec.Code != c.want {
			t.Errorf("%v: %d", c.err, rec.Code)
		}
	}

	if rec := serveResearch(nil, http.MethodGet, "/api/labs/"+tid(7)+"/research", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("without research: %d", rec.Code)
	}
}

func TestSubtopicsAreRenamedAndMergedForTheOwner(t *testing.T) {
	f := &fakeResearch{}
	path := "/api/labs/" + tid(7) + "/research/subtopics/"

	rec := serveResearch(f, http.MethodPut, path+tid(1), `{"name":"Blocks","focus":"what a block computes"}`)
	if rec.Code != http.StatusOK || f.owner != testUser.ID || f.lab != tid(7) || f.request != tid(1)+" Blocks what a block computes" {
		t.Errorf("rename: %d %s %+v", rec.Code, rec.Body, f)
	}

	if rec := serveResearch(f, http.MethodPost, path+tid(2)+"/merge", `{"into":"`+strings.ToUpper(tid(1))+`"}`); rec.Code != http.StatusOK || f.request != tid(2)+" into "+tid(1) {
		t.Errorf("merge: %d %s %+v", rec.Code, rec.Body, f)
	}

	if rec := serveResearch(f, http.MethodPost, path+tid(2)+"/merge", `{"into":"7"}`); rec.Code != http.StatusNotFound {
		t.Errorf("merge into a malformed id: %d", rec.Code)
	}

	if rec := serveResearch(f, http.MethodPut, path+"1", `{"name":"x"}`); rec.Code != http.StatusNotFound {
		t.Errorf("a malformed subtopic: %d", rec.Code)
	}

	for err, want := range map[error]int{research.ErrSubtopicTaken: http.StatusConflict, research.ErrRunning: http.StatusConflict,
		research.ErrSubtopicNotFound: http.StatusNotFound} {
		f.err = err
		if rec := serveResearch(f, http.MethodPut, path+tid(1), `{"name":"x"}`); rec.Code != want {
			t.Errorf("%v: %d, want %d", err, rec.Code, want)
		}
	}
}
