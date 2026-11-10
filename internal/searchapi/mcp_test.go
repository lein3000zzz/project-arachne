package searchapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"web-crawler/internal/artifacts"
	"web-crawler/internal/auth"
	"web-crawler/internal/labs"
	"web-crawler/internal/research"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (f *fakeArtifacts) List(_ context.Context, owner, lab int64) ([]artifacts.Artifact, error) {
	f.owner, f.lab = owner, lab
	return []artifacts.Artifact{{ID: 9, LabID: lab, Kind: artifacts.KindReport, Title: "Channels",
		Latest: &artifacts.Version{Number: 1, Status: artifacts.StatusRunning}}}, nil
}

func (f *fakeLabs) AddSources(_ context.Context, owner, lab int64, sources []labs.SourceRequest) (labs.Detail, error) {
	f.owner, f.labIDs, f.created.Sources = owner, []int64{lab}, sources
	return labs.Detail{Lab: labs.Lab{ID: lab}, Sources: []labs.SourceStatus{{Source: labs.Source{ID: 1, URL: sources[0].URL}}}}, f.err
}

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))

	return http.DefaultTransport.RoundTrip(r)
}

func mcpServer(t *testing.T, services Services) *httptest.Server {
	t.Helper()

	cfg := testConfig()
	cfg.MCP, cfg.WebLookupTimeout = true, time.Second

	srv := httptest.NewServer(newTestHandler(services, cfg))
	t.Cleanup(srv.Close)

	return srv
}

type heard struct {
	mu       sync.Mutex
	messages []string
}

// has waits a moment for a message: notifications may be handled after the
// call's result.
func (h *heard) has(message string) bool {
	for range 50 {
		h.mu.Lock()
		found := slices.Contains(h.messages, message)
		h.mu.Unlock()

		if found {
			return true
		}

		time.Sleep(10 * time.Millisecond)
	}

	return false
}

func (h *heard) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Clone(h.messages)
}

func connect(t *testing.T, srv *httptest.Server, token string) (*mcp.ClientSession, *heard, error) {
	t.Helper()

	progress := &heard{}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			progress.mu.Lock()
			defer progress.mu.Unlock()

			progress.messages = append(progress.messages, req.Params.Message)
		},
	})

	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: srv.URL + mcpPath, HTTPClient: &http.Client{Transport: bearer(token)}, MaxRetries: -1,
	}, nil)
	if err == nil {
		t.Cleanup(func() { _ = session.Close() })
	}

	return session, progress, err
}

func call(t *testing.T, session *mcp.ClientSession, name string, args map[string]any, progress bool) (*mcp.CallToolResult, string) {
	t.Helper()

	params := &mcp.CallToolParams{Name: name, Arguments: args}
	if progress {
		params.SetProgressToken("p1")
	}

	res, err := session.CallTool(context.Background(), params)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}

	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}

	return res, text.String()
}

func TestMCPNeedsAnAPIToken(t *testing.T) {
	srv := mcpServer(t, Services{})

	if _, _, err := connect(t, srv, "arachne_wrong"); err == nil {
		t.Error("a wrong token connected")
	}

	// A browser's session is no way in: a page on another site could ride it.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+mcpPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: testSession})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("with a session cookie: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	cfg := testConfig()
	if rec := do(newTestHandler(Services{}, cfg), signedIn(httptest.NewRequest(http.MethodPost, mcpPath, nil))); rec.Code != http.StatusNotFound {
		t.Errorf("with MCP off: %d", rec.Code)
	}
}

func TestMCPToolsAreListedWithHints(t *testing.T) {
	session, _, err := connect(t, mcpServer(t, Services{}), testToken)
	if err != nil {
		t.Fatal(err)
	}

	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	var names []string

	for _, tool := range list.Tools {
		names = append(names, tool.Name)

		if tool.Annotations == nil || tool.Description == "" {
			t.Errorf("%s has no hints or description", tool.Name)
		}

		if tool.Annotations != nil && tool.Annotations.ReadOnlyHint && !slices.Contains([]string{"search", "list_labs", "get_lab", "draft_report", "read_report", "read_map"}, tool.Name) {
			t.Errorf("%s claims to change nothing", tool.Name)
		}
	}

	want := []string{"add_sources", "ask", "create_lab", "draft_report", "get_lab", "list_labs", "read_map", "read_report", "research", "save_answer", "search",
		"set_work", "write_report"}
	if slices.Sort(names); !slices.Equal(names, want) {
		t.Errorf("tools %v", names)
	}
}

func TestMCPAskAnswersAsTheTokensUserAndKeepsTheAnswer(t *testing.T) {
	answerer, labService, reports := &fakeAnswerer{verifying: true}, &fakeLabs{}, &fakeArtifacts{}

	session, progress, err := connect(t, mcpServer(t, Services{Answerer: answerer, Labs: labService, Artifacts: reports}), testToken)
	if err != nil {
		t.Fatal(err)
	}

	res, text := call(t, session, "ask", map[string]any{"question": "how do goroutines talk?", "labs": []int{4}}, true)
	if res.IsError {
		t.Fatal(text)
	}

	for _, want := range []string{"Use channels", "[1] Go <https://go.dev/> (shared index)", "Checked 1 claim against what they cite: 1 supported.", "Answer id: ",
		`on speed: [1] "Zig is faster here", but [2] "Rust is faster here"`} {
		if !strings.Contains(text, want) {
			t.Errorf("the answer lacks %q:\n%s", want, text)
		}
	}

	if !slices.Equal(answerer.got.LabURLs, []string{"https://go.dev/doc/"}) || labService.owner != testUser.ID || answerer.got.Fresh {
		t.Errorf("asked %+v, labs of user %d", answerer.got, labService.owner)
	}

	if !progress.has("checked 1 claim") || !progress.has("writing the answer from 1 source") {
		t.Errorf("progress heard: %q", progress.all())
	}

	id := strings.TrimSpace(strings.Fields(text[strings.Index(text, "Answer id: ")+len("Answer id: "):])[0])

	res, text = call(t, session, "save_answer", map[string]any{"lab": 3, "answer_id": id}, false)
	if res.IsError || reports.saved.Text != "Use channels\n[1]." || reports.lab != 3 || reports.owner != testUser.ID {
		t.Errorf("saved %+v: %s", reports.saved, text)
	}

	if res, text = call(t, session, "save_answer", map[string]any{"lab": 3, "answer_id": "nope"}, false); !res.IsError || !strings.Contains(text, "no longer kept") {
		t.Errorf("an unknown answer: %s", text)
	}
}

func TestMCPAskReadsTheWebFirst(t *testing.T) {
	answerer := &fakeAnswerer{}
	look := &fakeLookup{pages: []string{"https://go.dev/"}}

	session, progress, err := connect(t, mcpServer(t, Services{Answerer: answerer, WebLookup: look}), testToken)
	if err != nil {
		t.Fatal(err)
	}

	res, text := call(t, session, "ask", map[string]any{"question": "zig vs rust", "web": true}, true)
	if res.IsError || !strings.Contains(text, "(web, read just now)") || !strings.Contains(text, "Looked on the web: read 1 page.") ||
		!strings.Contains(text, "were not checked") || !answerer.got.Fresh {
		t.Errorf("from the web:\n%s", text)
	}

	if !progress.has("searching the web") || !progress.has("reading the web: 1 of 1 pages done") {
		t.Errorf("progress heard: %q", progress.all())
	}

	// The web giving nothing is said, and the index answers anyway.
	session, _, _ = connect(t, mcpServer(t, Services{Answerer: &fakeAnswerer{}, WebLookup: &fakeLookup{err: errors.New("searxng: 502")}}), testToken)
	if res, text = call(t, session, "ask", map[string]any{"question": "zig", "web": true}, false); res.IsError ||
		!strings.Contains(text, "Looked on the web: the web could not be searched.") || strings.Contains(text, "502") {
		t.Errorf("the web failing:\n%s", text)
	}

	session, _, _ = connect(t, mcpServer(t, Services{Answerer: &fakeAnswerer{}}), testToken)
	if res, text = call(t, session, "ask", map[string]any{"question": "zig", "web": true}, false); !res.IsError || !strings.Contains(text, "looking on the web needs") {
		t.Errorf("without look-ups: %s", text)
	}
}

func TestMCPLabsAndReports(t *testing.T) {
	labService, reports, researcher := &fakeLabs{}, &fakeArtifacts{}, &fakeResearch{run: &research.Run{ID: 4, Status: research.StatusDone, Stopped: research.StopSaturated}}

	session, _, err := connect(t, mcpServer(t, Services{Labs: labService, Artifacts: reports, Research: researcher}), testToken)
	if err != nil {
		t.Fatal(err)
	}

	res, text := call(t, session, "get_lab", map[string]any{"lab": 4}, false)
	if res.IsError || !strings.Contains(text, `"reports":[{"id":9`) || !strings.Contains(text, `"status":"done","stopped":"saturated","works":1}`) || !strings.Contains(text, `"verdict":"mixed"`) {
		t.Errorf("get_lab: %s", text)
	}

	if res, text = call(t, session, "add_sources", map[string]any{"lab": 4, "urls": []string{" https://go.dev/ref/mem "}}, false); res.IsError ||
		labService.created.Sources[0].URL != "https://go.dev/ref/mem" {
		t.Errorf("add_sources: %s", text)
	}

	if res, text = call(t, session, "research", map[string]any{"lab": 4, "request": "the memory model", "found": 50}, false); res.IsError ||
		researcher.request != "the memory model" || *researcher.ask.Found != 50 || researcher.ask.Read != nil || !strings.Contains(text, `"budget":{"found":50`) {
		t.Errorf("research: %s", text)
	}

	if res, text = call(t, session, "read_map", map[string]any{"lab": 4}, false); res.IsError || !strings.Contains(text, "## 1. Blocks (subtopic 1): 1 kept") || !strings.Contains(text, "work 2, paper") ||
		!strings.Contains(text, "[ResNet](<https://arxiv.org/abs/1512.03385>)") || !strings.Contains(text, "stopped: new rounds found little new") {
		t.Errorf("read_map:\n%s", text)
	}

	if res, text = call(t, session, "research", map[string]any{"lab": 4, "work": 2}, false); res.IsError || researcher.ask.Focus.Work != 2 {
		t.Errorf("research one work: %s", text)
	}

	if res, text = call(t, session, "set_work", map[string]any{"lab": 4, "work": 2, "verdict": "drop", "read": true}, false); res.IsError ||
		!strings.Contains(text, `"read":"reading"`) {
		t.Errorf("set_work: %s", text)
	}

	if res, text = call(t, session, "set_work", map[string]any{"lab": 4, "work": 2}, false); !res.IsError {
		t.Errorf("set_work with nothing to do: %s", text)
	}

	// No sections: the outline is drafted, then written.
	if res, text = call(t, session, "write_report", map[string]any{"lab": 4, "request": "channels"}, false); res.IsError ||
		reports.outline.Title != "channels" || len(reports.outline.Sections) != 1 || !strings.Contains(text, `"id":9`) {
		t.Errorf("write_report: %+v %s", reports.outline, text)
	}

	call(t, session, "write_report", map[string]any{"lab": 4, "request": "channels", "title": "Mine",
		"sections": []map[string]string{{"heading": "B", "focus": "b"}, {"heading": "C", "focus": "c"}}}, false)
	if reports.outline.Title != "Mine" || len(reports.outline.Sections) != 2 {
		t.Errorf("the outline given was not written: %+v", reports.outline)
	}

	if res, text = call(t, session, "read_report", map[string]any{"report": 9}, false); res.IsError || text != "# T\n" {
		t.Errorf("read_report: %q", text)
	}

	labService.err = labs.ErrNotFound
	if res, text = call(t, session, "get_lab", map[string]any{"lab": 5}, false); !res.IsError || text != labs.ErrNotFound.Error() {
		t.Errorf("another user's lab: %s", text)
	}

	labService.err = errors.New("pq: connection refused")
	if res, text = call(t, session, "list_labs", nil, false); !res.IsError || strings.Contains(text, "pq") {
		t.Errorf("an internal failure shows: %s", text)
	}
}

func TestAReportBeingWrittenSaysHowFar(t *testing.T) {
	text := reportText(artifacts.Artifact{Kind: artifacts.KindReport, Title: "Channels"}, artifacts.Version{Status: artifacts.StatusRunning,
		Sections: []artifacts.Section{{Heading: "A", State: artifacts.StateDone, Text: "Text [1]."}, {Heading: "B", State: artifacts.StateWriting}}})

	if !strings.HasPrefix(text, "Still being written: 1 of 2 sections done.") || !strings.Contains(text, "Text [1].") ||
		!strings.Contains(text, "> Not written: still being written.") {
		t.Errorf("report:\n%s", text)
	}
}

type fakeTokens struct {
	user  int64
	names []string
	err   error
}

func (f *fakeTokens) AddToken(_ context.Context, user int64, name string) (string, error) {
	f.user, f.names = user, append(f.names, name)
	return "arachne_new", f.err
}

func (f *fakeTokens) Tokens(_ context.Context, user int64) ([]auth.Token, error) {
	f.user = user
	return []auth.Token{{Name: "claude-code"}}, f.err
}

func (f *fakeTokens) RemoveToken(_ context.Context, user int64, name string) error {
	f.user, f.names = user, append(f.names, "-"+name)
	return f.err
}

func TestTokensAreManagedFromABrowserOnly(t *testing.T) {
	tokens := &fakeTokens{}
	cfg := testConfig()
	cfg.MCP = true
	handler := newTestHandler(Services{Tokens: tokens}, cfg)

	if rec := do(handler, signedIn(httptest.NewRequest(http.MethodPost, "/api/me/tokens", strings.NewReader(`{"name":"x"}`)))); rec.Code != http.StatusForbidden {
		t.Errorf("a token making tokens: %d", rec.Code)
	}

	rec := do(handler, withSession(httptest.NewRequest(http.MethodPost, "/api/me/tokens", strings.NewReader(`{"name":"claude-code"}`))))
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"token":"arachne_new"`) || tokens.user != testUser.ID {
		t.Errorf("add: %d %s", rec.Code, rec.Body)
	}

	if rec := do(handler, withSession(httptest.NewRequest(http.MethodGet, "/api/me/tokens", nil))); !strings.Contains(rec.Body.String(), `"mcp":"/mcp"`) ||
		!strings.Contains(rec.Body.String(), `"name":"claude-code"`) {
		t.Errorf("list: %s", rec.Body)
	}

	if rec := do(handler, withSession(httptest.NewRequest(http.MethodDelete, "/api/me/tokens/claude-code", nil))); rec.Code != http.StatusNoContent ||
		tokens.names[len(tokens.names)-1] != "-claude-code" {
		t.Errorf("remove: %d %v", rec.Code, tokens.names)
	}

	for _, c := range []struct {
		err  error
		want int
	}{{auth.ErrTokenExists, http.StatusConflict}, {auth.ErrTooManyTokens, http.StatusConflict}, {auth.ErrInvalidTokenName, http.StatusBadRequest},
		{errors.New("boom"), http.StatusInternalServerError}} {
		tokens.err = c.err
		if rec := do(handler, withSession(httptest.NewRequest(http.MethodPost, "/api/me/tokens", strings.NewReader(`{"name":"x"}`)))); rec.Code != c.want ||
			strings.Contains(rec.Body.String(), "boom") {
			t.Errorf("%v: %d %s", c.err, rec.Code, rec.Body)
		}
	}
}
