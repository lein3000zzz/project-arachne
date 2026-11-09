package searchapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"web-crawler/internal/answer"
	"web-crawler/internal/artifacts"
	"web-crawler/internal/auth"
	"web-crawler/internal/labs"
	"web-crawler/internal/research"
	"web-crawler/internal/retrieval"
	"web-crawler/internal/weblookup"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

const mcpPath = "/mcp"

const mcpInstructions = `arachne answers questions from web pages it has crawled, reviewed and indexed, citing each claim by its source; a claim the cited source does not bear out is marked, never hidden.
A lab is a topic with its sources: pages and sites the user (or you) added by address, crawled into the index. Answers and searches can be narrowed to labs, and reports are written from a lab's pages alone.
Researching a lab maps what exists on its topic - papers and web works found by search and by following citations, each judged, the kept ones read into the lab - and estimates how much is still unfound.
You steer, you never write what a lab knows: add sources by URL, ask, research, request reports. Long work (crawling sources, research, reports) runs in the background: check on it with get_lab, read_map or read_report.`

// mcpTools are the same services as the pages, as tools for agents that sign
// in with an API token. The token's user is the one acting: the labs, the
// model endpoint and the limits are theirs.
type mcpTools struct {
	logger    *zap.SugaredLogger
	cfg       Config
	searcher  Searcher
	answers   *answerHandler
	labs      *labsHandler
	research  *researchHandler
	artifacts *artifactsHandler
}

// Stateless: every call stands alone, so nothing is kept between calls and
// no session can be taken over by another token.
func newMCPHandler(signIn *authHandler, tools *mcpTools) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "arachne", Title: "arachne", Version: "1"}, &mcp.ServerOptions{Instructions: mcpInstructions})
	tools.register(server)

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, PropagateRequestCancellation: true})

	// Signed in already, by signIn.agent: this only hands the user to the
	// tools, the way the SDK passes it.
	user := func(ctx context.Context, _ string, _ *http.Request) (*mcpauth.TokenInfo, error) {
		user, ok := auth.UserFrom(ctx)
		if !ok {
			return nil, mcpauth.ErrInvalidToken
		}

		return &mcpauth.TokenInfo{UserID: strconv.FormatInt(user.ID, 10), Extra: map[string]any{mcpUserKey: user}}, nil
	}

	// API tokens do not expire; revoking one is what ends it.
	return signIn.agent(mcpauth.RequireBearerToken(user, &mcpauth.RequireBearerTokenOptions{AllowMissingExpiration: true})(handler))
}

const mcpUserKey = "user"

func mcpUser(call *mcp.CallToolRequest) (auth.User, bool) {
	if call.Extra == nil || call.Extra.TokenInfo == nil {
		return auth.User{}, false
	}

	user, ok := call.Extra.TokenInfo.Extra[mcpUserKey].(auth.User)

	return user, ok
}

// addTool registers a tool whose result is JSON; do's errors must be fit to
// show, as the agent reads them.
func addTool[In, Out any](s *mcp.Server, def *mcp.Tool, do func(ctx context.Context, user auth.User, call *mcp.CallToolRequest, in In) (Out, error)) {
	mcp.AddTool(s, def, func(ctx context.Context, call *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var none Out

		user, ok := mcpUser(call)
		if !ok {
			return nil, none, errors.New("sign in first")
		}

		out, err := do(auth.WithUser(ctx, user), user, call, in)

		return nil, out, err
	})
}

// addTextTool registers a tool whose result is Markdown for the agent to
// read: an answer or a report, which JSON would only make longer.
func addTextTool[In any](s *mcp.Server, def *mcp.Tool, do func(ctx context.Context, user auth.User, call *mcp.CallToolRequest, in In) (string, error)) {
	mcp.AddTool(s, def, func(ctx context.Context, call *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		user, ok := mcpUser(call)
		if !ok {
			return nil, nil, errors.New("sign in first")
		}

		text, err := do(auth.WithUser(ctx, user), user, call, in)
		if err != nil {
			return nil, nil, err
		}

		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	})
}

var no = false

func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: &no}
}

// changes: adds to the user's labs or the shared base, never removes; web
// says it reaches out to the web.
func changes(title string, web bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: &no, OpenWorldHint: &web}
}

func (t *mcpTools) register(s *mcp.Server) {
	addTool(s, &mcp.Tool{Name: "search", Annotations: readOnly("Search the index"),
		Description: "Search the pages arachne has indexed - hybrid lexical and semantic, reranked - and return the best matches with a snippet each. No model writes anything: use ask for an answer."},
		t.search)
	addTextTool(s, &mcp.Tool{Name: "ask", Annotations: changes("Ask", true),
		Description: "Answer a question from indexed pages, citing them as [n], each claim checked against what it cites. Narrow to labs to answer from their pages first. With web, the web is searched and the best pages are read into the shared index first (minutes). Returns the answer in Markdown, its sources, the claims the sources do not bear out, and an answer id for save_answer."},
		t.ask)
	addTool(s, &mcp.Tool{Name: "list_labs", Annotations: readOnly("List labs"),
		Description: "List the user's labs: topics with their sources and how many pages are indexed."},
		t.listLabs)
	addTool(s, &mcp.Tool{Name: "get_lab", Annotations: readOnly("Show a lab"),
		Description: "Show a lab: its sources and how far each one's crawl has come, its latest research and how complete its map looks, and its reports."},
		t.getLab)
	addTool(s, &mcp.Tool{Name: "create_lab", Annotations: changes("Create a lab", true),
		Description: "Create a lab on a topic, optionally with sources to crawl (pages or sites, by URL). Crawling and indexing run in the background: check with get_lab."},
		t.createLab)
	addTool(s, &mcp.Tool{Name: "add_sources", Annotations: changes("Add sources to a lab", true),
		Description: "Add pages or sites, by URL, to a lab as sources. Each is crawled, reviewed and indexed in the background: check with get_lab."},
		t.addSources)
	addTool(s, &mcp.Tool{Name: "research", Annotations: changes("Research a lab's topic", true),
		Description: "Map what exists on a lab's topic: rounds of web and OpenAlex search and of following what kept works cite and what cites them; the model judges each work by what it shows, niche or famous, and the kept ones are read into the lab. Runs in the background, often tens of minutes, and stops when new rounds find little new. Check with get_lab; read the map with read_map."},
		t.startResearch)
	addTextTool(s, &mcp.Tool{Name: "read_map", Annotations: readOnly("Read a lab's map"),
		Description: "Read a lab's research map as Markdown: its subtopics, the works kept in each with how they were found and whether they are read, how complete the map looks, and optionally what was dropped and why."},
		t.readMap)
	addTool(s, &mcp.Tool{Name: "draft_report", Annotations: readOnly("Draft a report outline"),
		Description: "Draft the outline of a report on a lab: a title and sections, each with what it is to cover. Nothing is written yet; pass the outline, changed as you like, to write_report."},
		t.draftReport)
	addTool(s, &mcp.Tool{Name: "write_report", Annotations: changes("Write a report", false),
		Description: "Write a report from a lab's pages alone, section by section, each claim checked. Give the sections from draft_report, or none to have them drafted. Runs in the background (minutes): read it with read_report."},
		t.writeReport)
	addTextTool(s, &mcp.Tool{Name: "read_report", Annotations: readOnly("Read a report"),
		Description: "Read a report or a saved answer as Markdown, with its sources and the claims they do not bear out. While it is being written, says how far it has come."},
		t.readReport)
	addTool(s, &mcp.Tool{Name: "save_answer", Annotations: changes("Save an answer to a lab", false),
		Description: "Keep an answer from ask, as given and checked, in a lab, by the answer id ask returned. Answers are kept for an hour."},
		t.saveAnswer)
}

// progressOf reports how a long call is going when the agent asked to hear.
func progressOf(ctx context.Context, call *mcp.CallToolRequest) func(string) {
	token := call.Params.GetProgressToken()
	if token == nil || call.Session == nil {
		return func(string) {}
	}

	var (
		mu sync.Mutex
		n  float64
	)

	return func(message string) {
		mu.Lock()
		defer mu.Unlock()

		n++
		_ = call.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: n, Message: message})
	}
}

// ---- search ----

type searchInput struct {
	Query string  `json:"query" jsonschema:"what to look for"`
	Labs  []int64 `json:"labs,omitempty" jsonschema:"search only the pages of these labs, by id from list_labs"`
	Site  string  `json:"site,omitempty" jsonschema:"only pages of this host, e.g. go.dev"`
	Limit int     `json:"limit,omitempty" jsonschema:"how many results, 10 by default"`
}

type searchResult struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Site    string `json:"site"`
	Snippet string `json:"snippet"`
}

type searchOutput struct {
	Results []searchResult `json:"results"`
}

func (t *mcpTools) search(ctx context.Context, user auth.User, _ *mcp.CallToolRequest, in searchInput) (searchOutput, error) {
	limit := orDefault(in.Limit, t.cfg.DefaultLimit)
	if limit < 1 || limit > t.cfg.MaxLimit {
		return searchOutput{}, fmt.Errorf("limit must be between 1 and %d", t.cfg.MaxLimit)
	}

	ctx, cancel := context.WithTimeout(ctx, t.cfg.RequestTimeout)
	defer cancel()

	req := retrieval.Request{Query: in.Query, Host: in.Site, Mode: retrieval.ModeHybrid, Limit: limit}

	if len(in.Labs) > 0 {
		urls, err := labPages(ctx, t.labs.service, user.ID, in.Labs)
		if err != nil {
			return searchOutput{}, t.labFailed(err)
		}

		req.URLs = urls
	}

	results, err := t.searcher.Search(ctx, req)
	if err != nil {
		_, message := searchFailure(t.logger, req, err)
		return searchOutput{}, errors.New(message)
	}

	out := searchOutput{Results: make([]searchResult, len(results))}
	for i, r := range results {
		out.Results[i] = searchResult{URL: r.URL, Title: r.Title, Site: r.Host, Snippet: r.Snippet}
	}

	return out, nil
}

func orDefault(v, fallback int) int {
	if v == 0 {
		return fallback
	}

	return v
}

// ---- ask ----

type askInput struct {
	Question string  `json:"question" jsonschema:"the question, in plain words"`
	Labs     []int64 `json:"labs,omitempty" jsonschema:"answer from these labs' pages first, by id from list_labs; the shared index fills in when they hold too little"`
	Web      bool    `json:"web,omitempty" jsonschema:"search the web and read the best pages into the index before answering; takes minutes"`
	NoCheck  bool    `json:"no_check,omitempty" jsonschema:"skip checking each claim against its sources: faster, but claims the sources do not bear out go unmarked"`
}

func (t *mcpTools) ask(ctx context.Context, user auth.User, call *mcp.CallToolRequest, in askInput) (string, error) {
	a := t.answers

	switch {
	case a.answerer == nil:
		return "", errors.New("answering is not configured")
	case in.Web && a.web == nil:
		return "", weblookup.ErrUnavailable
	}

	timeout := a.timeout
	if in.Web {
		timeout += a.webTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	progress := progressOf(ctx, call)

	req := answer.Request{Query: strings.TrimSpace(in.Question), Mode: retrieval.ModeHybrid}
	if req.Query == "" {
		return "", retrieval.ErrEmptyQuery
	}

	if len(in.Labs) > 0 {
		urls, err := labPages(ctx, t.labs.service, user.ID, in.Labs)
		if err != nil {
			return "", t.labFailed(err)
		}

		req.LabURLs = urls
	}

	var looked webLook

	if in.Web {
		looked = t.lookOnTheWeb(ctx, user, &req, progress)
		if errors.Is(ctx.Err(), context.Canceled) {
			return "", ctx.Err()
		}
	}

	progress("reading the sources")

	res, err := a.answerer.Answer(ctx, req, answer.Events{Sources: func(list []answer.Source) error {
		progress("writing the answer from " + count(len(list), "source"))
		return nil
	}})
	if err != nil {
		_, message := a.failure(req, err)
		if in.Web {
			message += "; " + looked.said()
		}

		return "", errors.New(message)
	}

	check := a.answerer.Verifying() && !in.NoCheck
	id := a.recent.add(user.ID, req.Query, res, !check)

	var (
		claims []answer.Claim
		broke  bool
	)

	if check {
		progress("checking the claims")

		done := 0

		v, err := a.answerer.Verify(ctx, res, answer.VerifyEvents{
			Claims: func(_ []answer.Part, list []answer.Claim) error {
				progress("checking " + count(len(list), "claim"))
				return nil
			},
			Claim: func(int, answer.Claim) error {
				done++
				progress("checked " + count(done, "claim"))

				return nil
			},
		})
		if err != nil {
			t.logger.Warnw("Verifying an answer failed", "error", err)

			broke = true
		}

		claims = v.Claims
		a.recent.check(id, claims)
	}

	return answerText(res, claims, check, broke, looked, in.Web, id), nil
}

// webLook is what looking on the web gave: the pages read, or why none.
type webLook struct {
	pages   []string
	failure string
}

func (w webLook) said() string {
	switch {
	case w.failure != "":
		return "looked on the web: " + w.failure
	case len(w.pages) == 0:
		return "looked on the web: no page could be read"
	}

	return "looked on the web: read " + count(len(w.pages), "page")
}

// lookOnTheWeb reads the web for the question into the index and has the
// answer read those pages first. When the web gives nothing, the index
// answers as it would have.
func (t *mcpTools) lookOnTheWeb(ctx context.Context, user auth.User, req *answer.Request, progress func(string)) webLook {
	progress("searching the web")

	pages, err := t.answers.web.Look(ctx, user.ID, req.Query, weblookup.Events{
		Searched: func(_ string, found []weblookup.Found, _ map[string]string) error {
			picked := 0
			for _, f := range found {
				if f.Read {
					picked++
				}
			}

			progress(fmt.Sprintf("searched the web: %d results, reading %d", len(found), picked))

			return nil
		},
		Reading: func(reading []weblookup.Reading) error {
			ended := 0
			for _, r := range reading {
				if weblookup.Ended(r.State) {
					ended++
				}
			}

			progress(fmt.Sprintf("reading the web: %d of %d pages done", ended, len(reading)))

			return nil
		},
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return webLook{}
		}

		return webLook{failure: t.answers.webFailure(err)}
	}

	for _, p := range pages {
		if !slices.Contains(req.LabURLs, p) {
			req.LabURLs = append(req.LabURLs, p)
		}
	}

	req.Fresh = len(pages) > 0

	return webLook{pages: pages}
}

// answerText is the answer as the agent reads it: the Markdown, where each
// source came from, and the claims the sources did not bear out - the marks
// a page shows, in words.
func answerText(res answer.Result, claims []answer.Claim, checked, broke bool, looked webLook, web bool, id string) string {
	var b strings.Builder

	b.WriteString(strings.TrimSpace(res.Answer))
	b.WriteString("\n\n")

	if res.Weak {
		b.WriteString("Note: nothing clearly relevant was found, so the sources may not answer the question.\n\n")
	}

	if len(res.Sources) > 0 {
		b.WriteString("Sources:\n")

		for _, s := range res.Sources {
			from := "shared index"

			switch {
			case slices.Contains(looked.pages, s.URL):
				from = "web, read just now"
			case s.InLab:
				from = "lab"
			}

			title := s.Title
			if title == "" {
				title = s.URL
			}

			fmt.Fprintf(&b, "[%d] %s <%s> (%s)\n", s.N, oneLine(title), s.URL, from)
		}

		b.WriteString("\n")
	}

	switch {
	case !checked:
		b.WriteString("The claims were not checked against their sources.\n")
	case broke:
		b.WriteString("The claims could not be checked against their sources.\n")
	default:
		b.WriteString(claimSummary(claims))
	}

	if web {
		said := looked.said()
		b.WriteString(strings.ToUpper(said[:1]) + said[1:] + ".\n")
	}

	if id != "" {
		fmt.Fprintf(&b, "\nAnswer id: %s (save_answer keeps it in a lab, for the next hour).\n", id)
	}

	return b.String()
}

func claimSummary(claims []answer.Claim) string {
	if len(claims) == 0 {
		return "No sentence cites a source, so there were no claims to check.\n"
	}

	counts := map[answer.Verdict]int{}
	for _, c := range claims {
		counts[c.Verdict]++
	}

	var said []string

	for _, v := range []struct {
		verdict answer.Verdict
		name    string
	}{{answer.Supported, "supported"}, {answer.Partly, "partly supported"}, {answer.Unsupported, "not supported"}, {answer.Unchecked, "unchecked"}} {
		if counts[v.verdict] > 0 {
			said = append(said, fmt.Sprintf("%d %s", counts[v.verdict], v.name))
		}
	}

	var b strings.Builder

	fmt.Fprintf(&b, "Checked %s against what they cite: %s.\n", count(len(claims), "claim"), strings.Join(said, ", "))

	for _, c := range claims {
		if c.Verdict != answer.Unsupported && c.Verdict != answer.Partly {
			continue
		}

		fmt.Fprintf(&b, "- %s: %q %s", map[answer.Verdict]string{answer.Unsupported: "Not supported", answer.Partly: "Partly"}[c.Verdict], c.Text, cites(c.Cites))

		if c.Note != "" {
			fmt.Fprintf(&b, " (%s)", c.Note)
		}

		b.WriteString("\n")
	}

	return b.String()
}

func cites(ns []int) string {
	var b strings.Builder
	for _, n := range ns {
		fmt.Fprintf(&b, "[%d]", n)
	}

	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}

	return strconv.Itoa(n) + " " + noun + "s"
}

// ---- labs ----

type labInput struct {
	Lab int64 `json:"lab" jsonschema:"the lab's id, from list_labs"`
}

type labSummary struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Topic   string `json:"topic"`
	Sources int    `json:"sources"`
	// Indexed or set aside on review.
	Pages     int       `json:"pages"`
	SetAside  int       `json:"set_aside"`
	UpdatedAt time.Time `json:"updated_at"`
}

func summaryOf(l labs.Lab) labSummary {
	return labSummary{ID: l.ID, Name: l.Name, Topic: l.Topic, Sources: l.Sources, Pages: l.Pages, SetAside: l.SetAside, UpdatedAt: l.UpdatedAt}
}

type listLabsOutput struct {
	Labs []labSummary `json:"labs"`
}

func (t *mcpTools) listLabs(ctx context.Context, user auth.User, _ *mcp.CallToolRequest, _ struct{}) (listLabsOutput, error) {
	if t.labs.service == nil {
		return listLabsOutput{}, errors.New("labs are not available")
	}

	ctx, cancel := context.WithTimeout(ctx, labsTimeout)
	defer cancel()

	list, err := t.labs.service.List(ctx, user.ID)
	if err != nil {
		return listLabsOutput{}, t.labFailed(err)
	}

	out := listLabsOutput{Labs: make([]labSummary, len(list))}
	for i, l := range list {
		out.Labs[i] = summaryOf(l)
	}

	return out, nil
}

type sourceView struct {
	ID    int64  `json:"id"`
	Kind  string `json:"kind"`
	URL   string `json:"url"`
	Pages int    `json:"pages"`
	// The latest crawl's status: queued, running, indexing, finished, dropped
	// or stalled; empty before the first.
	Crawl string `json:"crawl,omitempty"`
	Note  string `json:"note,omitempty"`
}

type researchView struct {
	Status  string `json:"status"`
	Stopped string `json:"stopped,omitempty"`
	Error   string `json:"error,omitempty"`
	Rounds  int    `json:"rounds"`
	Works   int    `json:"works"`
	Kept    int    `json:"kept"`
	Pending int    `json:"not_yet_judged"`
	Read    int    `json:"read"`
	InIndex int    `json:"in_index"`
	// The estimated share of the kept works that exist that are found: rough,
	// and likely optimistic.
	Coverage *float64 `json:"coverage,omitempty"`
}

func researchOf(m research.Map) *researchView {
	if m.Run == nil {
		return nil
	}

	c := m.Coverage
	v := &researchView{Status: m.Run.Status, Stopped: m.Run.Stopped, Error: m.Run.Error, Rounds: len(m.Run.Rounds), Works: c.Works, Kept: c.Kept,
		Pending: c.Pending, Read: c.Read, InIndex: c.In}

	if c.Estimate != nil {
		v.Coverage = &c.Estimate.Share
	}

	return v
}

type reportView struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Status    string    `json:"status,omitempty"`
	Version   int       `json:"version,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

func reportOf(a artifacts.Artifact) reportView {
	r := reportView{ID: a.ID, Kind: string(a.Kind), Title: a.Title, UpdatedAt: a.UpdatedAt}
	if a.Latest != nil {
		r.Status, r.Version = string(a.Latest.Status), a.Latest.Number
	}

	return r
}

type labView struct {
	Lab      labSummary    `json:"lab"`
	Sources  []sourceView  `json:"sources"`
	Research *researchView `json:"research,omitempty"`
	Reports  []reportView  `json:"reports"`
}

func (t *mcpTools) getLab(ctx context.Context, user auth.User, _ *mcp.CallToolRequest, in labInput) (labView, error) {
	if t.labs.service == nil {
		return labView{}, errors.New("labs are not available")
	}

	ctx, cancel := context.WithTimeout(ctx, labsTimeout)
	defer cancel()

	detail, err := t.labs.service.Get(ctx, user.ID, in.Lab)
	if err != nil {
		return labView{}, t.labFailed(err)
	}

	return t.viewOf(ctx, user, detail)
}

// viewOf is a lab with its research and its reports, those that can be had:
// the lab is what was asked for.
func (t *mcpTools) viewOf(ctx context.Context, user auth.User, detail labs.Detail) (labView, error) {
	view := labView{Lab: summaryOf(detail.Lab), Sources: make([]sourceView, len(detail.Sources)), Reports: []reportView{}}

	for i, s := range detail.Sources {
		view.Sources[i] = sourceView{ID: s.ID, Kind: s.Kind, URL: s.URL, Pages: s.Pages}
		if s.Run != nil {
			view.Sources[i].Crawl, view.Sources[i].Note = string(s.Run.Status), s.Run.Note
		}
	}

	if t.research.service != nil {
		m, err := t.research.service.Map(ctx, user.ID, detail.ID)
		if err != nil {
			t.logger.Warnw("Reading a lab's research failed", "lab", detail.ID, "error", err)
		}

		view.Research = researchOf(m)
	}

	if t.artifacts.service != nil {
		list, err := t.artifacts.service.List(ctx, user.ID, detail.ID)
		if err != nil {
			t.logger.Warnw("Listing a lab's artifacts failed", "lab", detail.ID, "error", err)
		}

		for _, a := range list {
			view.Reports = append(view.Reports, reportOf(a))
		}
	}

	return view, nil
}

type createLabInput struct {
	Name  string   `json:"name" jsonschema:"a short name, unique among the user's labs"`
	Topic string   `json:"topic" jsonschema:"what the lab is about, in a sentence: research and reports go by it"`
	URLs  []string `json:"urls,omitempty" jsonschema:"pages or sites to crawl as its first sources"`
}

func (t *mcpTools) createLab(ctx context.Context, user auth.User, _ *mcp.CallToolRequest, in createLabInput) (labView, error) {
	if t.labs.service == nil {
		return labView{}, errors.New("labs are not available")
	}

	ctx, cancel := context.WithTimeout(ctx, labsTimeout)
	defer cancel()

	detail, err := t.labs.service.Create(ctx, user.ID, in.Name, in.Topic, sourceRequests(in.URLs), nil)
	if err != nil {
		return labView{}, t.labFailed(err)
	}

	return t.viewOf(ctx, user, detail)
}

type addSourcesInput struct {
	Lab  int64    `json:"lab" jsonschema:"the lab's id, from list_labs"`
	URLs []string `json:"urls" jsonschema:"pages or sites to crawl"`
}

func (t *mcpTools) addSources(ctx context.Context, user auth.User, _ *mcp.CallToolRequest, in addSourcesInput) (labView, error) {
	if t.labs.service == nil {
		return labView{}, errors.New("labs are not available")
	}

	ctx, cancel := context.WithTimeout(ctx, labsTimeout)
	defer cancel()

	detail, err := t.labs.service.AddSources(ctx, user.ID, in.Lab, sourceRequests(in.URLs))
	if err != nil {
		return labView{}, t.labFailed(err)
	}

	return t.viewOf(ctx, user, detail)
}

// sourceRequests leaves scope, depth and pages to the lab's defaults, as the
// pages do for a link pasted alone.
func sourceRequests(urls []string) []labs.SourceRequest {
	out := make([]labs.SourceRequest, len(urls))
	for i, u := range urls {
		out[i] = labs.SourceRequest{URL: strings.TrimSpace(u)}
	}

	return out
}

type researchInput struct {
	Lab     int64  `json:"lab" jsonschema:"the lab's id, from list_labs"`
	Request string `json:"request,omitempty" jsonschema:"what to look for, when not the lab's topic as a whole: newer work, another community, benchmarks"`
	Found   *int   `json:"found,omitempty" jsonschema:"how many works to judge at most; the server's default when absent"`
	Read    *int   `json:"read,omitempty" jsonschema:"how many kept works to read into the lab at most, 0 to map without reading; the server's default when absent"`
}

type runView struct {
	ID     int64           `json:"id"`
	Status string          `json:"status"`
	Budget research.Budget `json:"budget"`
}

func (t *mcpTools) startResearch(ctx context.Context, user auth.User, _ *mcp.CallToolRequest, in researchInput) (runView, error) {
	if t.research.service == nil {
		return runView{}, errors.New("research is not available")
	}

	ctx, cancel := context.WithTimeout(ctx, labsTimeout)
	defer cancel()

	run, err := t.research.service.Start(ctx, user.ID, in.Lab, research.Ask{Request: in.Request, Found: in.Found, Read: in.Read})
	if err != nil {
		return runView{}, t.labFailed(err)
	}

	return runView{ID: run.ID, Status: run.Status, Budget: run.Budget}, nil
}

type readMapInput struct {
	Lab     int64 `json:"lab" jsonschema:"the lab's id, from list_labs"`
	Dropped bool  `json:"dropped,omitempty" jsonschema:"also list the works dropped, with why"`
}

func (t *mcpTools) readMap(ctx context.Context, user auth.User, _ *mcp.CallToolRequest, in readMapInput) (string, error) {
	if t.research.service == nil || t.labs.service == nil {
		return "", errors.New("research is not available")
	}

	ctx, cancel := context.WithTimeout(ctx, labsTimeout)
	defer cancel()

	lab, err := t.labs.service.Get(ctx, user.ID, in.Lab)
	if err != nil {
		return "", t.labFailed(err)
	}

	m, err := t.research.service.Map(ctx, user.ID, in.Lab)
	if err != nil {
		return "", t.labFailed(err)
	}

	return mapText(lab.Name, m, in.Dropped), nil
}

// labFailed is what the agent is told of a lab request that failed: what the
// pages would say, never the error itself.
func (t *mcpTools) labFailed(err error) error {
	switch {
	case errors.Is(err, research.ErrRunning), errors.Is(err, research.ErrNoChannels), errors.Is(err, research.ErrUnavailable):
		return err
	}

	_, message := labFailure(t.logger, err)

	return errors.New(message)
}

// ---- reports ----

type draftReportInput struct {
	Lab     int64  `json:"lab" jsonschema:"the lab's id, from list_labs"`
	Request string `json:"request" jsonschema:"what the report is to be about"`
}

type plannedSection struct {
	Heading string `json:"heading" jsonschema:"the section's heading"`
	Focus   string `json:"focus" jsonschema:"what it covers, in the words the lab's pages would use: the section's search looks for this"`
}

type outlineView struct {
	Title    string           `json:"title"`
	Sections []plannedSection `json:"sections"`
}

func (t *mcpTools) draftReport(ctx context.Context, user auth.User, _ *mcp.CallToolRequest, in draftReportInput) (outlineView, error) {
	outline, err := t.draft(ctx, user, in.Lab, in.Request)
	if err != nil {
		return outlineView{}, err
	}

	out := outlineView{Title: outline.Title, Sections: make([]plannedSection, len(outline.Sections))}
	for i, s := range outline.Sections {
		out.Sections[i] = plannedSection{Heading: s.Heading, Focus: s.Focus}
	}

	return out, nil
}

func (t *mcpTools) draft(ctx context.Context, user auth.User, lab int64, request string) (artifacts.Outline, error) {
	if t.artifacts.service == nil {
		return artifacts.Outline{}, artifacts.ErrUnavailable
	}

	ctx, cancel := context.WithTimeout(ctx, t.artifacts.draftTimeout)
	defer cancel()

	outline, err := t.artifacts.service.Draft(ctx, user.ID, lab, request)
	if err != nil {
		return artifacts.Outline{}, t.reportFailed(err)
	}

	return outline, nil
}

type writeReportInput struct {
	Lab      int64            `json:"lab" jsonschema:"the lab's id, from list_labs"`
	Request  string           `json:"request" jsonschema:"what the report is to be about"`
	Title    string           `json:"title,omitempty" jsonschema:"the report's title, with sections; drafted when there are none"`
	Sections []plannedSection `json:"sections,omitempty" jsonschema:"the sections to write, in order, as draft_report gave them or changed; drafted when none"`
}

func (t *mcpTools) writeReport(ctx context.Context, user auth.User, _ *mcp.CallToolRequest, in writeReportInput) (reportView, error) {
	outline := artifacts.Outline{Title: in.Title}
	for _, s := range in.Sections {
		outline.Sections = append(outline.Sections, artifacts.Planned{Heading: s.Heading, Focus: s.Focus})
	}

	if len(outline.Sections) == 0 {
		drafted, err := t.draft(ctx, user, in.Lab, in.Request)
		if err != nil {
			return reportView{}, err
		}

		outline = drafted
	}

	ctx, cancel := context.WithTimeout(ctx, artifactsTimeout)
	defer cancel()

	a, err := t.artifacts.service.Build(ctx, user.ID, in.Lab, in.Request, outline)
	if err != nil {
		return reportView{}, t.reportFailed(err)
	}

	return reportOf(a), nil
}

type readReportInput struct {
	Report  int64 `json:"report" jsonschema:"the report's id, from get_lab or write_report"`
	Version int   `json:"version,omitempty" jsonschema:"an earlier version's number; the latest by default"`
}

func (t *mcpTools) readReport(ctx context.Context, user auth.User, _ *mcp.CallToolRequest, in readReportInput) (string, error) {
	if t.artifacts.service == nil {
		return "", artifacts.ErrUnavailable
	}

	ctx, cancel := context.WithTimeout(ctx, artifactsTimeout)
	defer cancel()

	a, v, _, err := t.artifacts.service.Get(ctx, user.ID, in.Report, in.Version)
	if err != nil {
		return "", t.reportFailed(err)
	}

	return reportText(a, v), nil
}

// reportText is the report's Markdown; one still being written says how far
// it has come, and shows the sections written so far.
func reportText(a artifacts.Artifact, v artifacts.Version) string {
	switch v.Status {
	case artifacts.StatusDone:
		if v.Markdown != "" {
			return v.Markdown
		}
	case artifacts.StatusFailed:
		return fmt.Sprintf("Version %d of %q failed: %s\n", v.Number, a.Title, v.Error)
	}

	done := 0
	sections := slices.Clone(v.Sections)

	for i := range sections {
		switch sections[i].State {
		case artifacts.StateDone, artifacts.StateThin, artifacts.StateFailed:
			done++
		default:
			sections[i].Note = "still being written."
		}
	}

	v.Sections = sections

	return fmt.Sprintf("Still being written: %d of %d sections done. Read it again later for the rest.\n\n", done, len(sections)) +
		artifacts.Markdown(a, v)
}

type saveAnswerInput struct {
	Lab    int64  `json:"lab" jsonschema:"the lab's id, from list_labs"`
	Answer string `json:"answer_id" jsonschema:"the answer id ask returned"`
}

func (t *mcpTools) saveAnswer(ctx context.Context, user auth.User, _ *mcp.CallToolRequest, in saveAnswerInput) (reportView, error) {
	if t.artifacts.service == nil {
		return reportView{}, artifacts.ErrUnavailable
	}

	kept, ok := t.artifacts.recent.get(user.ID, in.Answer)

	switch {
	case !ok:
		return reportView{}, errors.New("that answer is no longer kept: ask again")
	case !kept.checked:
		return reportView{}, errors.New("the answer is still being checked")
	}

	ctx, cancel := context.WithTimeout(ctx, artifactsTimeout)
	defer cancel()

	a, err := t.artifacts.service.SaveAnswer(ctx, user.ID, in.Lab, artifacts.SavedAnswer{
		Question: kept.query, Text: kept.res.Answer, Sources: kept.res.Sources, Claims: kept.claims, Model: kept.res.Model,
	})
	if err != nil {
		return reportView{}, t.reportFailed(err)
	}

	return reportOf(a), nil
}

func (t *mcpTools) reportFailed(err error) error {
	_, message := t.artifacts.failure(err)
	return errors.New(message)
}
