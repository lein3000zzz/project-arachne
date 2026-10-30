package gathering

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"web-crawler/internal/labs"
	"web-crawler/internal/llm"
	"web-crawler/internal/websearch"

	"go.uber.org/zap"
)

type memStore struct {
	mu      sync.Mutex
	g       map[int64]*Gathering
	running bool
}

func (m *memStore) Start(_ context.Context, labID int64, request string, engines []string, model string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.running {
		return 0, ErrRunning
	}

	id := int64(len(m.g) + 1)
	m.g[id] = &Gathering{ID: id, LabID: labID, Request: request, Engines: engines, Status: StatusRunning, Model: model, Steps: []Step{}}

	return id, nil
}

func (m *memStore) AddStep(_ context.Context, id int64, step Step) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.g[id].Steps = append(m.g[id].Steps, step)

	return nil
}

func (m *memStore) Finish(_ context.Context, id int64, status, summary, errText string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.g[id].Status, m.g[id].Summary, m.g[id].Error = status, summary, errText

	return nil
}

func (m *memStore) Get(_ context.Context, id int64) (Gathering, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return *m.g[id], nil
}

func (m *memStore) Latest(ctx context.Context, _ int64) (Gathering, error) {
	return m.Get(ctx, int64(len(m.g)))
}

func (m *memStore) Interrupt(context.Context) (int64, error) { return 0, nil }

type fakeLabs struct {
	mu      sync.Mutex
	lab     labs.Detail
	found   []labs.Found
	invalid string
}

func (f *fakeLabs) Get(_ context.Context, owner, labID int64) (labs.Detail, error) {
	if owner != 1 || labID != f.lab.ID {
		return labs.Detail{}, labs.ErrNotFound
	}

	return f.lab, nil
}

func (f *fakeLabs) AddFound(_ context.Context, _, _ int64, found labs.Found) (labs.Source, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if found.URL == f.invalid {
		return labs.Source{}, errors.Join(labs.ErrInvalid, errors.New("robots say no"))
	}

	f.found = append(f.found, found)

	return labs.Source{URL: found.URL}, nil
}

type fakeSearch struct {
	queries []string
	engines []string
}

func (f *fakeSearch) Search(_ context.Context, query string, engines []string) (websearch.Response, error) {
	f.queries, f.engines = append(f.queries, query), engines

	return websearch.Response{
		Results: []websearch.Result{
			{URL: "https://go.dev/blog/pipelines", Title: "Go Concurrency Patterns: Pipelines", Snippet: "pipelines and cancellation", Engines: []string{"brave"}},
			{URL: "https://go.dev/doc/effective_go", Title: "Effective Go", Snippet: "tips", Engines: []string{"bing"}},
			{URL: "https://shop.example.com/go-mug", Title: "Go mug", Snippet: "buy", Engines: []string{"bing"}},
		},
		Failed: map[string]string{"duckduckgo": "CAPTCHA"},
	}, nil
}

func (f *fakeSearch) Engines(context.Context) ([]string, error) {
	return []string{"bing", "brave", "duckduckgo", "wikipedia"}, nil
}

type scriptedChat struct {
	mu       sync.Mutex
	replies  []llm.Response
	err      error
	block    bool
	requests []llm.Request
}

func (c *scriptedChat) Model() string { return "small" }

func (c *scriptedChat) Stream(ctx context.Context, req llm.Request, _ func(string) error) (llm.Response, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()

	if c.block {
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}

	if c.err != nil {
		return llm.Response{}, c.err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	resp := c.replies[0]
	if len(c.replies) > 1 {
		c.replies = c.replies[1:]
	}

	return resp, nil
}

func calls(pairs ...string) llm.Response {
	var resp llm.Response
	for i := 0; i < len(pairs); i += 2 {
		resp.ToolCalls = append(resp.ToolCalls, llm.ToolCall{ID: pairs[i], Name: pairs[i], Arguments: json.RawMessage(pairs[i+1])})
	}

	return resp
}

func newService(chat llm.Client, search Search) (*Service, *memStore, *fakeLabs) {
	store := &memStore{g: map[int64]*Gathering{}}
	lab := &fakeLabs{lab: labs.Detail{
		Lab:     labs.Lab{ID: 7, Name: "Go concurrency", Topic: "goroutines, channels, cancellation", Engines: []string{"brave", "bing"}},
		Sources: []labs.SourceStatus{{Source: labs.Source{URL: "https://go.dev/doc/effective_go"}}},
	}}

	return NewService(zap.NewNop().Sugar(), store, lab, search, chat, Config{
		Searches: 2, Sources: 2, Results: 6, MaxTokens: 200, Timeout: time.Minute, DefaultEngines: []string{"brave", "google cse", "bing"},
	}), store, lab
}

func TestTheModelSearchesAndAddsOnlyWhatItWasShown(t *testing.T) {
	chat := &scriptedChat{replies: []llm.Response{
		calls("web_search", `{"query": "go pipelines cancellation"}`),
		calls("add_source", `{"url": "https://go.dev/blog/pipelines", "reason": "The official post on pipelines."}`,
			"add_source", `{"url": "https://made-up.example/go", "reason": "sounds good"}`,
			"add_source", `{"url": "https://go.dev/doc/effective_go", "reason": "classic"}`),
		calls("done", `{"summary": "Pipelines and cancellation are covered; benchmarks are missing."}`),
	}}
	search := &fakeSearch{}
	s, store, lab := newService(chat, search)

	g, err := s.Start(context.Background(), 1, 7, "  focus on cancellation ")
	if err != nil || g.Status != StatusRunning || g.Request != "focus on cancellation" || g.Model != "small" {
		t.Fatalf("start: %+v, %v", g, err)
	}

	s.running.Wait()

	got, _ := store.Get(context.Background(), g.ID)
	if got.Status != StatusDone || got.Summary != "Pipelines and cancellation are covered; benchmarks are missing." {
		t.Errorf("finished: %+v", got)
	}

	if len(lab.found) != 1 || lab.found[0].URL != "https://go.dev/blog/pipelines" || lab.found[0].Query != "go pipelines cancellation" ||
		lab.found[0].Engines[0] != "brave" || lab.found[0].GatheringID != g.ID || lab.found[0].Reason != "The official post on pipelines." {
		t.Errorf("found: %+v - only a shown result the lab lacks, with where it came from", lab.found)
	}

	var kinds []string
	for _, step := range got.Steps {
		kinds = append(kinds, step.Kind)
	}

	if strings.Join(kinds, ",") != "search,add,refused,done" {
		t.Errorf("steps %v: the made-up URL is refused, the one already there just declined", kinds)
	}

	if search := got.Steps[0]; len(search.Results) != 3 || !search.Results[1].InLab || search.Failed["duckduckgo"] != "CAPTCHA" {
		t.Errorf("search step: %+v", search)
	}

	opening := chat.requests[0].Messages[0].Content
	if !strings.Contains(opening, "REQUEST: focus on cancellation") || !strings.Contains(opening, "- https://go.dev/doc/effective_go") ||
		!strings.Contains(opening, "search 2 times and add up to 2 pages") {
		t.Errorf("opening:\n%s", opening)
	}

	shown := chat.requests[1].Messages[2].Content
	if !strings.Contains(shown, "Effective Go [already in the lab]") || !strings.Contains(shown, "Searches left: 1") {
		t.Errorf("results shown:\n%s", shown)
	}

	if strings.Join(search.engines, ",") != "brave,bing" {
		t.Errorf("searched %v, want the lab's engines", search.engines)
	}
}

func TestBudgetsHold(t *testing.T) {
	chat := &scriptedChat{replies: []llm.Response{
		calls("web_search", `{"query": "a"}`, "web_search", `{"query": "b"}`, "web_search", `{"query": "c"}`),
		calls("add_source", `{"url": "go.dev/blog/pipelines", "reason": "x"}`,
			"add_source", `{"url": "https://shop.example.com/go-mug", "reason": "y"}`),
		calls("add_source", `{"url": "https://go.dev/doc/effective_go", "reason": "z"}`),
		calls("done", `{"summary": "ok"}`),
	}}
	search := &fakeSearch{}
	s, store, lab := newService(chat, search)
	lab.lab.Sources = nil

	g, _ := s.Start(context.Background(), 1, 7, "")
	s.running.Wait()

	if len(search.queries) != 2 || len(lab.found) != 2 {
		t.Errorf("%d searches, %d added: the budget is 2 and 2", len(search.queries), len(lab.found))
	}

	third := chat.requests[1].Messages[4].Content
	if !strings.Contains(third, "No searches left") {
		t.Errorf("the third search was answered %q", third)
	}

	if got, _ := store.Get(context.Background(), g.ID); got.Status != StatusDone {
		t.Errorf("status %s", got.Status)
	}
}

func TestStartRefusesWhatCannotRun(t *testing.T) {
	s, store, lab := newService(&scriptedChat{}, &fakeSearch{})

	if _, err := s.Start(context.Background(), 2, 7, ""); !errors.Is(err, labs.ErrNotFound) {
		t.Errorf("another user's lab: %v", err)
	}

	lab.lab.Engines = nil
	if _, err := s.Start(context.Background(), 1, 7, ""); !errors.Is(err, ErrNoEngines) {
		t.Errorf("no engines: %v", err)
	}

	lab.lab.Engines = []string{"brave"}
	store.running = true

	if _, err := s.Start(context.Background(), 1, 7, ""); !errors.Is(err, ErrRunning) {
		t.Errorf("one at a time: %v", err)
	}

	off, _, _ := newService(nil, &fakeSearch{})
	if _, err := off.Start(context.Background(), 1, 7, ""); !errors.Is(err, ErrUnavailable) {
		t.Errorf("no model: %v", err)
	}

	if _, err := s.Start(context.Background(), 1, 7, strings.Repeat("x", maxRequestRunes+1)); !errors.Is(err, labs.ErrInvalid) {
		t.Errorf("long request: %v", err)
	}
}

func TestFailuresAreRecorded(t *testing.T) {
	s, store, _ := newService(&scriptedChat{err: errors.New("connection refused")}, &fakeSearch{})

	g, _ := s.Start(context.Background(), 1, 7, "")
	s.running.Wait()

	if got, _ := store.Get(context.Background(), g.ID); got.Status != StatusFailed || got.Error != "the model could not be reached" {
		t.Errorf("model down: %+v", got)
	}

	blocking, store2, _ := newService(&scriptedChat{block: true}, &fakeSearch{})
	g, _ = blocking.Start(context.Background(), 1, 7, "")

	if err := blocking.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got, _ := store2.Get(context.Background(), g.ID); got.Status != StatusFailed || got.Error != "interrupted by a shutdown" {
		t.Errorf("shutdown: %+v", got)
	}
}

func TestEnginesOffersTheDefaultsThatExist(t *testing.T) {
	s, _, _ := newService(&scriptedChat{}, &fakeSearch{})

	available, defaults, err := s.Engines(context.Background())
	if err != nil || len(available) != 4 || strings.Join(defaults, ",") != "brave,bing" {
		t.Errorf("available %v, defaults %v, %v: google cse is not offered here", available, defaults, err)
	}
}

func TestAModelThatStopsCallingIsAskedToChooseOnce(t *testing.T) {
	chat := &scriptedChat{replies: []llm.Response{
		calls("web_search", `{"query": "go pipelines"}`),
		{Text: "Key pages added include: https://go.dev/blog/pipelines and more."},
		{Text: `{"add": [{"n": 1, "reason": "The official post."}, {"n": 9, "reason": "out of range"}, {"n": "2", "reason": "a mug"}],` +
			` "summary": "Pipelines covered; benchmarks missing."}`},
	}}
	s, store, lab := newService(chat, &fakeSearch{})

	g, _ := s.Start(context.Background(), 1, 7, "focus on cancellation")
	s.running.Wait()

	if len(lab.found) != 2 || lab.found[0].URL != "https://go.dev/blog/pipelines" || lab.found[0].Reason != "The official post." ||
		lab.found[1].URL != "https://shop.example.com/go-mug" {
		t.Errorf("found %+v: picks are numbers into what the lab lacks", lab.found)
	}

	choose := chat.requests[2]
	if !choose.JSON || len(choose.Tools) != 0 || !strings.Contains(choose.Messages[0].Content, "REQUEST: focus on cancellation") ||
		strings.Contains(choose.Messages[0].Content, "effective_go") || !strings.Contains(choose.Messages[0].Content, "Choose at most 2.") {
		t.Errorf("choosing request: %+v", choose)
	}

	if got, _ := store.Get(context.Background(), g.ID); got.Summary != "Pipelines covered; benchmarks missing." || got.Status != StatusDone {
		t.Errorf("finished: %+v", got)
	}
}

func TestAModelThatFinishesProperlyIsNotAskedAgain(t *testing.T) {
	chat := &scriptedChat{replies: []llm.Response{
		calls("web_search", `{"query": "go"}`),
		calls("done", `{"summary": "Nothing worth adding."}`),
	}}
	s, _, lab := newService(chat, &fakeSearch{})

	_, _ = s.Start(context.Background(), 1, 7, "")
	s.running.Wait()

	if len(chat.requests) != 2 || len(lab.found) != 0 {
		t.Errorf("%d requests, %d found: done with nothing added is respected", len(chat.requests), len(lab.found))
	}
}
