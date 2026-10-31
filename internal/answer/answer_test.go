package answer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"web-crawler/internal/llm"
	"web-crawler/internal/retrieval"
)

type fakeSearch struct {
	results []retrieval.Result
	got     retrieval.Request
}

func (f *fakeSearch) Search(_ context.Context, req retrieval.Request) ([]retrieval.Result, error) {
	f.got = req
	return f.results, nil
}

type fakeChat struct {
	reply string
	got   llm.Request
	err   error
}

func (f *fakeChat) Model() string { return "fake" }

func (f *fakeChat) Stream(_ context.Context, req llm.Request, onDelta func(string) error) (llm.Response, error) {
	f.got = req

	for _, word := range strings.SplitAfter(f.reply, " ") {
		if onDelta != nil {
			if err := onDelta(word); err != nil {
				return llm.Response{}, err
			}
		}
	}

	return llm.Response{Text: f.reply, Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}}, f.err
}

func service(t *testing.T, search Searcher, chat llm.Client) *Service {
	t.Helper()

	s, err := New(search, chat, Config{Sources: 3, MaxTokens: 256})
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func TestAnswerGroundsTheModelInNumberedSources(t *testing.T) {
	search := &fakeSearch{results: []retrieval.Result{
		{URL: "https://go.dev/doc/effective_go", Title: "Effective Go", Snippet: "Do not communicate by sharing memory."},
		{URL: "https://go.dev/empty", Title: "Empty", Snippet: "  "},
		{URL: "https://go.dev/ref/spec", Title: "Spec", Snippet: "Channels are typed conduits."},
	}}
	chat := &fakeChat{reply: "Goroutines communicate over channels [2], sharing memory by communicating [1][2]. See also [9]."}

	var (
		streamed []string
		sources  []Source
	)

	res, err := service(t, search, chat).Answer(context.Background(), Request{Query: " how do goroutines talk? ", Host: "go.dev", Mode: retrieval.ModeHybrid}, Events{
		Sources: func(s []Source) error { sources = s; return nil },
		Delta:   func(d string) error { streamed = append(streamed, d); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	if search.got.Limit != 3 || search.got.Host != "go.dev" {
		t.Errorf("search request = %+v", search.got)
	}

	if len(sources) != 2 || sources[0].N != 1 || sources[1].N != 2 || sources[1].URL != "https://go.dev/ref/spec" {
		t.Fatalf("sources = %+v: a result without text is no source, and numbering stays dense", sources)
	}

	prompt := chat.got.Messages[0].Content
	for _, want := range []string{"[1] Effective Go\nhttps://go.dev/doc/effective_go\nDo not communicate", "[2] Spec", "Question: how do goroutines talk?"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}

	if !strings.Contains(chat.got.System, "untrusted") || chat.got.MaxTokens != 256 {
		t.Errorf("system prompt / max tokens = %q / %d", chat.got.System, chat.got.MaxTokens)
	}

	if strings.Join(streamed, "") != chat.reply || res.Answer != chat.reply {
		t.Errorf("streamed %q, answer %q", strings.Join(streamed, ""), res.Answer)
	}

	if !slices.Equal(res.Cited, []int{2, 1}) {
		t.Errorf("cited = %v: in order of use, without the invented [9]", res.Cited)
	}
}

func TestAnswerDoesNotAskTheModelWithoutSources(t *testing.T) {
	chat := &fakeChat{reply: "made up"}

	res, err := service(t, &fakeSearch{}, chat).Answer(context.Background(), Request{Query: "q", Mode: retrieval.ModeHybrid}, Events{})
	if !errors.Is(err, ErrNoSources) || res.Answer != "" || chat.got.System != "" {
		t.Errorf("err %v answer %q: with nothing found the model must not be asked", err, res.Answer)
	}
}

func TestAnswerKeepsWhatWasGeneratedOnFailure(t *testing.T) {
	search := &fakeSearch{results: []retrieval.Result{{URL: "u", Title: "T", Snippet: "text"}}}
	chat := &fakeChat{reply: "partial answer [1]", err: errors.New("stream cut")}

	res, err := service(t, search, chat).Answer(context.Background(), Request{Query: "q", Mode: retrieval.ModeHybrid}, Events{})
	if err == nil || res.Answer != "partial answer [1]" || !slices.Equal(res.Cited, []int{1}) {
		t.Errorf("err %v result %+v", err, res)
	}
}

// scopedSearch answers a search restricted to lab pages from lab, any other
// from shared.
type scopedSearch struct {
	lab, shared []retrieval.Result

	mu       sync.Mutex
	requests []retrieval.Request
}

func (f *scopedSearch) Search(_ context.Context, req retrieval.Request) ([]retrieval.Result, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()

	if req.URLs != nil {
		return f.lab, nil
	}

	return f.shared, nil
}

func reranked(url string, score float64) retrieval.Result {
	return retrieval.Result{URL: url, Title: url, Snippet: "text of " + url, Rerank: &retrieval.Signal{Rank: 1, Score: score}}
}

func labService(t *testing.T, search Searcher, chat llm.Client) *Service {
	t.Helper()

	s, err := New(search, chat, Config{Sources: 3, MaxTokens: 256, MinRelevance: 0, MinLabSources: 2})
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func urlsOf(sources []Source) string {
	var b strings.Builder
	for _, s := range sources {
		b.WriteString(s.URL)
		if s.InLab {
			b.WriteString("(lab)")
		}
		b.WriteString(" ")
	}

	return strings.TrimSpace(b.String())
}

func TestIrrelevantSourcesAreLeftOutWhileRelevantOnesExist(t *testing.T) {
	search := &fakeSearch{results: []retrieval.Result{reranked("a", 2.1), reranked("b", -4), reranked("c", 0.5)}}
	chat := &fakeChat{reply: "x [1]"}

	res, err := labService(t, search, chat).Answer(context.Background(), Request{Query: "q"}, Events{})
	if err != nil || urlsOf(res.Sources) != "a c" || res.Sources[1].N != 2 || res.Scope != ScopeShared || res.Weak {
		t.Errorf("sources %q scope %s weak %v, %v", urlsOf(res.Sources), res.Scope, res.Weak, err)
	}

	if strings.Contains(chat.got.Messages[0].Content, "none of these sources looked clearly relevant") {
		t.Error("relevant sources were called weak")
	}
}

// Search found something, so the model reads it: it can still say the sources
// do not answer, and it is told they may not.
func TestWithNothingRelevantTheBestFoundIsReadAndMarkedWeak(t *testing.T) {
	search := &fakeSearch{results: []retrieval.Result{reranked("a", -0.5), reranked("b", -6)}}
	chat := &fakeChat{reply: "x"}

	res, err := labService(t, search, chat).Answer(context.Background(), Request{Query: "q"}, Events{})
	if err != nil || !res.Weak || urlsOf(res.Sources) != "a b" || res.Scope != ScopeShared {
		t.Fatalf("weak %v, sources %q, scope %s, %v", res.Weak, urlsOf(res.Sources), res.Scope, err)
	}

	if !strings.Contains(chat.got.Messages[0].Content, "none of these sources looked clearly relevant") {
		t.Errorf("the model was not told the sources are weak:\n%s", chat.got.Messages[0].Content)
	}
}

func TestLabsThatHoldEnoughAreNotWidened(t *testing.T) {
	search := &scopedSearch{lab: []retrieval.Result{reranked("l1", 3), reranked("l2", 1)}, shared: []retrieval.Result{reranked("s1", 5)}}

	res, err := labService(t, search, &fakeChat{reply: "x"}).Answer(context.Background(),
		Request{Query: "q", LabURLs: []string{"l1", "l2", "l3"}}, Events{})
	if err != nil || res.Scope != ScopeLabs || urlsOf(res.Sources) != "l1(lab) l2(lab)" {
		t.Errorf("scope %s, sources %q, %v: the model reads the lab only", res.Scope, urlsOf(res.Sources), err)
	}

	if !searchedBoth(search.requests, 3) {
		t.Errorf("searches %+v: the shared base, for the results, and the labs", search.requests)
	}
}

func searchedBoth(requests []retrieval.Request, labURLs int) bool {
	var lab, shared int

	for _, req := range requests {
		switch {
		case req.URLs == nil:
			shared++
		case len(req.URLs) == labURLs:
			lab++
		}
	}

	return len(requests) == 2 && lab == 1 && shared == 1
}

func TestResultsAreListedWithTheSourcesTheyBecame(t *testing.T) {
	search := &scopedSearch{
		lab:    []retrieval.Result{reranked("l1", 2), reranked("l2", 1)},
		shared: []retrieval.Result{reranked("s1", 4), reranked("l2", 1), reranked("s2", 0.5), reranked("s3", -2)},
	}

	var listed []Found

	res, err := labService(t, search, &fakeChat{reply: "x"}).Answer(context.Background(),
		Request{Query: "q", LabURLs: []string{"l1", "l2"}, Results: 3}, Events{Results: func(f []Found) error { listed = f; return nil }})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, f := range listed {
		got = append(got, fmt.Sprintf("%s:%d:%v", f.URL, f.Source, f.InLab))
	}

	if strings.Join(got, " ") != "s1:0:false l2:2:true s2:0:false" || len(res.Results) != 3 {
		t.Errorf("listed %v: the shared ranking, cut to 3, with the source number of what the model read", got)
	}

	for _, req := range search.requests {
		if req.URLs == nil && req.Limit != 3 {
			t.Errorf("the shared search asked for %d results, want 3", req.Limit)
		}
	}
}

func TestThinLabsWidenToTheSharedBase(t *testing.T) {
	search := &scopedSearch{
		lab:    []retrieval.Result{reranked("l1", 2), reranked("l2", -3)},
		shared: []retrieval.Result{reranked("l1", 2), reranked("s1", 1.5), reranked("s2", -1), reranked("s3", 0.2), reranked("s4", 0.1)},
	}

	res, err := labService(t, search, &fakeChat{reply: "x"}).Answer(context.Background(),
		Request{Query: "q", LabURLs: []string{"l1", "l2"}}, Events{})
	if err != nil || res.Scope != ScopeWidened || urlsOf(res.Sources) != "l1(lab) s1 s3" {
		t.Errorf("scope %s, sources %q, %v: lab first, duplicates and irrelevant ones skipped, up to 3", res.Scope, urlsOf(res.Sources), err)
	}

	if !searchedBoth(search.requests, 2) {
		t.Errorf("searches: %+v", search.requests)
	}

	search.lab = nil

	res, err = labService(t, search, &fakeChat{reply: "x"}).Answer(context.Background(),
		Request{Query: "q", LabURLs: []string{}}, Events{})
	if err != nil || res.Scope != ScopeWidened || urlsOf(res.Sources) != "l1 s1 s3" {
		t.Errorf("empty labs: scope %s, sources %q, %v", res.Scope, urlsOf(res.Sources), err)
	}
}

func TestWeakLabsAndSharedBaseAreReadLabsFirst(t *testing.T) {
	search := &scopedSearch{
		lab:    []retrieval.Result{reranked("l1", -3), reranked("l2", -4)},
		shared: []retrieval.Result{reranked("s1", -2), reranked("l1", -3), reranked("s2", -5)},
	}

	res, err := labService(t, search, &fakeChat{reply: "x"}).Answer(context.Background(),
		Request{Query: "q", LabURLs: []string{"l1", "l2"}}, Events{})
	if err != nil || !res.Weak || res.Scope != ScopeWidened || urlsOf(res.Sources) != "l1(lab) l2(lab) s1" {
		t.Errorf("weak %v, scope %s, sources %q, %v", res.Weak, res.Scope, urlsOf(res.Sources), err)
	}
}

func TestOnlyAnEmptySearchGoesUnanswered(t *testing.T) {
	search := &scopedSearch{}
	chat := &fakeChat{reply: "x"}

	res, err := labService(t, search, chat).Answer(context.Background(), Request{Query: "q", LabURLs: []string{"l1"}}, Events{})
	if !errors.Is(err, ErrNoSources) || res.Scope != ScopeNone || len(chat.got.Messages) != 0 || len(search.requests) != 2 {
		t.Errorf("scope %s, err %v, searches %d, model asked %v", res.Scope, err, len(search.requests), len(chat.got.Messages) != 0)
	}
}

func TestFewSourcesAreReadDeeper(t *testing.T) {
	book := reranked("book", 3)
	book.Passages = []string{"book passage two", "book passage three", "book passage four"}
	paper := reranked("paper", 2)
	paper.Passages = []string{"paper passage two"}

	chat := &fakeChat{reply: "x"}
	search := &fakeSearch{results: []retrieval.Result{book, paper}}

	s, err := New(search, chat, Config{Sources: 5, MaxTokens: 256, SourceRunes: 0})
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.Answer(context.Background(), Request{Query: "q"}, Events{})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Join(res.Sources[0].Excerpts, "|") != "book passage two|book passage three" ||
		strings.Join(res.Sources[1].Excerpts, "|") != "paper passage two" {
		t.Errorf("excerpts %q / %q: three spare slots, taking turns", res.Sources[0].Excerpts, res.Sources[1].Excerpts)
	}

	prompt := chat.got.Messages[0].Content
	if !strings.Contains(prompt, "text of book\n…\nbook passage two\n…\nbook passage three\n") || strings.Contains(prompt, "book passage four") {
		t.Errorf("prompt:\n%s", prompt)
	}

	full := []retrieval.Result{book, paper, reranked("c", 1), reranked("d", 1), reranked("e", 1)}
	res, _ = service(t, &fakeSearch{results: full}, &fakeChat{reply: "x"}).Answer(context.Background(), Request{Query: "q"}, Events{})

	for _, src := range res.Sources {
		if len(src.Excerpts) != 0 {
			t.Errorf("with every slot taken by a source, none reads deeper: %+v", src.Excerpts)
		}
	}
}
