package weblookup

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
	"web-crawler/internal/llm"
	"web-crawler/internal/runs"
	"web-crawler/internal/websearch"
)

type search struct {
	mu      sync.Mutex
	engines []string
	results []websearch.Result
	asked   []string
}

func (s *search) Engines(context.Context) ([]string, error) { return s.engines, nil }

func (s *search) Search(_ context.Context, query string, engines []string) (websearch.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.asked = append(s.asked, query)
	return websearch.Response{Results: s.results, Failed: map[string]string{"duckduckgo": "CAPTCHA"}}, nil
}

// fakeRuns walks each run through its states, one per look.
type fakeRuns struct {
	mu      sync.Mutex
	steps   []runs.Status
	seen    map[string]int
	stuck   bool
	refuse  string
	started []string
}

func (f *fakeRuns) Submit(_ context.Context, req runs.Request, _ runs.Origin) (runs.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if req.URL == f.refuse {
		return runs.Run{}, errors.New("excluded")
	}

	if req.Depth != 1 || req.Links != 1 {
		return runs.Run{}, errors.New("a look-up reads one page")
	}

	f.started = append(f.started, req.URL)

	return runs.Run{ID: "run-" + req.URL, Status: runs.StatusQueued}, nil
}

func (f *fakeRuns) Get(_ context.Context, id string) (runs.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.stuck {
		return runs.Run{ID: id, Status: runs.StatusRunning}, nil
	}

	i := min(f.seen[id], len(f.steps)-1)
	f.seen[id]++

	r := runs.Run{ID: id, Status: f.steps[i]}
	if r.Status == runs.StatusFinished {
		r.DocsIndexed = 1
	}

	return r, nil
}

type pages struct {
	fresh []string
}

func (p *pages) Fresh(_ context.Context, urls []string, _ time.Duration) ([]string, error) {
	return slices.DeleteFunc(slices.Clone(urls), func(u string) bool { return !slices.Contains(p.fresh, u) }), nil
}

func (p *pages) OfRun(_ context.Context, id string) ([]string, error) {
	return []string{id[len("run-"):]}, nil
}

type picker struct{ reply string }

func (p *picker) Model() string { return "m" }

func (p *picker) Stream(context.Context, llm.Request, func(string) error) (llm.Response, error) {
	return llm.Response{Text: p.reply}, nil
}

func results() []websearch.Result {
	return []websearch.Result{
		{URL: "https://ziglang.org/learn/overview/", Title: "Zig overview"},
		{URL: "https://ziglang.org/learn/overview/#comptime", Title: "the same page"},
		{URL: "ftp://files.example/zig.txt", Title: "not fetchable"},
		{URL: "https://doc.rust-lang.org/book/ch04-01.html", Title: "Ownership"},
		{URL: "https://dictionary.example/zig", Title: "zig: definition"},
		{URL: "https://bench.example/zig-vs-rust", Title: "Benchmarks"},
		{URL: "https://shop.example/rust-mug", Title: "Rust mug"},
	}
}

func newService(s *search, r *fakeRuns, p *pages, reply string, timeout time.Duration) *Service {
	return New(s, r, p, &picker{reply: reply}, Config{Engines: []string{"bing", "brave", "duckduckgo"}, Results: 5, Read: 3,
		FreshFor: time.Hour, Timeout: timeout, ChooseTokens: 100, Poll: time.Millisecond})
}

func TestTheModelsPicksAreReadIntoTheIndex(t *testing.T) {
	s := &search{engines: []string{"bing", "duckduckgo"}, results: results()}
	r := &fakeRuns{steps: []runs.Status{runs.StatusRunning, runs.StatusIndexing, runs.StatusFinished}, seen: map[string]int{}}
	p := &pages{fresh: []string{"https://ziglang.org/learn/overview/"}}

	var (
		found   []Found
		updates [][]Reading
	)

	got, err := newService(s, r, p, `{"read": [1, 2, 4, 9, 2]}`, time.Minute).Look(context.Background(), 1, "how do zig and rust compare?", Events{
		Searched: func(_ string, f []Found, failed map[string]string) error {
			found = f
			return nil
		},
		Reading: func(rs []Reading) error {
			updates = append(updates, rs)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Canonical, each once, fetchable, at most Results.
	if len(found) != 5 || found[0].URL != "https://ziglang.org/learn/overview" && found[0].URL != "https://ziglang.org/learn/overview/" ||
		found[1].URL != "https://doc.rust-lang.org/book/ch04-01.html" {
		t.Fatalf("found = %+v", found)
	}

	read := []string{}
	for _, f := range found {
		if f.Read {
			read = append(read, f.URL)
		}
	}

	if len(read) != 3 || !slices.Contains(read, "https://bench.example/zig-vs-rust") || slices.Contains(read, "https://shop.example/rust-mug") {
		t.Errorf("read = %v; the model's picks, 9 and the repeat ignored", read)
	}

	// The fresh page is not fetched again.
	if slices.Contains(r.started, found[0].URL) || len(r.started) != 2 {
		t.Errorf("crawled %v", r.started)
	}

	if len(got) != 3 {
		t.Errorf("pages = %v", got)
	}

	last := updates[len(updates)-1]
	if len(updates) < 3 || last[0].State != "known" || last[1].State != "finished" || last[1].Indexed != 1 {
		t.Errorf("progress = %+v", updates)
	}
}

func TestAPickThatIsNoUseReadsTheFirstResults(t *testing.T) {
	s := &search{engines: []string{"bing"}, results: results()}
	r := &fakeRuns{steps: []runs.Status{runs.StatusFinished}, seen: map[string]int{}, refuse: "https://doc.rust-lang.org/book/ch04-01.html"}

	var found []Found

	got, err := newService(s, r, &pages{}, `{"read": []}`, time.Minute).Look(context.Background(), 1, "zig", Events{
		Searched: func(_ string, f []Found, _ map[string]string) error {
			found = f
			return nil
		},
	})
	if err != nil || !found[0].Read || !found[1].Read || !found[2].Read || found[3].Read {
		t.Fatalf("found %+v, %v", found, err)
	}

	// One crawl could not start: the other two are read.
	if len(got) != 2 {
		t.Errorf("pages = %v", got)
	}
}

func TestOutOfTimeReadsWhatIsThere(t *testing.T) {
	s := &search{engines: []string{"bing"}, results: results()[:1]}
	r := &fakeRuns{stuck: true, seen: map[string]int{}}

	started := time.Now()

	got, err := newService(s, r, &pages{}, `{"read": [1]}`, 50*time.Millisecond).Look(context.Background(), 1, "zig", Events{})
	if err != nil || len(got) != 1 || time.Since(started) > time.Second {
		t.Errorf("pages %v, %v after %s", got, err, time.Since(started))
	}
}

func TestWhatCannotBeLookedUp(t *testing.T) {
	r := &fakeRuns{steps: []runs.Status{runs.StatusFinished}, seen: map[string]int{}}

	if _, err := newService(&search{engines: []string{"yandex"}}, r, &pages{}, "", time.Minute).Look(context.Background(), 1, "q", Events{}); !errors.Is(err, ErrNoEngines) {
		t.Errorf("no engine of ours: %v", err)
	}

	if _, err := newService(&search{engines: []string{"bing"}}, r, &pages{}, "", time.Minute).Look(context.Background(), 1, "q", Events{}); !errors.Is(err, ErrNothingFound) {
		t.Errorf("no results: %v", err)
	}

	if _, err := New(nil, r, &pages{}, nil, Config{}).Look(context.Background(), 1, "q", Events{}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("no search engine: %v", err)
	}

	s := newService(&search{engines: []string{"bing"}, results: results()}, &fakeRuns{stuck: true, seen: map[string]int{}}, &pages{}, "", 200*time.Millisecond)

	done := make(chan error)
	go func() {
		_, err := s.Look(context.Background(), 7, "q", Events{})
		done <- err
	}()

	time.Sleep(20 * time.Millisecond)

	if _, err := s.Look(context.Background(), 7, "q", Events{}); !errors.Is(err, ErrBusy) {
		t.Errorf("a second look-up of one user at once: %v", err)
	}

	if _, err := s.Look(context.Background(), 8, "q", Events{}); err != nil {
		t.Errorf("another user's: %v", err)
	}

	<-done
}
