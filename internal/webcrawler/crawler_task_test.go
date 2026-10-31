package webcrawler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"web-crawler/internal/appconfig"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/domain/data"
	"web-crawler/internal/networker"
	"web-crawler/internal/parser"
	"web-crawler/internal/processor"
	"web-crawler/internal/processor/queue"
	"web-crawler/internal/webcrawler/runstates"

	"go.uber.org/zap"
)

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.events = append(l.events, e)
}

func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return slices.Clone(l.events)
}

type pageNetworker struct {
	nopNetworker
	err error
}

func (p pageNetworker) Fetch(url string) (*networker.FetchResult, error) {
	if p.err != nil && !strings.HasSuffix(url, "/robots.txt") {
		return nil, p.err
	}

	if strings.HasSuffix(url, "/robots.txt") {
		return &networker.FetchResult{Status: http.StatusNotFound}, nil
	}

	return &networker.FetchResult{Status: http.StatusOK, Body: []byte("<html></html>"), ContentType: "text/html"}, nil
}

type linkParser struct{ links []string }

func (l linkParser) Parse(context.Context, *parser.ParseParams) (*parser.ParseResult, error) {
	res := &parser.ParseResult{Title: "Page", Content: "Enough text to be a document."}
	for _, link := range l.links {
		res.Links = append(res.Links, parser.Link{URL: link, Kind: parser.LinkPage})
	}

	return res, nil
}

type fakeRunState struct {
	runstates.RunStateManager
	log      *eventLog
	state    runstates.TaskState
	left     int64
	counted  bool
	complErr error
}

func (f *fakeRunState) TaskState(context.Context, string, string, time.Duration) (runstates.TaskState, error) {
	return f.state, nil
}

func (f *fakeRunState) CompleteTask(context.Context, string, string, time.Duration) (int64, bool, error) {
	f.log.add("complete")
	return f.left, f.counted, f.complErr
}

func (f *fakeRunState) ReleaseRunSlot(context.Context, string) error {
	f.log.add("release")
	return nil
}

func (f *fakeRunState) CleanupRun(context.Context, string) error {
	f.log.add("cleanup")
	return nil
}

type fakeSender struct {
	log  *eventLog
	errs map[string]error
}

func (f *fakeSender) SendTask(task *config.Task) error {
	f.log.add("send " + task.URL)
	return f.errs[task.URL]
}

type taskHarness struct {
	repo  *CrawlerRepo
	state *fakeRunState
	log   *eventLog
	task  *config.Task
}

func newHarness(links ...string) *taskHarness {
	log := &eventLog{}
	state := &fakeRunState{log: log, state: runstates.TaskState{RunActive: true}, left: 5, counted: true}

	repo := (&CrawlerRepo{
		logger:          zap.NewNop().Sugar(),
		parser:          linkParser{links: links},
		networker:       pageNetworker{},
		extraWorker:     nopExtra{},
		cachePages:      nopCache{},
		cacheRobots:     nopCache{},
		runStateManager: state,
		documents:       &recordingSink{},
		settings:        appconfig.Default(),
		cfg:             &CrawlerConfig{TaskSender: &fakeSender{log: log}},
	}).withRobots()

	task := &config.Task{URL: "https://example.com/", Run: &config.Run{ID: "run", MaxDepth: 3, MaxLinks: 100}}
	task.SetAck(func() { log.add("ack") })

	return &taskHarness{repo: repo, state: state, log: log, task: task}
}

func (h *taskHarness) sender() *fakeSender {
	return h.repo.cfg.TaskSender.(*fakeSender)
}

func (h *taskHarness) run() []string {
	h.repo.handleTask(h.task, make(chan *data.PageData, 1))
	return h.log.all()
}

func TestChildrenAreQueuedBeforeTheTaskCompletes(t *testing.T) {
	h := newHarness("https://example.com/a", "https://example.com/b")

	want := []string{"send https://example.com/a", "send https://example.com/b", "complete", "ack"}
	if got := h.run(); !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

func TestOnlyTheCompletionThatReachesZeroReleasesTheSlot(t *testing.T) {
	cases := []struct {
		name        string
		left        int64
		counted     bool
		wantRelease bool
	}{
		{"last task of the run", 0, true, true},
		{"tasks still running", 3, true, false},
		{"redelivered duplicate", 0, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			h.state.left, h.state.counted = tc.left, tc.counted
			h.repo.runs = &fakeRecorder{log: h.log}

			got := h.run()
			released := slices.Contains(got, "release") && slices.Contains(got, "cleanup")

			if released != tc.wantRelease {
				t.Errorf("events = %v, want release=%v", got, tc.wantRelease)
			}

			if recorded := slices.Contains(got, "run finished"); recorded != tc.wantRelease {
				t.Errorf("events = %v: the run is recorded as finished exactly when its slot is released", got)
			}

			if !slices.Contains(got, "ack") {
				t.Error("a completed task must be acked")
			}
		})
	}
}

func TestDoneOrInactiveTasksAreSkippedAndAcked(t *testing.T) {
	for name, state := range map[string]runstates.TaskState{
		"already done": {RunActive: true, Done: true},
		"run inactive": {RunActive: false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness("https://example.com/a")
			h.state.state = state

			if got := h.run(); !slices.Equal(got, []string{"ack"}) {
				t.Errorf("events = %v: the task must be acked without crawling, sending or counting", got)
			}
		})
	}
}

func TestUnsentChildrenLeaveTheTaskForRedelivery(t *testing.T) {
	h := newHarness("https://example.com/a", "https://example.com/b")
	h.sender().errs = map[string]error{"https://example.com/b": queue.ErrQueueClosed}

	got := h.run()
	if slices.Contains(got, "complete") || slices.Contains(got, "ack") {
		t.Errorf("events = %v: with a child lost, completing or acking would lose its whole subtree", got)
	}
}

func TestRefusedChildrenDoNotBlockCompletion(t *testing.T) {
	cases := map[string]struct {
		err       error
		wantSends int
	}{
		"over the link limit":  {processor.ErrRunLimitExceeded, 3},
		"run no longer active": {processor.ErrRunNotActive, 1},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			links := []string{"https://example.com/a", "https://example.com/b", "https://example.com/c"}
			h := newHarness(links...)
			h.sender().errs = map[string]error{}
			for _, l := range links {
				h.sender().errs[l] = tc.err
			}

			got := h.run()

			sends := 0
			for _, e := range got {
				if strings.HasPrefix(e, "send ") {
					sends++
				}
			}

			if sends != tc.wantSends {
				t.Errorf("sent %d children, want %d: %v", sends, tc.wantSends, got)
			}

			if !slices.Contains(got, "complete") || !slices.Contains(got, "ack") {
				t.Errorf("events = %v: a deliberately refused child is not a lost one", got)
			}
		})
	}
}

func TestFailedCompletionIsNotAcked(t *testing.T) {
	h := newHarness()
	h.state.complErr = errors.New("redis down")

	got := h.run()
	if slices.Contains(got, "ack") || slices.Contains(got, "release") {
		t.Errorf("events = %v: an uncounted task must stay unacked for redelivery", got)
	}
}

func TestMaxDepthSendsNoChildren(t *testing.T) {
	h := newHarness("https://example.com/a")
	h.task.CurrentDepth = 2

	for _, e := range h.run() {
		if strings.HasPrefix(e, "send ") {
			t.Fatalf("a task at depth %d of %d must not queue children", h.task.CurrentDepth, h.task.Run.MaxDepth)
		}
	}
}

func TestFailedFetchStillCompletes(t *testing.T) {
	h := newHarness("https://example.com/a")
	h.repo.networker = pageNetworker{err: fmt.Errorf("connection refused")}

	if got := h.run(); !slices.Equal(got, []string{"complete", "ack"}) {
		t.Errorf("events = %v: an unreachable page is finished, not retried forever", got)
	}
}

func TestChildrenOutsideTheScopeAreNotFollowed(t *testing.T) {
	h := newHarness("https://example.com/docs/a", "https://example.com/blog/b", "https://other.example/docs/c")
	h.task.URL = "https://example.com/docs/"
	h.task.Run.Scope = "https://example.com/docs/"

	want := []string{"send https://example.com/docs/a", "complete", "ack"}
	if got := h.run(); !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

type fakeRecorder struct {
	log *eventLog
}

func (f *fakeRecorder) Finished(context.Context, *config.Run, string) error {
	f.log.add("run finished")
	return nil
}

func TestExcludedLinksAreNotFollowed(t *testing.T) {
	h := newHarness(
		"https://ru.wikipedia.org/wiki/%D0%92%D0%B8%D0%BA%D0%B8%D0%BF%D0%B5%D0%B4%D0%B8%D1%8F:%D0%A1%D0%BE%D0%BE%D0%B1%D1%89%D0%B5%D0%BD%D0%B8%D1%8F_%D0%BE%D0%B1_%D0%BE%D1%88%D0%B8%D0%B1%D0%BA%D0%B0%D1%85",
		"https://ru.wikipedia.org/wiki/%D0%9E%D0%B1%D1%81%D1%83%D0%B6%D0%B4%D0%B5%D0%BD%D0%B8%D0%B5_%D1%83%D1%87%D0%B0%D1%81%D1%82%D0%BD%D0%B8%D0%BA%D0%B0:X",
		"https://en.wikipedia.org/wiki/Special:Random",
		"https://ru.wikipedia.org/w/index.php?title=Go&action=edit",
		"https://ru.wikipedia.org/wiki/Go",
		"https://en.wikipedia.org/wiki/Star_Wars:_Episode_IV",
		"https://en.wikipedia.org/wiki/Go_(programming_language)",
		"https://pkg.go.dev/golang.org/x/sync@v0.24.0/errgroup",
		"https://pkg.go.dev/golang.org/x/sync/errgroup?tab=versions",
		"https://pkg.go.dev/golang.org/x/sync/errgroup?tab=importedby",
		"https://pkg.go.dev/golang.org/x/sync/errgroup",
		"https://pkg.go.dev/golang.org/x/sync/errgroup?tab=doc",
	)
	h.repo.exclude = compileExclude(appconfig.DefaultExclude)

	var sent []string
	for _, e := range h.run() {
		if strings.HasPrefix(e, "send ") {
			sent = append(sent, strings.TrimPrefix(e, "send "))
		}
	}

	want := []string{
		"https://ru.wikipedia.org/wiki/Go",
		"https://en.wikipedia.org/wiki/Star_Wars:_Episode_IV",
		"https://en.wikipedia.org/wiki/Go_(programming_language)",
		"https://pkg.go.dev/golang.org/x/sync@v0.24.0/errgroup",
		"https://pkg.go.dev/golang.org/x/sync/errgroup",
		"https://pkg.go.dev/golang.org/x/sync/errgroup?tab=doc",
	}

	if !slices.Equal(sent, want) {
		t.Errorf("followed %v, want %v: namespace, special and edit pages are excluded, articles with a colon are not; "+
			"pkg.go.dev's side tabs are excluded, its documentation - versioned links too - is not", sent, want)
	}
}

type docNetworker struct {
	nopNetworker
	body, contentType string
	// A temporary redirect to here, as a download link to a cache.
	servedFrom string
}

func (d docNetworker) Fetch(url string) (*networker.FetchResult, error) {
	if strings.HasSuffix(url, "/robots.txt") {
		return &networker.FetchResult{Status: http.StatusNotFound}, nil
	}

	final := url
	if d.servedFrom != "" {
		final = d.servedFrom
	}

	return &networker.FetchResult{Status: http.StatusOK, Body: []byte(d.body), ContentType: d.contentType, FinalURL: final, IdentityURL: url}, nil
}

type fakeDocs struct {
	read []string
	err  error
}

func (f *fakeDocs) Read(_ context.Context, _ []byte, contentType string) (string, string, error) {
	f.read = append(f.read, contentType)
	return "Oxide: The Essence of Rust", "Rust claims to advance industrial programming.", f.err
}

func TestOnlyALabsOwnDocumentIsRead(t *testing.T) {
	const pdf, epub = "%PDF-1.5\n%\xe2\xe3\n1 0 obj", "PK\x03\x04\x14\x00\x00\x00mimetypeapplication/epub+zip"

	cases := []struct {
		name        string
		start       string
		read        bool
		body, ctype string
		err         error
		wantRead    bool
		wantText    bool
		servedFrom  string
	}{
		{"the lab's PDF", "https://example.com/paper", true, pdf, "application/pdf", nil, true, true, ""},
		{"the lab's EPUB, from a cache", "https://example.com/paper", true, epub, "application/epub+zip", nil, true, true,
			"https://cache.example.com/pg132.epub"},
		{"the lab's EPUB, typed wrongly", "https://example.com/paper", true, epub, "application/octet-stream", nil, true, true, ""},
		{"a plain crawl", "https://example.com/paper", false, pdf, "application/pdf", nil, false, false, ""},
		{"linked from a lab's source", "https://example.com/", true, pdf, "application/pdf", nil, false, false, ""},
		{"a lab's web page", "https://example.com/paper", true, "<html><body>hi</body></html>", "text/html", nil, false, false, ""},
		{"unreadable", "https://example.com/paper", true, pdf, "application/pdf", errors.New("encrypted"), true, false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			docs := &fakeDocs{err: tc.err}
			h.repo.networker = docNetworker{body: tc.body, contentType: tc.ctype, servedFrom: tc.servedFrom}
			h.repo.ReadsDocuments(docs)
			h.task.URL = "https://example.com/paper"
			h.task.Run.StartURL, h.task.Run.ReadDocuments = tc.start, tc.read

			h.run()

			if read := len(docs.read) > 0; read != tc.wantRead {
				t.Errorf("read the document: %v, want %v", read, tc.wantRead)
			}

			if tc.wantRead && docs.read[0] != tc.ctype {
				t.Errorf("passed content type %q", docs.read[0])
			}

			sink := h.repo.documents.(*recordingSink)
			gotText := len(sink.docs) > 0 && sink.docs[0].Content == "Rust claims to advance industrial programming."

			if gotText != tc.wantText {
				t.Errorf("indexed its text: %v, want %v (docs %+v)", gotText, tc.wantText, sink.docs)
			}

			if tc.wantText && sink.docs[0].Title != "Oxide: The Essence of Rust" {
				t.Errorf("title %q", sink.docs[0].Title)
			}
		})
	}
}
