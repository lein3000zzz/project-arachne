package searchapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"web-crawler/internal/answer"
	"web-crawler/internal/artifacts"
	"web-crawler/internal/auth"
	"web-crawler/internal/retrieval"

	"go.uber.org/zap"
)

type fakeArtifacts struct {
	ArtifactService

	owner   int64
	lab     int64
	outline artifacts.Outline
	saved   artifacts.SavedAnswer
	err     error
}

func (f *fakeArtifacts) Draft(_ context.Context, owner, lab int64, request string) (artifacts.Outline, error) {
	f.owner, f.lab = owner, lab
	return artifacts.Outline{Title: request, Sections: []artifacts.Planned{{Heading: "A", Focus: "a"}}}, f.err
}

func (f *fakeArtifacts) Build(_ context.Context, owner, lab int64, _ string, o artifacts.Outline) (artifacts.Artifact, error) {
	f.owner, f.lab, f.outline = owner, lab, o
	return artifacts.Artifact{ID: 9, LabID: lab, Title: o.Title}, f.err
}

func (f *fakeArtifacts) Get(_ context.Context, owner, id int64, version int) (artifacts.Artifact, artifacts.Version, []int, error) {
	f.owner = owner
	v := artifacts.Version{Number: 2, Status: artifacts.StatusDone, Markdown: "# T\n",
		Sections: []artifacts.Section{{Heading: "A", State: artifacts.StateDone, Text: "Text [1]."}},
		Sources:  []artifacts.Source{{N: 1, URL: "https://go.dev/", Title: "Go"}}}

	return artifacts.Artifact{ID: id, Title: "T"}, v, []int{1, 2}, f.err
}

func (f *fakeArtifacts) SaveAnswer(_ context.Context, owner, lab int64, saved artifacts.SavedAnswer) (artifacts.Artifact, error) {
	f.owner, f.lab, f.saved = owner, lab, saved
	return artifacts.Artifact{ID: 5, LabID: lab, Kind: artifacts.KindAnswer, Title: saved.Question}, f.err
}

func serveArtifacts(service ArtifactService, recent *recentAnswers, method, target, body string) *httptest.ResponseRecorder {
	h := &artifactsHandler{logger: zap.NewNop().Sugar(), service: service, recent: recent, draftTimeout: time.Second}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/labs/{id}/artifacts/outline", h.draft)
	mux.HandleFunc("POST /api/labs/{id}/artifacts", h.build)
	mux.HandleFunc("POST /api/labs/{id}/artifacts/answers", h.saveAnswer)
	mux.HandleFunc("GET /api/artifacts/{id}", h.get)
	mux.HandleFunc("GET /api/artifacts/{id}/markdown", h.markdown)

	req := httptest.NewRequest(method, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req.WithContext(auth.WithUser(req.Context(), testUser)))

	return rec
}

func TestReportsAreDraftedAndBuiltAsTheSignedInUser(t *testing.T) {
	service := &fakeArtifacts{}

	if rec := serveArtifacts(service, nil, http.MethodPost, "/api/labs/4/artifacts/outline", `{"request":"residual networks"}`); rec.Code != http.StatusOK ||
		service.owner != testUser.ID || service.lab != 4 || !strings.Contains(rec.Body.String(), `"title":"residual networks"`) {
		t.Errorf("draft: %d %s", rec.Code, rec.Body)
	}

	rec := serveArtifacts(service, nil, http.MethodPost, "/api/labs/4/artifacts",
		`{"request":"r","outline":{"title":"T","sections":[{"heading":"B","focus":"b"},{"heading":"A","focus":"a"}]}}`)
	if rec.Code != http.StatusAccepted || service.outline.Sections[0].Heading != "B" {
		t.Errorf("build: %d %s, outline %+v", rec.Code, rec.Body, service.outline)
	}

	for err, want := range map[error]int{artifacts.ErrBuilding: http.StatusConflict, artifacts.ErrInvalid: http.StatusBadRequest,
		artifacts.ErrNotFound: http.StatusNotFound, artifacts.ErrUnavailable: http.StatusServiceUnavailable} {
		if rec := serveArtifacts(&fakeArtifacts{err: err}, nil, http.MethodPost, "/api/labs/4/artifacts/outline", `{"request":"r"}`); rec.Code != want {
			t.Errorf("%v: %d, want %d", err, rec.Code, want)
		}
	}
}

func TestAnArtifactComesRendered(t *testing.T) {
	rec := serveArtifacts(&fakeArtifacts{}, nil, http.MethodGet, "/api/artifacts/7?version=2", "")

	var got struct {
		HTML     string              `json:"html"`
		TOC      []artifacts.Heading `json:"toc"`
		Versions []int               `json:"versions"`
		Version  artifacts.Version   `json:"version"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK || !strings.Contains(got.HTML, `<a href="#ref-1"`) ||
		len(got.TOC) != 2 || len(got.Versions) != 2 || got.Version.Markdown != "" {
		t.Errorf("get: %d %s, %v", rec.Code, rec.Body, err)
	}

	rec = serveArtifacts(&fakeArtifacts{}, nil, http.MethodGet, "/api/artifacts/7/markdown", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "# T\n" || !strings.Contains(rec.Header().Get("Content-Disposition"), `artifact-7-v2.md`) {
		t.Errorf("markdown: %d %v %q", rec.Code, rec.Header(), rec.Body)
	}

	if rec := serveArtifacts(&fakeArtifacts{}, nil, http.MethodGet, "/api/artifacts/7?version=0", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("version 0: %d", rec.Code)
	}
}

func TestASavedAnswerIsTheServersOwn(t *testing.T) {
	recent := newRecentAnswers(10, time.Hour)
	res := answer.Result{Answer: "The degradation problem [1].", Model: "m",
		Sources: []answer.Source{{N: 1, Result: retrieval.Result{URL: "https://arxiv.org/abs/1512.03385"}}}}

	checking := recent.add(testUser.ID, "what do residual connections solve", res, false)
	theirs := recent.add(testUser.ID+1, "someone else's question", res, true)

	service := &fakeArtifacts{}

	if rec := serveArtifacts(service, recent, http.MethodPost, "/api/labs/4/artifacts/answers", `{"answer":"`+checking+`"}`); rec.Code != http.StatusConflict {
		t.Errorf("still being checked: %d %s", rec.Code, rec.Body)
	}

	for _, id := range []string{theirs, "made-up"} {
		if rec := serveArtifacts(service, recent, http.MethodPost, "/api/labs/4/artifacts/answers", `{"answer":"`+id+`"}`); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", id, rec.Code)
		}
	}

	recent.check(checking, []answer.Claim{{Text: "The degradation problem.", Verdict: answer.Supported}})

	// Whatever else the body carries is refused; the text is the server's.
	if rec := serveArtifacts(service, recent, http.MethodPost, "/api/labs/4/artifacts/answers",
		`{"answer":"`+checking+`","text":"Residual connections were invented by me."}`); rec.Code != http.StatusBadRequest {
		t.Errorf("a text in the body: %d", rec.Code)
	}

	rec := serveArtifacts(service, recent, http.MethodPost, "/api/labs/4/artifacts/answers", `{"answer":"`+checking+`"}`)
	if rec.Code != http.StatusCreated || service.lab != 4 || service.saved.Text != res.Answer ||
		service.saved.Question != "what do residual connections solve" || service.saved.Claims[0].Verdict != answer.Supported {
		t.Errorf("save: %d %s, saved %+v", rec.Code, rec.Body, service.saved)
	}
}

func TestAnAnsweredQuestionCanBeNamed(t *testing.T) {
	rec := serveAnswer(&fakeAnswerer{}, "/answer/stream?q=goroutines")
	if !strings.Contains(rec.Body.String(), `"id":"`) {
		t.Errorf("done carries no id:\n%s", rec.Body)
	}

	var res struct {
		ID string `json:"id"`
	}

	if err := json.Unmarshal(serveAnswer(&fakeAnswerer{}, "/answer?q=goroutines").Body.Bytes(), &res); err != nil || len(res.ID) != 40 {
		t.Errorf("JSON id %q, %v", res.ID, err)
	}
}

func TestRecentAnswersAreBounded(t *testing.T) {
	r := newRecentAnswers(2, time.Hour)
	first := r.add(1, "a", answer.Result{}, true)
	r.add(1, "b", answer.Result{}, true)
	r.add(1, "c", answer.Result{}, true)

	if _, ok := r.get(1, first); ok {
		t.Error("the oldest past the limit is kept")
	}

	stale := newRecentAnswers(10, -time.Second)
	if _, ok := stale.get(1, stale.add(1, "a", answer.Result{}, true)); ok {
		t.Error("an expired answer is kept")
	}
}
