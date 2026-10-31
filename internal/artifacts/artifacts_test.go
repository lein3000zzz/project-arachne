package artifacts

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"web-crawler/internal/answer"
	"web-crawler/internal/labs"
	"web-crawler/internal/llm"
	"web-crawler/internal/retrieval"

	"go.uber.org/zap"
)

type memoryStore struct {
	mu        sync.Mutex
	artifacts map[int64]*Artifact
	versions  map[int64][]Version
	saves     int
}

func newMemoryStore() *memoryStore {
	return &memoryStore{artifacts: map[int64]*Artifact{}, versions: map[int64][]Version{}}
}

func (m *memoryStore) Create(_ context.Context, _, labID int64, kind Kind, title, request string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := int64(len(m.artifacts) + 1)
	m.artifacts[id] = &Artifact{ID: id, LabID: labID, Kind: kind, Title: title, Request: request}

	return id, nil
}

func (m *memoryStore) StartVersion(_ context.Context, id int64, v Version) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, old := range m.versions[id] {
		if old.Status == StatusRunning {
			return 0, ErrBuilding
		}
	}

	v.Number = len(m.versions[id]) + 1
	m.versions[id] = append(m.versions[id], v)

	return v.Number, nil
}

func (m *memoryStore) SaveVersion(_ context.Context, id int64, v Version) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.saves++
	v.Sections = slices.Clone(v.Sections)
	m.versions[id][v.Number-1] = v

	return nil
}

func (m *memoryStore) List(context.Context, int64, int64) ([]Artifact, error) { return nil, nil }

func (m *memoryStore) Get(_ context.Context, _, id int64) (Artifact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, ok := m.artifacts[id]
	if !ok {
		return Artifact{}, ErrNotFound
	}

	return *a, nil
}

func (m *memoryStore) Version(_ context.Context, _, id int64, number int) (Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	vs := m.versions[id]
	if number == 0 {
		number = len(vs)
	}

	if number < 1 || number > len(vs) {
		return Version{}, ErrNotFound
	}

	return vs[number-1], nil
}

func (m *memoryStore) Versions(context.Context, int64, int64) ([]int, error) { return nil, nil }
func (m *memoryStore) Delete(context.Context, int64, int64) error            { return nil }
func (m *memoryStore) Interrupt(context.Context) (int64, error)              { return 0, nil }

type fakeLabs struct {
	pages []labs.Page
}

func (f *fakeLabs) Get(_ context.Context, owner, lab int64) (labs.Detail, error) {
	if owner != 1 {
		return labs.Detail{}, labs.ErrNotFound
	}

	return labs.Detail{Lab: labs.Lab{ID: lab, Name: "ResNets", Topic: "deep residual learning"}}, nil
}

func (f *fakeLabs) PageURLs(context.Context, int64, []int64) ([]string, error) {
	return []string{"https://arxiv.org/abs/1512.03385", "https://d2l.ai/resnet.html"}, nil
}

func (f *fakeLabs) Pages(context.Context, int64, int64, int) ([]labs.Page, error) {
	return f.pages, nil
}

// scripted answers each section by its heading: what the lab holds on it,
// what the model writes, how the check goes.
type scripted struct {
	mu       sync.Mutex
	sources  map[string][]answer.Source
	written  map[string]string
	verdicts map[string][]answer.Verdict
	asked    []answer.Request
}

func (f *scripted) heading(req answer.Request) string {
	for h := range f.sources {
		if strings.HasPrefix(req.Query, h+" ") {
			return h
		}
	}

	return ""
}

func (f *scripted) Read(_ context.Context, req answer.Request) (answer.Result, error) {
	f.mu.Lock()
	f.asked = append(f.asked, req)
	f.mu.Unlock()

	srcs := f.sources[f.heading(req)]

	return answer.Result{Sources: srcs, Weak: len(srcs) == 0}, nil
}

func (f *scripted) Write(_ context.Context, req answer.Request, res answer.Result, _ func(string) error) (answer.Result, error) {
	res.Answer = f.written[f.heading(req)]
	res.Cited = nil

	_, claims := answer.Split(res.Answer, len(res.Sources))
	for _, c := range claims {
		res.Cited = append(res.Cited, c.Cites...)
	}

	return res, nil
}

func (f *scripted) Verify(_ context.Context, res answer.Result, _ answer.VerifyEvents) (answer.Verification, error) {
	_, claims := answer.Split(res.Answer, len(res.Sources))

	for h, text := range f.written {
		if text == res.Answer {
			for i := range claims {
				if i < len(f.verdicts[h]) {
					claims[i].Verdict = f.verdicts[h][i]
				}
			}
		}
	}

	return answer.Verification{Claims: claims}, nil
}

func (f *scripted) Verifying() bool { return true }

type model struct{ reply string }

func (m *model) Model() string { return "qwen3:4b-instruct" }

func (m *model) Stream(context.Context, llm.Request, func(string) error) (llm.Response, error) {
	return llm.Response{Text: m.reply}, nil
}

func newService(store Store, answers Answers, chat llm.Client, l *fakeLabs) *Service {
	return NewService(zap.NewNop().Sugar(), store, l, answers, chat,
		Config{MaxSections: 5, SectionTokens: 600, OutlineTokens: 300, Timeout: time.Minute, Verify: true})
}

const paper, book = "https://arxiv.org/abs/1512.03385", "https://d2l.ai/resnet.html"

func TestAReportIsBuiltSectionBySection(t *testing.T) {
	store := newMemoryStore()
	answers := &scripted{
		sources: map[string][]answer.Source{
			"Degradation": {{N: 1, Result: retrieval.Result{URL: paper, Title: "Deep Residual Learning"}}},
			"Blocks": {
				{N: 1, Result: retrieval.Result{URL: book, Title: "Residual Networks"}},
				{N: 2, Result: retrieval.Result{URL: paper, Title: "Deep Residual Learning"}},
			},
			"Hardware": nil,
			"Opinion":  {{N: 1, Result: retrieval.Result{URL: book, Title: "Residual Networks"}}},
		},
		written: map[string]string{
			"Degradation": "Deeper plain networks train worse [1].",
			"Blocks":      "A block adds its input to its output [1][2]. It was invented in 1990 [2].",
			"Opinion":     "ResNets are the best networks ever made.",
		},
		verdicts: map[string][]answer.Verdict{
			"Degradation": {answer.Supported},
			"Blocks":      {answer.Supported, answer.Unsupported},
		},
	}

	s := newService(store, answers, &model{}, &fakeLabs{})
	outline := Outline{Title: "Residual networks", Sections: []Planned{
		{Heading: "Degradation", Focus: "why deeper plain networks train worse"},
		{Heading: "Blocks", Focus: "what a residual block computes"},
		{Heading: "Hardware", Focus: "GPUs used for training"},
		{Heading: "Opinion"},
	}}

	a, err := s.Build(context.Background(), 1, 7, "a report on residual networks", outline)
	if err != nil {
		t.Fatal(err)
	}

	s.running.Wait()

	v, _ := store.Version(context.Background(), 1, a.ID, 0)
	if v.Status != StatusDone || v.Number != 1 || len(v.Sections) != 4 {
		t.Fatalf("version = %+v", v)
	}

	if !slices.Equal(stateList(v), []State{StateDone, StateDone, StateThin, StateThin}) {
		t.Errorf("states = %v; want a thin section where the lab holds nothing, and where the model cited nothing", stateList(v))
	}

	// The paper is [1] in the first section and stays [1]; the book is new: [2].
	if len(v.Sources) != 2 || v.Sources[0].URL != paper || v.Sources[1].URL != book {
		t.Errorf("sources = %+v", v.Sources)
	}

	if blocks := v.Sections[1]; blocks.Text != "A block adds its input to its output [2][1]. It was invented in 1990 [1]." ||
		!slices.Equal(blocks.Claims[0].Cites, []int{2, 1}) || blocks.Claims[1].Verdict != answer.Unsupported {
		t.Errorf("blocks = %+v", blocks)
	}

	if v.Sections[3].Text != "" || v.Sections[3].Note == "" {
		t.Errorf("text citing nothing must stay out of a report: %+v", v.Sections[3])
	}

	if !strings.Contains(v.Markdown, "## Blocks") || !strings.Contains(v.Markdown, "- It was invented in 1990. (unsupported)") {
		t.Errorf("markdown:\n%s", v.Markdown)
	}

	for _, req := range answers.asked {
		if !req.LabOnly || req.Writing == nil || !strings.Contains(req.Writing.Task, "Report: Residual networks") {
			t.Errorf("a section reads its lab alone, as a section: %+v", req)
		}
	}

	if store.saves < 4 {
		t.Errorf("progress saved %d times; the page shows it section by section", store.saves)
	}

	if _, err := s.Rebuild(context.Background(), 1, a.ID); err != nil {
		t.Fatal(err)
	}

	s.running.Wait()

	if v2, _ := store.Version(context.Background(), 1, a.ID, 0); v2.Number != 2 || v2.Status != StatusDone {
		t.Errorf("rebuild = %+v", v2)
	}
}

func stateList(v Version) []State {
	var out []State
	for _, s := range v.Sections {
		out = append(out, s.State)
	}

	return out
}

func TestAnOutlineIsCheckedBeforeAnythingIsBuilt(t *testing.T) {
	s := newService(newMemoryStore(), &scripted{}, &model{}, &fakeLabs{})

	for _, o := range []Outline{
		{Title: "", Sections: []Planned{{Heading: "A"}}},
		{Title: "T"},
		{Title: "T", Sections: make([]Planned, 6)},
		{Title: "T", Sections: []Planned{{Heading: ""}}},
		{Title: "T", Sections: []Planned{{Heading: strings.Repeat("h", 121)}}},
	} {
		if _, err := s.Build(context.Background(), 1, 7, "", o); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", o, err)
		}
	}

	if _, err := s.Build(context.Background(), 2, 7, "", Outline{Title: "T", Sections: []Planned{{Heading: "A"}}}); !errors.Is(err, labs.ErrNotFound) {
		t.Errorf("another user's lab: %v", err)
	}

	if _, err := NewService(zap.NewNop().Sugar(), newMemoryStore(), &fakeLabs{}, nil, nil, Config{MaxSections: 5}).
		Build(context.Background(), 1, 7, "", Outline{Title: "T", Sections: []Planned{{Heading: "A"}}}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("without a model: %v", err)
	}
}

func TestDraftSeesWhatTheLabHolds(t *testing.T) {
	chat := &recordingModel{reply: "```json\n" + `{"title": "Residual networks", "sections": [
		{"heading": "Degradation", "focus": "deeper plain networks train worse"},
		{"heading": "Blocks", "focus": ""},
		{"heading": "", "focus": "dropped"},
		{"heading": "4"}, {"heading": "5"}, {"heading": "6"}, {"heading": "7"}]}` + "\n```"}
	l := &fakeLabs{pages: []labs.Page{
		{Title: "Deep Residual Learning"},
		{Title: "Buy GPUs cheap", Review: &labs.PageReview{Decision: "skip"}},
	}}

	o, err := newService(newMemoryStore(), &scripted{}, chat, l).Draft(context.Background(), 1, 7, "a report on residual networks")
	if err != nil {
		t.Fatal(err)
	}

	if o.Title != "Residual networks" || len(o.Sections) != 5 || o.Sections[1].Focus != "Blocks" || o.Model == "" {
		t.Errorf("outline = %+v; want empty headings dropped, a missing focus the heading, at most 5", o)
	}

	prompt := chat.got.Messages[0].Content
	if !strings.Contains(prompt, "- Deep Residual Learning") || strings.Contains(prompt, "Buy GPUs") || !chat.got.JSON {
		t.Errorf("prompt:\n%s", prompt)
	}

	chat.reply = "I cannot do that."

	o, _ = newService(newMemoryStore(), &scripted{}, chat, l).Draft(context.Background(), 1, 7, "residual networks")
	if o.Title != "residual networks" || len(o.Sections) != 1 || o.Sections[0].Heading != "residual networks" {
		t.Errorf("with nothing usable, the request is the outline: %+v", o)
	}
}

type recordingModel struct {
	reply string
	got   llm.Request
}

func (m *recordingModel) Model() string { return "qwen3:4b-instruct" }

func (m *recordingModel) Stream(_ context.Context, req llm.Request, _ func(string) error) (llm.Response, error) {
	m.got = req
	return llm.Response{Text: m.reply}, nil
}

func TestASavedAnswerKeepsItsCheck(t *testing.T) {
	store := newMemoryStore()
	s := newService(store, &scripted{}, &model{}, &fakeLabs{})

	a, err := s.SaveAnswer(context.Background(), 1, 7, SavedAnswer{
		Question: "What problem do residual connections solve?",
		Text:     "The degradation problem [1].",
		Sources:  []answer.Source{{N: 1, Result: retrieval.Result{URL: paper, Title: "Deep Residual Learning"}}},
		Claims:   []answer.Claim{{Text: "The degradation problem.", Cites: []int{1}, Verdict: answer.Supported}},
		Model:    "qwen3:4b-instruct",
	})
	if err != nil || a.Kind != KindAnswer {
		t.Fatalf("save: %+v, %v", a, err)
	}

	v, _ := store.Version(context.Background(), 1, a.ID, 0)
	if v.Status != StatusDone || v.Sections[0].Claims[0].Verdict != answer.Supported || v.Sources[0].URL != paper ||
		!strings.Contains(v.Markdown, "The degradation problem [1].") {
		t.Errorf("version = %+v", v)
	}

	if _, err := s.SaveAnswer(context.Background(), 1, 7, SavedAnswer{Question: "q"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("an empty answer: %v", err)
	}
}

func TestASectionIsRewrittenOrAddedAloneAndSourcesStayTidy(t *testing.T) {
	store := newMemoryStore()
	answers := &scripted{
		sources: map[string][]answer.Source{
			"A": {{N: 1, Result: retrieval.Result{URL: paper, Title: "Paper"}}},
			"B": {{N: 1, Result: retrieval.Result{URL: book, Title: "Book"}}, {N: 2, Result: retrieval.Result{URL: paper, Title: "Paper"}}},
			"C": nil,
		},
		written: map[string]string{
			"A": "A cites the paper [1].",
			"B": "B cites the book [1] and the paper [2].",
		},
		verdicts: map[string][]answer.Verdict{"B": {answer.Supported}},
	}

	s := newService(store, answers, &model{}, &fakeLabs{})
	ctx := context.Background()

	a, err := s.Build(ctx, 1, 7, "", Outline{Title: "T", Sections: []Planned{{Heading: "A"}, {Heading: "B"}, {Heading: "C"}}})
	if err != nil {
		t.Fatal(err)
	}

	s.running.Wait()

	// The lab has gathered since: A now rests on the book alone, C on the paper.
	answers.sources["A"] = []answer.Source{{N: 1, Result: retrieval.Result{URL: book, Title: "Book"}}}
	answers.written["A"] = "A now cites the book [1]."
	answers.sources["C"] = []answer.Source{{N: 1, Result: retrieval.Result{URL: paper, Title: "Paper"}}}
	answers.written["C"] = "C cites the paper [1]."

	asked := len(answers.asked)

	if _, err := s.Rewrite(ctx, 1, a.ID, 1); err != nil {
		t.Fatal(err)
	}

	s.running.Wait()

	v, _ := store.Version(ctx, 1, a.ID, 0)
	if len(answers.asked) != asked+1 || v.Number != 2 || v.Sections[2].State != StateThin {
		t.Fatalf("rewriting A asked %d times, version %+v; want A alone written", len(answers.asked)-asked, v)
	}

	// The book is now first cited, in A: [1]. The paper, cited only by B now, is [2].
	if v.Sections[0].Text != "A now cites the book [1]." || v.Sections[1].Text != "B cites the book [1] and the paper [2]." ||
		len(v.Sources) != 2 || v.Sources[0].URL != book || v.Sources[1].URL != paper || !slices.Equal(v.Sections[1].Claims[0].Cites, []int{1, 2}) {
		t.Errorf("renumbered: %q, %q, sources %+v, claims %+v", v.Sections[0].Text, v.Sections[1].Text, v.Sources, v.Sections[1].Claims)
	}

	if _, err := s.Rewrite(ctx, 1, a.ID, 3); err != nil {
		t.Fatal(err)
	}

	s.running.Wait()

	if v, _ = store.Version(ctx, 1, a.ID, 0); v.Sections[2].State != StateDone || v.Sections[2].Text != "C cites the paper [2]." {
		t.Errorf("C after gathering: %+v", v.Sections[2])
	}

	answers.sources["D"] = []answer.Source{{N: 1, Result: retrieval.Result{URL: "https://example.com/d", Title: "D"}}}
	answers.written["D"] = "D cites something new [1]."
	asked = len(answers.asked)

	if _, err := s.Extend(ctx, 1, a.ID, Planned{Heading: "D", Focus: "something new"}); err != nil {
		t.Fatal(err)
	}

	s.running.Wait()

	v, _ = store.Version(ctx, 1, a.ID, 0)
	if len(answers.asked) != asked+1 || len(v.Sections) != 4 || len(v.Outline.Sections) != 4 || v.Sections[3].Text != "D cites something new [3]." {
		t.Errorf("extended: %d asked, %+v", len(answers.asked)-asked, v)
	}

	if _, err := s.Extend(ctx, 1, a.ID, Planned{Heading: "E"}); err != nil {
		t.Fatal(err)
	}

	s.running.Wait()

	if _, err := s.Extend(ctx, 1, a.ID, Planned{Heading: "F"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("past max_sections: %v", err)
	}

	for _, n := range []int{0, 6} {
		if _, err := s.Rewrite(ctx, 1, a.ID, n); !errors.Is(err, ErrInvalid) {
			t.Errorf("rewrite section %d: %v", n, err)
		}
	}

	saved, _ := s.SaveAnswer(ctx, 1, 7, SavedAnswer{Question: "q", Text: "An answer [1].", Sources: answers.sources["A"]})
	if _, err := s.Rewrite(ctx, 1, saved.ID, 1); !errors.Is(err, ErrInvalid) {
		t.Errorf("rewriting a saved answer: %v", err)
	}
}

func TestDraftSectionKnowsTheReport(t *testing.T) {
	store := newMemoryStore()
	id, _ := store.Create(context.Background(), 1, 7, KindReport, "Residual networks", "")
	_, _ = store.StartVersion(context.Background(), id, Version{Status: StatusDone, Outline: Outline{Title: "Residual networks"},
		Sections: []Section{{Heading: "Degradation"}, {Heading: "Blocks"}}})

	chat := &recordingModel{reply: `{"heading": "Bottleneck blocks", "focus": "the 1x1, 3x3, 1x1 bottleneck design of deeper ResNets"}`}

	p, err := newService(store, &scripted{}, chat, &fakeLabs{}).DraftSection(context.Background(), 1, id, "the bottleneck design")
	if err != nil || p.Heading != "Bottleneck blocks" || !strings.Contains(p.Focus, "1x1") {
		t.Fatalf("section = %+v, %v", p, err)
	}

	if prompt := chat.got.Messages[0].Content; !strings.Contains(prompt, "- Degradation\n- Blocks\n") || !strings.Contains(prompt, "Request: the bottleneck design") {
		t.Errorf("prompt:\n%s", prompt)
	}

	chat.reply = "no"
	if p, _ := newService(store, &scripted{}, chat, &fakeLabs{}).DraftSection(context.Background(), 1, id, "the bottleneck design"); p.Heading != "the bottleneck design" {
		t.Errorf("with nothing usable, the request is the section: %+v", p)
	}
}
