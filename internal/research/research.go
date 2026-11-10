// Package research maps what exists on a lab's topic: in rounds it finds
// works through independent channels - web search, OpenAlex search, the
// works a kept one cites, the works citing it - has the model judge each by
// what it shows, and reads the kept ones into the lab. Niche works count as
// much as famous ones; junk is dropped with a reason, never hidden.
package research

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"web-crawler/internal/labs"
	"web-crawler/internal/llm"
	"web-crawler/internal/openalex"
	"web-crawler/internal/urlnorm"
	"web-crawler/internal/websearch"

	"go.uber.org/zap"
)

var (
	ErrRunning     = errors.New("the lab is already being researched")
	ErrUnavailable = errors.New("research needs a chat model, and web search or OpenAlex")
	ErrNoChannels  = errors.New("tick search engines for the lab first: research has nowhere else to search")
	ErrNotFound    = errors.New("the lab has not been researched yet")

	ErrWorkNotFound     = errors.New("no such work in this lab's map")
	ErrSubtopicNotFound = errors.New("no such subtopic in this lab")
	ErrNothingToFollow  = errors.New("this work's citations cannot be followed: OpenAlex does not know it, and it has not been read")
)

const (
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// Why a run ended.
const (
	StopSaturated   = "saturated"
	StopBudget      = "budget"
	StopRounds      = "rounds"
	StopTime        = "time"
	StopInterrupted = "interrupted"
	// A run following one work's citations ends when it has.
	StopFollowed = "followed"
)

const (
	KindPaper = "paper"
	KindWeb   = "web"
)

// Channels a work is found through. Search and citations are independent
// enough to estimate from their overlap how much is still unfound.
const (
	ChannelSeed      = "seed"
	ChannelSearch    = "search"
	ChannelReference = "reference"
	ChannelCiting    = "citing"
)

const (
	VerdictPending = "pending"
	VerdictKeep    = "keep"
	VerdictMaybe   = "maybe"
	VerdictDrop    = "drop"
)

// How far a work read into the lab has come.
const (
	ReadReading  = "reading"
	ReadIn       = "in"
	ReadSetAside = "set_aside"
	// The site refused it: robots.txt forbids it, or it answered 401, 403,
	// 429 or 451.
	ReadBlocked = "blocked"
	// The page is no more: 404 or 410. Its site may serve others well.
	ReadGone = "gone"
	// Fetched, but with no text to index: a page built by scripts in the
	// browser, or an empty one.
	ReadEmpty  = "empty"
	ReadFailed = "failed"
	// Read and indexed, but its text is the same, or nearly, as a page the
	// lab held already: a mirror, a reprint, another URL for one paper.
	ReadCopy = "copy"
)

// unread says a read came to nothing, through no fault of the work: the
// site, not the reviewer, kept it out.
func unread(state string) bool {
	return state == ReadBlocked || state == ReadEmpty || state == ReadFailed || state == ReadGone
}

type Budget struct {
	// Works judged in a run, and works read.
	Found int `json:"found"`
	Read  int `json:"read"`
}

// Ask is what a run is asked to do. A nil budget takes the configured one;
// a read budget of 0 maps without reading.
type Ask struct {
	Request string
	Found   *int
	Read    *int
	Focus   Focus
}

// Focus narrows a run: one work's citations followed, both ways, or one
// subtopic researched further. Zero is the whole topic.
type Focus struct {
	Work     int64 `json:"work,omitempty"`
	Subtopic int64 `json:"subtopic,omitempty"`
}

type Run struct {
	ID         int64      `json:"id"`
	LabID      int64      `json:"lab_id"`
	Request    string     `json:"request"`
	Budget     Budget     `json:"budget"`
	Focus      Focus      `json:"focus"`
	Status     string     `json:"status"`
	Stopped    string     `json:"stopped,omitempty"`
	Rounds     []Round    `json:"rounds"`
	Error      string     `json:"error,omitempty"`
	Model      string     `json:"model"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Round is what one round of a run did.
type Round struct {
	N       int      `json:"n"`
	Queries []string `json:"queries,omitempty"`
	// New works found, by channel.
	Found  map[string]int `json:"found"`
	Judged int            `json:"judged"`
	Kept   int            `json:"kept"`
	Read   int            `json:"read"`
	// Of those read, how many their sites kept out: their budget went back to
	// later reads.
	Unread int `json:"unread,omitempty"`
	// Of those read, how many held text the lab had already: their budget
	// went back too.
	Copies int `json:"copies,omitempty"`
	// What did not answer, and why.
	Failed map[string]string `json:"failed,omitempty"`
}

type Subtopic struct {
	ID      int64    `json:"id"`
	Name    string   `json:"name"`
	Focus   string   `json:"focus"`
	Queries []string `json:"queries"`
}

type Work struct {
	ID         int64  `json:"id"`
	URL        string `json:"url"`
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	Snippet    string `json:"snippet,omitempty"`
	Site       string `json:"site,omitempty"`
	Year       int    `json:"year,omitempty"`
	DOI        string `json:"doi,omitempty"`
	OpenAlexID string `json:"openalex_id,omitempty"`
	// Shown, never used to judge.
	CitedBy    *int   `json:"cited_by,omitempty"`
	SubtopicID int64  `json:"subtopic_id,omitempty"`
	Verdict    string `json:"verdict"`
	Reason     string `json:"reason,omitempty"`
	ByUser     bool   `json:"by_user,omitempty"`
	SourceID   int64  `json:"source_id,omitempty"`
	// Set once it is read: reading, in, set_aside or failed.
	Read string `json:"read,omitempty"`
	// For a copy: the page of the lab whose text it repeats.
	CopyOf string `json:"copy_of,omitempty"`
	Finds  []Find `json:"finds"`

	ReferencesDone bool `json:"-"`
	CitingDone     bool `json:"-"`
}

// Find is one way a work was found.
type Find struct {
	Channel string `json:"channel"`
	Query   string `json:"query,omitempty"`
	// The work it was found through: the one citing it, or the one it cites.
	Via   int64 `json:"via,omitempty"`
	Round int   `json:"round"`
	// The run that found it this way.
	Run int64 `json:"run,omitempty"`
}

// Link is a work of the lab citing another.
type Link struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// Reference is a link or citation in a page a work's source brought in.
type Reference struct {
	Work  int64
	URL   string
	Label string
}

// Map is a lab's research: its subtopics, every work found, how they cite
// one another, and how complete it looks.
type Map struct {
	Run       *Run       `json:"run,omitempty"`
	Subtopics []Subtopic `json:"subtopics"`
	Works     []Work     `json:"works"`
	Links     []Link     `json:"links"`
	Coverage  Coverage   `json:"coverage"`
	// What a run takes when the user does not say, and at most.
	Budget    Budget `json:"budget"`
	MaxBudget Budget `json:"max_budget"`
}

type Labs interface {
	Get(ctx context.Context, ownerID, labID int64) (labs.Detail, error)
	AddResearched(ctx context.Context, ownerID, labID int64, r labs.Researched) (labs.Source, error)
}

type Search interface {
	Search(ctx context.Context, query string, engines []string) (websearch.Response, error)
}

// Scholar is OpenAlex.
type Scholar interface {
	Search(ctx context.Context, query string, n int) ([]openalex.Work, error)
	Citing(ctx context.Context, id string, n int) ([]openalex.Work, error)
	Works(ctx context.Context, ids []string) ([]openalex.Work, error)
	ByDOI(ctx context.Context, doi string) (*openalex.Work, error)
}

type Store interface {
	StartRun(ctx context.Context, labID int64, request string, budget Budget, focus Focus, model string) (int64, error)
	SaveRounds(ctx context.Context, id int64, rounds []Round) error
	FinishRun(ctx context.Context, id int64, status, stopped, errText string) error
	Run(ctx context.Context, id int64) (Run, error)
	LatestRun(ctx context.Context, labID int64) (Run, error)
	// Interrupt ends the runs a process that is gone left running.
	Interrupt(ctx context.Context) (int64, error)

	Subtopics(ctx context.Context, labID int64) ([]Subtopic, error)
	AddSubtopics(ctx context.Context, labID int64, subs []Subtopic) ([]Subtopic, error)

	// Find records a find of w, adding w when the lab has no work of its URL,
	// DOI or OpenAlex ID; added says it was new.
	Find(ctx context.Context, labID, runID int64, w Work, f Find) (id int64, added bool, err error)
	Works(ctx context.Context, labID int64) ([]Work, error)
	// Judge leaves a verdict the user gave alone.
	Judge(ctx context.Context, workID int64, verdict, reason string, subtopicID int64) error
	// SetVerdict is the user's: it stands over the model's, now and later.
	SetVerdict(ctx context.Context, labID, workID int64, verdict string, subtopicID int64) error
	SetSource(ctx context.Context, workID, sourceID int64) error
	Expanded(ctx context.Context, workID int64, references, citing bool) error
	Link(ctx context.Context, labID int64, l Link) error
	Links(ctx context.Context, labID int64) ([]Link, error)
	// ReferencesOf is what the pages the works were read into cite, but for
	// pages set aside on review.
	ReferencesOf(ctx context.Context, workIDs []int64) ([]Reference, error)
}

type Config struct {
	Budget    Budget
	MaxBudget Budget
	Rounds    int
	// Works shown to the model at once to judge.
	JudgeBatch int
	// Results taken per web query and per OpenAlex query.
	SearchResults int
	PaperResults  int
	// References and citing works taken per work followed.
	PerWork   int
	MaxTokens int
	// How long a round waits for its works to be read before the next.
	RoundWait time.Duration
	Timeout   time.Duration
	Poll      time.Duration
	// The crawler reads PDFs: a paper is then read from its open-access copy.
	Documents bool
}

type Service struct {
	logger  *zap.SugaredLogger
	store   Store
	labs    Labs
	search  Search
	scholar Scholar
	chat    llm.Client
	cfg     Config

	ctx     context.Context
	cancel  context.CancelFunc
	running sync.WaitGroup
}

// NewService takes a nil search or scholar when there is none; with neither,
// or no chat model, research says it is unavailable.
func NewService(logger *zap.SugaredLogger, store Store, labsService Labs, search Search, scholar Scholar, chat llm.Client, cfg Config) *Service {
	ctx, cancel := context.WithCancel(context.Background())

	return &Service{logger: logger, store: store, labs: labsService, search: search, scholar: scholar, chat: chat, cfg: cfg, ctx: ctx, cancel: cancel}
}

const maxRequestRunes = 1000

// Start researches the lab in the background and returns at once.
func (s *Service) Start(ctx context.Context, ownerID, labID int64, ask Ask) (Run, error) {
	if s.chat == nil || (s.search == nil && s.scholar == nil) {
		return Run{}, ErrUnavailable
	}

	lab, err := s.labs.Get(ctx, ownerID, labID)
	if err != nil {
		return Run{}, err
	}

	if err := s.checkFocus(ctx, lab, ask.Focus); err != nil {
		return Run{}, err
	}

	// Following one work's citations searches nothing.
	if ask.Focus.Work == 0 && s.scholar == nil && len(lab.Engines) == 0 {
		return Run{}, ErrNoChannels
	}

	request := strings.TrimSpace(ask.Request)
	if utf8.RuneCountInString(request) > maxRequestRunes {
		return Run{}, fmt.Errorf("%w: a request is at most %d characters", labs.ErrInvalid, maxRequestRunes)
	}

	budget := s.cfg.Budget
	if ask.Found != nil {
		budget.Found = *ask.Found
	}

	if ask.Read != nil {
		budget.Read = *ask.Read
	}

	if budget.Found < 1 || budget.Found > s.cfg.MaxBudget.Found || budget.Read < 0 || budget.Read > s.cfg.MaxBudget.Read {
		return Run{}, fmt.Errorf("%w: a run judges 1 to %d works and reads 0 to %d", labs.ErrInvalid, s.cfg.MaxBudget.Found, s.cfg.MaxBudget.Read)
	}

	id, err := s.store.StartRun(ctx, labID, request, budget, ask.Focus, llm.ModelOf(ctx, s.chat))
	if err != nil {
		return Run{}, err
	}

	s.running.Add(1)

	go func() {
		defer s.running.Done()
		s.run(id, ownerID, lab, request, budget, ask.Focus)
	}()

	return s.store.Run(ctx, id)
}

// checkFocus says whether a run can be narrowed so: to a work of the lab with
// citations to follow, or to one of its subtopics.
func (s *Service) checkFocus(ctx context.Context, lab labs.Detail, f Focus) error {
	switch {
	case f.Work != 0 && f.Subtopic != 0:
		return fmt.Errorf("%w: follow a work or research a subtopic, not both", labs.ErrInvalid)
	case f.Work != 0:
		w, err := s.work(ctx, lab.ID, f.Work)
		if err != nil {
			return err
		}

		if (s.scholar == nil || w.OpenAlexID == "") && w.Read != ReadIn {
			return ErrNothingToFollow
		}
	case f.Subtopic != 0:
		subs, err := s.store.Subtopics(ctx, lab.ID)
		if err != nil {
			return err
		}

		if !slices.ContainsFunc(subs, func(sub Subtopic) bool { return sub.ID == f.Subtopic }) {
			return ErrSubtopicNotFound
		}
	}

	return nil
}

func (s *Service) work(ctx context.Context, labID, workID int64) (Work, error) {
	works, err := s.store.Works(ctx, labID)
	if err != nil {
		return Work{}, err
	}

	for _, w := range works {
		if w.ID == workID {
			return w, nil
		}
	}

	return Work{}, ErrWorkNotFound
}

// SetVerdict is the user keeping, dropping or putting aside a work, and maybe
// moving it to another subtopic: research never overrules it.
func (s *Service) SetVerdict(ctx context.Context, ownerID, labID, workID int64, verdict string, subtopicID int64) (Work, error) {
	if verdict != VerdictKeep && verdict != VerdictMaybe && verdict != VerdictDrop {
		return Work{}, fmt.Errorf("%w: a verdict is keep, maybe or drop", labs.ErrInvalid)
	}

	if _, err := s.labs.Get(ctx, ownerID, labID); err != nil {
		return Work{}, err
	}

	if subtopicID != 0 {
		subs, err := s.store.Subtopics(ctx, labID)
		if err != nil {
			return Work{}, err
		}

		if !slices.ContainsFunc(subs, func(sub Subtopic) bool { return sub.ID == subtopicID }) {
			return Work{}, ErrSubtopicNotFound
		}
	}

	if err := s.store.SetVerdict(ctx, labID, workID, verdict, subtopicID); err != nil {
		return Work{}, err
	}

	return s.work(ctx, labID, workID)
}

// ReadWork reads a work into the lab now, whatever research would choose: the
// user picked it, so it is kept as theirs too.
func (s *Service) ReadWork(ctx context.Context, ownerID, labID, workID int64) (Work, error) {
	if _, err := s.labs.Get(ctx, ownerID, labID); err != nil {
		return Work{}, err
	}

	w, err := s.work(ctx, labID, workID)

	switch {
	case err != nil:
		return Work{}, err
	case w.SourceID != 0:
		return w, nil
	case !readable(w):
		return Work{}, fmt.Errorf("%w: this work has no address to read it from", labs.ErrInvalid)
	}

	src, err := s.labs.AddResearched(ctx, ownerID, labID, labs.Researched{URL: w.URL, Reason: w.Reason})
	if err != nil {
		return Work{}, err
	}

	if err := s.store.SetSource(ctx, workID, src.ID); err != nil {
		return Work{}, err
	}

	if err := s.store.SetVerdict(ctx, labID, workID, VerdictKeep, 0); err != nil {
		return Work{}, err
	}

	return s.work(ctx, labID, workID)
}

// Map is the lab's research as it stands, while a run goes on too.
func (s *Service) Map(ctx context.Context, ownerID, labID int64) (Map, error) {
	if _, err := s.labs.Get(ctx, ownerID, labID); err != nil {
		return Map{}, err
	}

	m := Map{Budget: s.cfg.Budget, MaxBudget: s.cfg.MaxBudget}

	run, err := s.store.LatestRun(ctx, labID)

	switch {
	case err == nil:
		m.Run = &run
	case !errors.Is(err, ErrNotFound):
		return Map{}, err
	}

	if m.Subtopics, err = s.store.Subtopics(ctx, labID); err != nil {
		return Map{}, err
	}

	if m.Works, err = s.store.Works(ctx, labID); err != nil {
		return Map{}, err
	}

	if m.Links, err = s.store.Links(ctx, labID); err != nil {
		return Map{}, err
	}

	m.Coverage = coverageOf(m.Subtopics, m.Works)

	return m, nil
}

// Recover ends the runs a previous process left running.
func (s *Service) Recover(ctx context.Context) {
	n, err := s.store.Interrupt(ctx)
	if err != nil {
		s.logger.Warnw("Failed to mark interrupted research", "error", err)
		return
	}

	if n > 0 {
		s.logger.Infow("Marked research interrupted by a restart", "count", n)
	}
}

// Shutdown stops running research; what it found and read stays.
func (s *Service) Shutdown(ctx context.Context) error {
	s.cancel()

	finished := make(chan struct{})
	go func() {
		s.running.Wait()
		close(finished)
	}()

	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var arxivURL = regexp.MustCompile(`(?i)^https?://(?:www\.|export\.)?arxiv\.org/(?:abs|pdf)/(\d{4}\.\d{4,5})(?:v\d+)?(?:\.pdf)?/?$`)

// doiOf is the DOI a URL names: a doi.org link, or an arXiv paper, whose DOI
// arXiv assigns. OpenAlex knows a paper by either.
func doiOf(u string) string {
	if d := openalex.BareDOI(u); d != "" {
		return d
	}

	if m := arxivURL.FindStringSubmatch(u); m != nil {
		return "10.48550/arxiv." + m[1]
	}

	return ""
}

// paper is a work as OpenAlex knows it, at the address it is best read from:
// its open-access PDF when the crawler reads PDFs, else its open copy's
// landing page, else the publisher's.
func (s *Service) paper(w openalex.Work) Work {
	out := Work{Kind: KindPaper, Title: w.Title, Snippet: w.Abstract, Site: w.Venue, Year: w.Year, DOI: w.DOI, OpenAlexID: w.ID}

	if w.CitedBy > 0 {
		out.CitedBy = &w.CitedBy
	}

	// The open copy before the publisher's page, which often refuses crawlers.
	candidates := []string{w.OALandingURL, w.LandingURL}
	if s.cfg.Documents {
		candidates = append([]string{w.PDFURL}, candidates...)
	}

	if w.DOI != "" {
		candidates = append(candidates, "https://doi.org/"+w.DOI)
	}

	candidates = append(candidates, "https://openalex.org/"+w.ID)

	for _, c := range candidates {
		if u, ok := urlnorm.Canonical(c, nil); ok && c != "" {
			out.URL = u
			break
		}
	}

	if out.Site == "" {
		out.Site = hostOf(out.URL)
	}

	return out
}

// readable says whether reading the work's address gives more than its
// record: an OpenAlex page holds nothing the lab could answer from.
func readable(w Work) bool {
	return w.URL != "" && hostOf(w.URL) != "openalex.org"
}

func hostOf(u string) string {
	rest := u[strings.Index(u, "//")+2:]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}

	return strings.TrimPrefix(rest, "www.")
}

// skippable is what is never a work: files no one reads, share buttons,
// sign-ins. Judging them would spend the model on nothing.
func skippable(u string) bool {
	lower := strings.ToLower(u)

	for _, ext := range []string{".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".mp4", ".mp3", ".zip", ".gz", ".tar", ".exe", ".dmg", ".css", ".js"} {
		if strings.HasSuffix(strings.SplitN(lower, "?", 2)[0], ext) {
			return true
		}
	}

	for _, part := range []string{"/sharer", "/intent/tweet", "/share?", "/login", "/signin", "/sign-in", "/signup", "/cart", "mailto:"} {
		if strings.Contains(lower, part) {
			return true
		}
	}

	return false
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func cut(s string, limit int) string {
	s = oneLine(s)
	if utf8.RuneCountInString(s) <= limit {
		return s
	}

	return string([]rune(s)[:limit]) + "…"
}
