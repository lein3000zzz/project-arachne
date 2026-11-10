package research

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"web-crawler/internal/labs"
	"web-crawler/internal/llm"
	"web-crawler/internal/openalex"
	"web-crawler/internal/websearch"

	"go.uber.org/zap"
)

// memStore keeps research in memory, deduplicating works as the database does.
type memStore struct {
	mu        sync.Mutex
	runs      map[int64]*Run
	subtopics []Subtopic
	works     []*Work
	links     []Link
	refs      map[int64][]Reference
	startErr  error
	// How reading a site's works goes; read works of other sites are in.
	readAs map[string]string
}

func newMemStore() *memStore {
	return &memStore{runs: map[int64]*Run{}, refs: map[int64][]Reference{}}
}

func (m *memStore) StartRun(_ context.Context, labID int64, request string, budget Budget, focus Focus, model string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.startErr != nil {
		return 0, m.startErr
	}

	id := int64(len(m.runs) + 1)
	m.runs[id] = &Run{ID: id, LabID: labID, Request: request, Budget: budget, Focus: focus, Status: StatusRunning, Model: model, Rounds: []Round{}}

	return id, nil
}

func (m *memStore) SaveRounds(_ context.Context, id int64, rounds []Round) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.runs[id].Rounds = slices.Clone(rounds)

	return nil
}

func (m *memStore) FinishRun(_ context.Context, id int64, status, stopped, errText string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	r := m.runs[id]
	r.Status, r.Stopped, r.Error = status, stopped, errText

	return nil
}

func (m *memStore) Run(_ context.Context, id int64) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return *m.runs[id], nil
}

func (m *memStore) LatestRun(_ context.Context, _ int64) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.runs) == 0 {
		return Run{}, ErrNotFound
	}

	return *m.runs[int64(len(m.runs))], nil
}

func (m *memStore) Interrupt(context.Context) (int64, error) { return 0, nil }

func (m *memStore) Subtopics(context.Context, int64) ([]Subtopic, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return slices.Clone(m.subtopics), nil
}

func (m *memStore) AddSubtopics(_ context.Context, _ int64, subs []Subtopic) ([]Subtopic, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, sub := range subs {
		sub.ID = int64(len(m.subtopics) + 1)
		m.subtopics = append(m.subtopics, sub)
	}

	return slices.Clone(m.subtopics), nil
}

func (m *memStore) Find(_ context.Context, _, runID int64, w Work, f Find) (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	f.Run = runID

	for _, have := range m.works {
		if have.URL == w.URL || (w.DOI != "" && have.DOI == w.DOI) || (w.OpenAlexID != "" && have.OpenAlexID == w.OpenAlexID) {
			if have.Title == "" {
				have.Title = w.Title
			}

			if have.DOI == "" {
				have.DOI = w.DOI
			}

			if have.OpenAlexID == "" {
				have.OpenAlexID = w.OpenAlexID
			}

			if !slices.Contains(have.Finds, f) {
				have.Finds = append(have.Finds, f)
			}

			return have.ID, false, nil
		}
	}

	if w.Verdict == "" {
		w.Verdict = VerdictPending
	}

	w.ID = int64(len(m.works) + 1)
	w.Finds = []Find{f}
	m.works = append(m.works, &w)

	return w.ID, true, nil
}

func (m *memStore) Works(context.Context, int64) ([]Work, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Work, len(m.works))
	for i, w := range m.works {
		out[i] = *w
		out[i].Finds = slices.Clone(w.Finds)

		if w.SourceID != 0 {
			out[i].Read = ReadIn
			if state, ok := m.readAs[siteOf(w.URL)]; ok {
				out[i].Read = state
			}
		}
	}

	return out, nil
}

func (m *memStore) work(id int64) *Work { return m.works[id-1] }

func (m *memStore) Judge(_ context.Context, id int64, verdict, reason string, subtopicID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if w := m.work(id); !w.ByUser {
		w.Verdict, w.Reason = verdict, reason
		if subtopicID != 0 {
			w.SubtopicID = subtopicID
		}
	}

	return nil
}

func (m *memStore) SetVerdict(_ context.Context, _, id int64, verdict string, subtopicID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if id < 1 || int(id) > len(m.works) {
		return ErrWorkNotFound
	}

	w := m.work(id)
	w.Verdict, w.ByUser = verdict, true

	if subtopicID != 0 {
		w.SubtopicID = subtopicID
	}

	return nil
}

func (m *memStore) SetSource(_ context.Context, id, sourceID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.work(id).SourceID = sourceID

	return nil
}

func (m *memStore) Expanded(_ context.Context, id int64, references, citing bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	w := m.work(id)
	w.ReferencesDone, w.CitingDone = w.ReferencesDone || references, w.CitingDone || citing

	return nil
}

func (m *memStore) Link(_ context.Context, _ int64, l Link) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !slices.Contains(m.links, l) {
		m.links = append(m.links, l)
	}

	return nil
}

func (m *memStore) Links(context.Context, int64) ([]Link, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return slices.Clone(m.links), nil
}

func (m *memStore) ReferencesOf(_ context.Context, ids []int64) ([]Reference, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []Reference
	for _, id := range ids {
		out = append(out, m.refs[id]...)
	}

	return out, nil
}

// byURL finds a work as a test names it.
func (m *memStore) byURL(u string) *Work {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, w := range m.works {
		if w.URL == u {
			c := *w
			return &c
		}
	}

	return nil
}

type fakeLabs struct {
	mu     sync.Mutex
	lab    labs.Detail
	read   []string
	limit  int
	getErr error
}

func (f *fakeLabs) Get(context.Context, int64, int64) (labs.Detail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.lab, f.getErr
}

func (f *fakeLabs) AddResearched(_ context.Context, _, _ int64, r labs.Researched) (labs.Source, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.limit > 0 && len(f.read) >= f.limit {
		return labs.Source{}, fmt.Errorf("%w: %w: full", labs.ErrInvalid, labs.ErrFull)
	}

	f.read = append(f.read, r.URL)

	return labs.Source{ID: int64(100 + len(f.read)), URL: r.URL, Kind: labs.KindResearch}, nil
}

type fakeSearch struct {
	mu      sync.Mutex
	results map[string][]websearch.Result
	asked   []string
}

func (f *fakeSearch) Search(_ context.Context, query string, _ []string) (websearch.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.asked = append(f.asked, query)

	return websearch.Response{Results: f.results[query]}, nil
}

type fakeScholar struct {
	search  map[string][]openalex.Work
	citing  map[string][]openalex.Work
	catalog map[string]openalex.Work
	dois    map[string]openalex.Work
}

func (f *fakeScholar) Search(_ context.Context, query string, _ int) ([]openalex.Work, error) {
	return f.search[query], nil
}

func (f *fakeScholar) Citing(_ context.Context, id string, _ int) ([]openalex.Work, error) {
	return f.citing[id], nil
}

func (f *fakeScholar) Works(_ context.Context, ids []string) ([]openalex.Work, error) {
	var out []openalex.Work
	for _, id := range ids {
		if w, ok := f.catalog[id]; ok {
			out = append(out, w)
		}
	}

	return out, nil
}

func (f *fakeScholar) ByDOI(_ context.Context, doi string) (*openalex.Work, error) {
	if w, ok := f.dois[doi]; ok {
		return &w, nil
	}

	return nil, nil
}

// model answers each kind of question research asks: a plan, queries, and
// verdicts - every work kept but those titled as junk.
type model struct {
	mu      sync.Mutex
	plan    string
	queries string
	judged  int
}

var workLine = regexp.MustCompile(`(?m)^(\d+)\. \[[^\]]*\] (.*)$`)

func (m *model) Stream(_ context.Context, req llm.Request, _ func(string) error) (llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch req.System {
	case planPrompt:
		return llm.Response{Text: m.plan}, nil
	case queriesPrompt:
		return llm.Response{Text: m.queries}, nil
	}

	var parts []string

	for _, line := range workLine.FindAllStringSubmatch(req.Messages[0].Content, -1) {
		m.judged++

		verdict := "keep"
		if strings.Contains(line[2], "Junk") {
			verdict = "drop"
		}

		parts = append(parts, fmt.Sprintf(`{"n": %s, "verdict": %q, "subtopic": 1, "reason": "about it"}`, line[1], verdict))
	}

	return llm.Response{Text: `{"works": [` + strings.Join(parts, ",") + `]}`}, nil
}

func (m *model) Model() string { return "small" }

func ptr(n int) *int { return &n }

func testConfig() Config {
	return Config{
		Budget: Budget{Found: 300, Read: 50}, MaxBudget: Budget{Found: 1000, Read: 200}, Rounds: 3, JudgeBatch: 10,
		SearchResults: 10, PaperResults: 25, PerWork: 30, MaxTokens: 500, RoundWait: time.Second, Timeout: 10 * time.Second,
		Poll: 5 * time.Millisecond, Documents: true,
	}
}

func waitDone(t *testing.T, store *memStore, id int64) Run {
	t.Helper()

	for range 500 {
		if r, _ := store.Run(context.Background(), id); r.Status != StatusRunning {
			return r
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("the run did not end")

	return Run{}
}

const resnetPlan = `{"subtopics": [{"name": "Residual blocks", "focus": "what a residual block computes", "queries": ["residual networks"]},
	{"name": "Skip connections", "focus": "shortcut connections in deep networks", "queries": ["skip connections"]}]}`

func TestARunFindsThroughEveryChannelAndReadsWhatItKeeps(t *testing.T) {
	store := newMemStore()
	lab := &fakeLabs{lab: labs.Detail{Lab: labs.Lab{ID: 3, Name: "ResNets", Topic: "deep residual learning", Engines: []string{"brave"}},
		Sources: []labs.SourceStatus{{Source: labs.Source{ID: 7, Kind: labs.KindLink, URL: "https://arxiv.org/abs/1512.03385"}}}}}

	search := &fakeSearch{results: map[string][]websearch.Result{
		"residual networks": {
			{URL: "https://d2l.ai/chapter_convolutional-modern/resnet.html", Title: "ResNet - Dive into Deep Learning", Snippet: "residual blocks"},
			{URL: "https://shop.example.com/deals", Title: "Junk deals on GPUs"},
			{URL: "https://example.com/logo.png", Title: "a picture"},
		},
	}}

	resnet := openalex.Work{ID: "W1", DOI: "10.1109/cvpr.2016.90", Title: "Deep Residual Learning", Year: 2016, Venue: "CVPR",
		PDFURL: "https://arxiv.org/pdf/1512.03385", LandingURL: "https://doi.org/10.1109/cvpr.2016.90", References: []string{"W2", "W3"}}
	scholar := &fakeScholar{
		dois:    map[string]openalex.Work{"10.48550/arxiv.1512.03385": resnet},
		catalog: map[string]openalex.Work{"W1": resnet, "W2": {ID: "W2", Title: "Highway Networks", PDFURL: "https://arxiv.org/pdf/1505.00387"}, "W3": {ID: "W3", Title: "Junk erratum", Type: "erratum"}},
		citing:  map[string][]openalex.Work{"W1": {{ID: "W4", Title: "Densely Connected Networks", LandingURL: "https://openreview.net/densenet"}}},
		search:  map[string][]openalex.Work{"skip connections": {{ID: "W5", Title: "Identity Mappings in Deep Residual Networks", LandingURL: "https://link.example.org/identity"}}},
	}

	chat := &model{plan: resnetPlan, queries: `{"queries": [{"subtopic": 2, "query": "resnet variants"}]}`}
	s := NewService(zap.NewNop().Sugar(), store, lab, search, scholar, chat, testConfig())

	run, err := s.Start(context.Background(), 1, 3, Ask{})
	if err != nil {
		t.Fatal(err)
	}

	run = waitDone(t, store, run.ID)
	if run.Status != StatusDone || run.Stopped != StopSaturated || len(run.Rounds) < 2 || run.Budget != (Budget{Found: 300, Read: 50}) {
		t.Fatalf("run %+v", run)
	}

	seed := store.byURL("https://arxiv.org/abs/1512.03385")
	if seed == nil || seed.OpenAlexID != "W1" || seed.SourceID != 7 || seed.Verdict != VerdictKeep {
		t.Fatalf("the seed kept its source and learned its OpenAlex ID: %+v", seed)
	}

	for u, want := range map[string]string{
		"https://d2l.ai/chapter_convolutional-modern/resnet.html": ChannelSearch,
		"https://arxiv.org/pdf/1505.00387":                        ChannelReference,
		"https://openreview.net/densenet":                         ChannelCiting,
		"https://link.example.org/identity":                       ChannelSearch,
	} {
		w := store.byURL(u)
		if w == nil || w.Finds[0].Channel != want || w.Verdict != VerdictKeep || w.SourceID == 0 {
			t.Errorf("%s: %+v, want found by %s, kept and read", u, w, want)
		}
	}

	if w := store.byURL("https://shop.example.com/deals"); w == nil || w.Verdict != VerdictDrop || w.Reason == "" || w.SourceID != 0 {
		t.Errorf("junk is dropped with a reason, and kept on the map: %+v", w)
	}

	if store.byURL("https://example.com/logo.png") != nil || store.byURL("https://openalex.org/W3") != nil {
		t.Error("a picture and an erratum are not works")
	}

	highway, densenet := store.byURL("https://arxiv.org/pdf/1505.00387"), store.byURL("https://openreview.net/densenet")
	if !slices.Contains(store.links, Link{From: seed.ID, To: highway.ID}) || !slices.Contains(store.links, Link{From: densenet.ID, To: seed.ID}) {
		t.Errorf("citations are the map's lines: %+v", store.links)
	}

	if !slices.Contains(search.asked, "resnet variants") {
		t.Errorf("a later round searches what the model wrote: %v", search.asked)
	}

	m, err := s.Map(context.Background(), 1, 3)
	if err != nil || m.Run == nil || len(m.Subtopics) != 2 || m.Coverage.Kept != 5 || m.Coverage.Dropped != 1 || m.Coverage.Estimate != nil {
		t.Errorf("map: %+v, %v", m.Coverage, err)
	}
}

func TestAPreprintAndItsPublishedVersionAreOneWork(t *testing.T) {
	store := newMemStore()
	lab := &fakeLabs{lab: labs.Detail{Lab: labs.Lab{ID: 3, Name: "ResNets", Engines: []string{"brave"}}}}

	paper := openalex.Work{ID: "W1", DOI: "10.1109/cvpr.2016.90", Title: "Deep Residual Learning", PDFURL: "https://arxiv.org/pdf/1512.03385"}
	search := &fakeSearch{results: map[string][]websearch.Result{"resnet": {{URL: "https://arxiv.org/abs/1512.03385v3", Title: "[1512.03385] Deep Residual Learning"}}}}
	scholar := &fakeScholar{search: map[string][]openalex.Work{"resnet": {paper}}, dois: map[string]openalex.Work{"10.48550/arxiv.1512.03385": paper}}

	chat := &model{plan: `{"subtopics": [{"name": "A", "focus": "a", "queries": ["resnet"]}, {"name": "B", "focus": "b", "queries": []}]}`, queries: `{}`}
	s := NewService(zap.NewNop().Sugar(), store, lab, search, scholar, chat, testConfig())

	run, _ := s.Start(context.Background(), 1, 3, Ask{Found: ptr(10), Read: ptr(0)})
	waitDone(t, store, run.ID)

	works, _ := store.Works(context.Background(), 3)
	if len(works) != 1 || works[0].OpenAlexID != "W1" || len(works[0].Finds) != 1 {
		t.Errorf("works %+v", works)
	}
}

func TestTheBudgetStopsJudgingAndLeavesTheRestPending(t *testing.T) {
	store := newMemStore()
	lab := &fakeLabs{lab: labs.Detail{Lab: labs.Lab{ID: 3, Name: "Go", Engines: []string{"brave"}}}}

	var results []websearch.Result
	for i := range 8 {
		results = append(results, websearch.Result{URL: fmt.Sprintf("https://go.dev/doc/%d", i), Title: fmt.Sprintf("Doc %d", i)})
	}

	chat := &model{plan: `{"subtopics": [{"name": "A", "focus": "a", "queries": ["go"]}, {"name": "B", "focus": "b"}]}`}
	s := NewService(zap.NewNop().Sugar(), store, lab, &fakeSearch{results: map[string][]websearch.Result{"go": results}}, nil, chat, testConfig())

	run, _ := s.Start(context.Background(), 1, 3, Ask{Found: ptr(3), Read: ptr(1)})
	run = waitDone(t, store, run.ID)

	m, _ := s.Map(context.Background(), 1, 3)
	if run.Stopped != StopBudget || m.Coverage.Kept != 3 || m.Coverage.Pending != 5 || m.Coverage.Read != 1 {
		t.Errorf("stopped %q, coverage %+v", run.Stopped, m.Coverage)
	}
}

func TestReadingStopsWhenTheLabIsFull(t *testing.T) {
	store := newMemStore()
	lab := &fakeLabs{limit: 2, lab: labs.Detail{Lab: labs.Lab{ID: 3, Name: "Go", Engines: []string{"brave"}}}}

	var results []websearch.Result
	for i := range 5 {
		results = append(results, websearch.Result{URL: fmt.Sprintf("https://site%d.example/", i), Title: fmt.Sprintf("Page %d", i)})
	}

	chat := &model{plan: `{"subtopics": [{"name": "A", "focus": "a", "queries": ["go"]}, {"name": "B", "focus": "b"}]}`, queries: `{}`}
	s := NewService(zap.NewNop().Sugar(), store, lab, &fakeSearch{results: map[string][]websearch.Result{"go": results}}, nil, chat, testConfig())

	run, _ := s.Start(context.Background(), 1, 3, Ask{})
	run = waitDone(t, store, run.ID)

	if len(lab.read) != 2 || !strings.Contains(run.Rounds[0].Failed["reading"], "full") {
		t.Errorf("read %v, round %+v", lab.read, run.Rounds[0])
	}
}

// A site that keeps crawlers out gets one try, its read is given back, and
// it is not tried again; a site that gave its pages gets more.
func TestASiteThatRefusesIsTriedOnceAndItsReadRefunded(t *testing.T) {
	store := newMemStore()
	store.readAs = map[string]string{"medium.com": ReadBlocked}

	lab := &fakeLabs{lab: labs.Detail{Lab: labs.Lab{ID: 3, Name: "Zig", Engines: []string{"brave"}}}}

	var results []websearch.Result
	for _, u := range []string{"https://a.medium.com/x", "https://b.medium.com/y", "https://medium.com/z",
		"https://go.dev/doc/1", "https://go.dev/doc/2", "https://go.dev/doc/3", "https://go.dev/doc/4"} {
		results = append(results, websearch.Result{URL: u, Title: "Page " + u})
	}

	chat := &model{plan: `{"subtopics": [{"name": "A", "focus": "a", "queries": ["zig"]}, {"name": "B", "focus": "b"}]}`, queries: `{}`}
	s := NewService(zap.NewNop().Sugar(), store, lab, &fakeSearch{results: map[string][]websearch.Result{"zig": results}}, nil, chat, testConfig())

	run, _ := s.Start(context.Background(), 1, 3, Ask{Read: ptr(10)})
	run = waitDone(t, store, run.ID)

	medium := 0
	for _, u := range lab.read {
		if siteOf(u) == "medium.com" {
			medium++
		}
	}

	if medium != 1 || len(lab.read) != 1+maxReadsPerSite {
		t.Errorf("read %v: one try of medium.com, then go.dev up to its share", lab.read)
	}

	if run.Rounds[0].Read != 2 || run.Rounds[0].Unread != 1 {
		t.Errorf("round 1 %+v: one read of each site, the refused one given back", run.Rounds[0])
	}
}

func TestPapersAreReadFromTheirOpenCopyFirst(t *testing.T) {
	cfg := testConfig()
	cfg.Documents = false

	s := NewService(zap.NewNop().Sugar(), nil, nil, nil, nil, nil, cfg)

	w := s.paper(openalex.Work{ID: "W1", DOI: "10.1145/1", Title: "T", LandingURL: "https://dl.acm.org/doi/10.1145/1",
		OALandingURL: "https://arxiv.org/abs/2401.00001", PDFURL: "https://arxiv.org/pdf/2401.00001"})
	if w.URL != "https://arxiv.org/abs/2401.00001" {
		t.Errorf("without PDFs: %s", w.URL)
	}

	cfg.Documents = true
	if w := NewService(zap.NewNop().Sugar(), nil, nil, nil, nil, nil, cfg).paper(openalex.Work{ID: "W1", LandingURL: "https://dl.acm.org/x",
		PDFURL: "https://arxiv.org/pdf/2401.00001"}); w.URL != "https://arxiv.org/pdf/2401.00001" {
		t.Errorf("with PDFs: %s", w.URL)
	}

	if w := s.paper(openalex.Work{ID: "W2", LandingURL: "https://dl.acm.org/x"}); w.URL != "https://dl.acm.org/x" {
		t.Errorf("no open copy: %s", w.URL)
	}

	if got := siteOf("https://levelup.medium.com/a"); got != "medium.com" {
		t.Errorf("siteOf: %s", got)
	}
}

func TestStartingNeedsAModelChannelsAndASaneBudget(t *testing.T) {
	lab := &fakeLabs{lab: labs.Detail{Lab: labs.Lab{ID: 3, Name: "Go"}}}
	cfg := testConfig()

	if _, err := NewService(zap.NewNop().Sugar(), newMemStore(), lab, &fakeSearch{}, nil, nil, cfg).Start(context.Background(), 1, 3, Ask{}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("without a model: %v", err)
	}

	if _, err := NewService(zap.NewNop().Sugar(), newMemStore(), lab, &fakeSearch{}, nil, &model{}, cfg).Start(context.Background(), 1, 3, Ask{}); !errors.Is(err, ErrNoChannels) {
		t.Errorf("no engines, no OpenAlex: %v", err)
	}

	s := NewService(zap.NewNop().Sugar(), newMemStore(), lab, nil, &fakeScholar{}, &model{}, cfg)
	if _, err := s.Start(context.Background(), 1, 3, Ask{Found: ptr(5000)}); !errors.Is(err, labs.ErrInvalid) {
		t.Errorf("past the budget's cap: %v", err)
	}

	busy := newMemStore()
	busy.startErr = ErrRunning

	if _, err := NewService(zap.NewNop().Sugar(), busy, lab, nil, &fakeScholar{}, &model{}, cfg).Start(context.Background(), 1, 3, Ask{}); !errors.Is(err, ErrRunning) {
		t.Errorf("twice: %v", err)
	}

	lab.getErr = labs.ErrNotFound
	if _, err := s.Map(context.Background(), 2, 3); !errors.Is(err, labs.ErrNotFound) {
		t.Errorf("another user's lab: %v", err)
	}
}

func TestAPlanThatIsNoUseKeepsTheTopicWhole(t *testing.T) {
	s := NewService(zap.NewNop().Sugar(), newMemStore(), nil, nil, nil, &model{plan: "I cannot help with that."}, testConfig())

	subs, err := s.plan(context.Background(), labs.Detail{Lab: labs.Lab{Name: "Go", Topic: "concurrency"}}, "")
	if err != nil || len(subs) != 1 || subs[0].Name != "Go" || subs[0].Queries[0] != "Go: concurrency" {
		t.Errorf("subtopics %+v, %v", subs, err)
	}
}

type reply string

func (r reply) Stream(context.Context, llm.Request, func(string) error) (llm.Response, error) {
	return llm.Response{Text: string(r)}, nil
}

func (r reply) Model() string { return "small" }

func TestJudgingTakesOnlyWhatItWasShown(t *testing.T) {
	s := NewService(zap.NewNop().Sugar(), nil, nil, nil, nil,
		reply(`{"works": [{"n": 1, "verdict": "KEEP", "subtopic": 2}, {"n": 1, "verdict": "drop"}, {"n": 9, "verdict": "keep"}, {"n": 2, "verdict": "great"}]}`), testConfig())

	subs := []Subtopic{{ID: 10, Name: "A"}, {ID: 20, Name: "B"}}

	got, err := s.judge(context.Background(), labs.Detail{}, subs, []Work{{ID: 5, Title: "x"}, {ID: 6, Title: "y"}})
	if err != nil || len(got) != 1 || got[0] != (verdict{work: 5, verdict: VerdictKeep, subtopicID: 20}) {
		t.Errorf("verdicts %+v, %v", got, err)
	}
}

func TestCoverageIsEstimatedFromWhatSearchAndCitationsBothFound(t *testing.T) {
	var works []Work

	add := func(n int, finds ...string) {
		for range n {
			w := Work{ID: int64(len(works) + 1), Verdict: VerdictKeep, SubtopicID: 1}
			for _, c := range finds {
				w.Finds = append(w.Finds, Find{Channel: c})
			}

			works = append(works, w)
		}
	}

	add(6, ChannelSearch)
	add(4, ChannelReference)
	add(4, ChannelSearch, ChannelCiting)

	c := coverageOf([]Subtopic{{ID: 1}}, works)

	// Chapman: (10+1)(8+1)/(4+1) - 1 = 18.8.
	e := c.Estimate
	if e == nil || e.BySearch != 10 || e.ByCitations != 8 || e.ByBoth != 4 || e.Found != 14 || e.Total != 19 || e.Share < 0.73 || e.Share > 0.74 {
		t.Errorf("estimate %+v", e)
	}

	if c.Subtopics[0].Kept != 14 || c.Subtopics[0].Estimate == nil {
		t.Errorf("per subtopic %+v", c.Subtopics[0])
	}

	if coverageOf(nil, works[:8]).Estimate != nil {
		t.Error("too few found by citations to say anything")
	}
}

func TestDOIsAreReadFromLinks(t *testing.T) {
	for in, want := range map[string]string{
		"https://doi.org/10.1145/3292500.3330701": "10.1145/3292500.3330701",
		"https://arxiv.org/abs/1512.03385v3":      "10.48550/arxiv.1512.03385",
		"https://arxiv.org/pdf/1512.03385.pdf":    "10.48550/arxiv.1512.03385",
		"https://arxiv.org/list/cs.LG/recent":     "",
		"https://go.dev/":                         "",
	} {
		if got := doiOf(in); got != want {
			t.Errorf("doiOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// steeredLab is a lab already researched once: two subtopics, a kept paper
// in each, both already followed.
func steeredLab() (*memStore, *fakeLabs) {
	store := newMemStore()
	store.subtopics = []Subtopic{{ID: 1, Name: "Blocks", Queries: []string{"residual blocks", "resnet"}}, {ID: 2, Name: "Training"}}
	store.works = []*Work{
		{ID: 1, URL: "https://arxiv.org/abs/1512.03385", Kind: KindPaper, Title: "ResNet", OpenAlexID: "W1", SubtopicID: 1, Verdict: VerdictKeep,
			ReferencesDone: true, CitingDone: true, Finds: []Find{{Channel: ChannelSearch, Query: "resnet"}}},
		{ID: 2, URL: "https://arxiv.org/abs/1603.09382", Kind: KindPaper, Title: "Stochastic Depth", OpenAlexID: "W9", SubtopicID: 2, Verdict: VerdictKeep,
			Finds: []Find{{Channel: ChannelSearch, Query: "resnet"}}},
	}

	return store, &fakeLabs{lab: labs.Detail{Lab: labs.Lab{ID: 3, Name: "ResNets", Engines: []string{"brave"}}}}
}

func TestFollowingOneWorkFollowsItAloneEvenIfDone(t *testing.T) {
	store, lab := steeredLab()
	scholar := &fakeScholar{
		catalog: map[string]openalex.Work{"W1": {ID: "W1", References: []string{"W2"}}, "W2": {ID: "W2", Title: "Highway Networks", LandingURL: "https://example.org/highway"}},
		citing: map[string][]openalex.Work{
			"W1": {{ID: "W4", Title: "DenseNet", LandingURL: "https://densenet.example.net/paper"}},
			"W9": {{ID: "W8", Title: "Not asked for", LandingURL: "https://example.org/other"}},
		},
	}

	s := NewService(zap.NewNop().Sugar(), store, lab, nil, scholar, &model{}, testConfig())

	run, err := s.Start(context.Background(), 1, 3, Ask{Focus: Focus{Work: 1}})
	if err != nil {
		t.Fatal(err)
	}

	run = waitDone(t, store, run.ID)
	if run.Stopped != StopFollowed || len(run.Rounds) != 1 || run.Focus.Work != 1 || len(run.Rounds[0].Queries) != 0 {
		t.Errorf("run %+v", run)
	}

	if store.byURL("https://example.org/highway") == nil || store.byURL("https://densenet.example.net/paper") == nil || store.byURL("https://example.org/other") != nil {
		t.Errorf("found %d works: what the work cites and what cites it, nothing else", len(store.works))
	}

	if !slices.Equal(lab.read, []string{"https://example.org/highway", "https://densenet.example.net/paper"}) &&
		!slices.Equal(lab.read, []string{"https://densenet.example.net/paper", "https://example.org/highway"}) {
		t.Errorf("read %v: only what was found through the work", lab.read)
	}
}

func TestResearchingOneSubtopicStaysOnIt(t *testing.T) {
	store, lab := steeredLab()

	search := &fakeSearch{results: map[string][]websearch.Result{
		"residual blocks": {{URL: "https://d2l.ai/resnet", Title: "ResNet chapter"}},
	}}

	chat := &model{queries: `{"queries": [{"subtopic": 1, "query": "bottleneck blocks"}]}`}
	s := NewService(zap.NewNop().Sugar(), store, lab, search, nil, chat, testConfig())

	run, err := s.Start(context.Background(), 1, 3, Ask{Focus: Focus{Subtopic: 1}})
	if err != nil {
		t.Fatal(err)
	}

	waitDone(t, store, run.ID)

	if len(search.asked) < 2 || search.asked[0] != "residual blocks" || slices.Contains(search.asked, "resnet") || !slices.Contains(search.asked, "bottleneck blocks") {
		t.Errorf("searched %v: the subtopic's own untried query first, then the model's", search.asked)
	}

	if slices.Contains(lab.read, "https://arxiv.org/abs/1603.09382") || !slices.Contains(lab.read, "https://d2l.ai/resnet") {
		t.Errorf("read %v: the subtopic's works alone", lab.read)
	}
}

func TestTheUsersVerdictStandsAndAWorkCanBeReadByHand(t *testing.T) {
	store, lab := steeredLab()
	s := NewService(zap.NewNop().Sugar(), store, lab, nil, &fakeScholar{}, &model{}, testConfig())
	ctx := context.Background()

	w, err := s.SetVerdict(ctx, 1, 3, 2, VerdictDrop, 1)
	if err != nil || w.Verdict != VerdictDrop || !w.ByUser || w.SubtopicID != 1 {
		t.Fatalf("set: %+v, %v", w, err)
	}

	if err := store.Judge(ctx, 2, VerdictKeep, "the model's", 2); err != nil || store.work(2).Verdict != VerdictDrop {
		t.Error("the model overruled the user")
	}

	for _, c := range []struct {
		verdict  string
		work     int64
		subtopic int64
		want     error
	}{{"great", 2, 0, labs.ErrInvalid}, {VerdictKeep, 99, 0, ErrWorkNotFound}, {VerdictKeep, 2, 77, ErrSubtopicNotFound}} {
		if _, err := s.SetVerdict(ctx, 1, 3, c.work, c.verdict, c.subtopic); !errors.Is(err, c.want) {
			t.Errorf("%+v: %v", c, err)
		}
	}

	w, err = s.ReadWork(ctx, 1, 3, 2)
	if err != nil || w.SourceID == 0 || w.Verdict != VerdictKeep || !slices.Equal(lab.read, []string{"https://arxiv.org/abs/1603.09382"}) {
		t.Errorf("read by hand: %+v, %v, read %v", w, err, lab.read)
	}

	if _, err := s.ReadWork(ctx, 1, 3, 2); err != nil || len(lab.read) != 1 {
		t.Errorf("read twice: %v, read %v", err, lab.read)
	}
}

func TestAFocusMustMakeSense(t *testing.T) {
	store, lab := steeredLab()
	store.works = append(store.works, &Work{ID: 3, URL: "https://example.com/blog", Kind: KindWeb, Verdict: VerdictKeep})

	s := NewService(zap.NewNop().Sugar(), store, lab, nil, &fakeScholar{}, &model{}, testConfig())

	for focus, want := range map[Focus]error{
		{Work: 1, Subtopic: 1}: labs.ErrInvalid,
		{Work: 42}:             ErrWorkNotFound,
		{Work: 3}:              ErrNothingToFollow,
		{Subtopic: 9}:          ErrSubtopicNotFound,
	} {
		if _, err := s.Start(context.Background(), 1, 3, Ask{Focus: focus}); !errors.Is(err, want) {
			t.Errorf("%+v: %v, want %v", focus, err, want)
		}
	}
}
