// Package artifacts keeps the longer works the model builds inside a lab -
// research reports, saved answers. Each is Markdown with every claim cited to
// a document, versioned, and read-only for its owner, who can only ask for it
// to be rebuilt: what an artifact says always traces to a source, never to an
// edit. Answers never cite artifacts.
package artifacts

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
	"web-crawler/internal/answer"
	"web-crawler/internal/labs"
	"web-crawler/internal/llm"

	"go.uber.org/zap"
)

var (
	ErrNotFound    = errors.New("no such artifact")
	ErrInvalid     = errors.New("invalid artifact request")
	ErrBuilding    = errors.New("this artifact is being built")
	ErrUnavailable = errors.New("artifacts need a chat model, and there is none")
)

type Kind string

const (
	KindReport Kind = "report"
	KindAnswer Kind = "answer"
)

type Status string

const (
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
)

// State is how far a section of a build has come.
type State string

const (
	StatePending  State = "pending"
	StateWriting  State = "writing"
	StateChecking State = "checking"
	StateDone     State = "done"
	// The lab holds too little for it: marked, never filled from memory.
	StateThin   State = "thin"
	StateFailed State = "failed"
)

type Artifact struct {
	ID        int64     `json:"id"`
	LabID     int64     `json:"lab_id"`
	Kind      Kind      `json:"kind"`
	Title     string    `json:"title"`
	Request   string    `json:"request"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// The newest version, without its text.
	Latest *Version `json:"latest,omitempty"`
}

// Outline is what a report is to cover, as the model drafted it and the user
// confirmed it: dropping and reordering sections steers, it writes nothing.
type Outline struct {
	Title    string    `json:"title"`
	Sections []Planned `json:"sections"`
	Model    string    `json:"model,omitempty"`
}

type Planned struct {
	Heading string `json:"heading"`
	// What the section covers, in words the documents would use: it is what
	// the section's search looks for.
	Focus string `json:"focus"`
}

type Version struct {
	Number   int       `json:"number"`
	Status   Status    `json:"status"`
	Outline  Outline   `json:"outline"`
	Sections []Section `json:"sections"`
	// Numbered across the whole artifact, as its sections cite them.
	Sources    []Source   `json:"sources"`
	Markdown   string     `json:"markdown,omitempty"`
	Error      string     `json:"error,omitempty"`
	Model      string     `json:"model"`
	Seconds    float64    `json:"seconds,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Section is a section as built.
type Section struct {
	Heading string `json:"heading"`
	Focus   string `json:"focus,omitempty"`
	State   State  `json:"state"`
	// Markdown citing Sources by their numbers.
	Text string `json:"text,omitempty"`
	// Each sentence of Text that cites sources, in order, with its verdict.
	Claims []answer.Claim `json:"claims,omitempty"`
	Note   string         `json:"note,omitempty"`
}

type Source struct {
	N     int    `json:"n"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

type Store interface {
	Create(ctx context.Context, ownerID, labID int64, kind Kind, title, request string) (int64, error)
	// StartVersion adds the next version of the artifact; ErrBuilding when one
	// is running.
	StartVersion(ctx context.Context, artifactID int64, v Version) (int, error)
	SaveVersion(ctx context.Context, artifactID int64, v Version) error
	List(ctx context.Context, ownerID, labID int64) ([]Artifact, error)
	Get(ctx context.Context, ownerID, artifactID int64) (Artifact, error)
	// Version 0 is the newest.
	Version(ctx context.Context, ownerID, artifactID int64, number int) (Version, error)
	Versions(ctx context.Context, ownerID, artifactID int64) ([]int, error)
	Delete(ctx context.Context, ownerID, artifactID int64) error
	Interrupt(ctx context.Context) (int64, error)
}

// Answers reads and writes from a lab's documents, and checks what was
// written: a section is a longer answer, from the lab alone.
type Answers interface {
	Read(ctx context.Context, req answer.Request) (answer.Result, error)
	Write(ctx context.Context, req answer.Request, res answer.Result, onDelta func(string) error) (answer.Result, error)
	Verify(ctx context.Context, res answer.Result, events answer.VerifyEvents) (answer.Verification, error)
	Verifying() bool
}

type Labs interface {
	Get(ctx context.Context, ownerID, labID int64) (labs.Detail, error)
	PageURLs(ctx context.Context, ownerID int64, labIDs []int64) ([]string, error)
	Pages(ctx context.Context, ownerID, labID int64, limit int) ([]labs.Page, error)
}

type Config struct {
	MaxSections   int
	SectionTokens int
	OutlineTokens int
	Timeout       time.Duration
	// Check each section's claims, when answers can be checked.
	Verify bool
}

type Service struct {
	logger  *zap.SugaredLogger
	store   Store
	labs    Labs
	answers Answers
	chat    llm.Client
	cfg     Config

	ctx     context.Context
	cancel  context.CancelFunc
	running sync.WaitGroup
}

// NewService takes nil answers or chat when there is no chat model: artifacts
// can then be read and deleted, not built.
func NewService(logger *zap.SugaredLogger, store Store, labsService Labs, answers Answers, chat llm.Client, cfg Config) *Service {
	ctx, cancel := context.WithCancel(context.Background())

	return &Service{logger: logger, store: store, labs: labsService, answers: answers, chat: chat, cfg: cfg, ctx: ctx, cancel: cancel}
}

const (
	maxRequestRunes = 1000
	maxTitleRunes   = 200
	maxHeadingRunes = 120
	maxFocusRunes   = 400
)

// Build checks the outline, records the report and builds it in the
// background; it returns at once.
func (s *Service) Build(ctx context.Context, ownerID, labID int64, request string, outline Outline) (Artifact, error) {
	if s.answers == nil || s.chat == nil {
		return Artifact{}, ErrUnavailable
	}

	request, err := clean(request, maxRequestRunes, "a request")
	if err != nil {
		return Artifact{}, err
	}

	if outline, err = s.checkOutline(outline); err != nil {
		return Artifact{}, err
	}

	if _, err := s.labs.Get(ctx, ownerID, labID); err != nil {
		return Artifact{}, err
	}

	id, err := s.store.Create(ctx, ownerID, labID, KindReport, outline.Title, request)
	if err != nil {
		return Artifact{}, err
	}

	if err := s.start(ctx, ownerID, labID, id, fresh(outline)); err != nil {
		return Artifact{}, err
	}

	return s.store.Get(ctx, ownerID, id)
}

// Rebuild builds a report again from its outline, as a new version: the lab
// may hold more by now.
func (s *Service) Rebuild(ctx context.Context, ownerID, artifactID int64) (Artifact, error) {
	return s.change(ctx, ownerID, artifactID, func(latest Version) (Version, error) {
		return fresh(latest.Outline), nil
	})
}

// Rewrite writes one section again (numbered from 1), as a new version, the
// others kept as they are: after gathering for a section the lab held too
// little on, say.
func (s *Service) Rewrite(ctx context.Context, ownerID, artifactID int64, section int) (Artifact, error) {
	return s.change(ctx, ownerID, artifactID, func(latest Version) (Version, error) {
		if section < 1 || section > len(latest.Sections) {
			return Version{}, fmt.Errorf("%w: the report has sections 1 to %d", ErrInvalid, len(latest.Sections))
		}

		v := carried(latest)
		old := v.Sections[section-1]
		v.Sections[section-1] = Section{Heading: old.Heading, Focus: old.Focus, State: StatePending}

		return v, nil
	})
}

// Extend adds a section at the end and writes it alone, as a new version: a
// report is extended by asking, never by editing it.
func (s *Service) Extend(ctx context.Context, ownerID, artifactID int64, planned Planned) (Artifact, error) {
	heading, err := clean(planned.Heading, maxHeadingRunes, "a heading")
	if err != nil || heading == "" {
		return Artifact{}, fmt.Errorf("%w: a section needs a heading of at most %d characters", ErrInvalid, maxHeadingRunes)
	}

	focus, err := clean(planned.Focus, maxFocusRunes, "a focus")
	if err != nil {
		return Artifact{}, err
	}

	if focus == "" {
		focus = heading
	}

	planned = Planned{Heading: oneLine(heading), Focus: oneLine(focus)}

	return s.change(ctx, ownerID, artifactID, func(latest Version) (Version, error) {
		if len(latest.Sections) >= s.cfg.MaxSections {
			return Version{}, fmt.Errorf("%w: a report has at most %d sections", ErrInvalid, s.cfg.MaxSections)
		}

		v := carried(latest)
		v.Outline.Sections = append(v.Outline.Sections, planned)
		v.Sections = append(v.Sections, Section{Heading: planned.Heading, Focus: planned.Focus, State: StatePending})

		return v, nil
	})
}

// change starts a new version of a report, made from its newest by next.
func (s *Service) change(ctx context.Context, ownerID, artifactID int64, next func(Version) (Version, error)) (Artifact, error) {
	if s.answers == nil || s.chat == nil {
		return Artifact{}, ErrUnavailable
	}

	a, err := s.store.Get(ctx, ownerID, artifactID)
	if err != nil {
		return Artifact{}, err
	}

	if a.Kind != KindReport {
		return Artifact{}, fmt.Errorf("%w: only a report is rebuilt or extended", ErrInvalid)
	}

	latest, err := s.store.Version(ctx, ownerID, artifactID, 0)
	if err != nil {
		return Artifact{}, err
	}

	if latest.Status == StatusRunning {
		return Artifact{}, ErrBuilding
	}

	v, err := next(latest)
	if err != nil {
		return Artifact{}, err
	}

	if err := s.start(ctx, ownerID, a.LabID, artifactID, v); err != nil {
		return Artifact{}, err
	}

	return s.store.Get(ctx, ownerID, artifactID)
}

// fresh is a version to write in full from an outline.
func fresh(outline Outline) Version {
	v := Version{Outline: outline, Sources: []Source{}}
	for _, p := range outline.Sections {
		v.Sections = append(v.Sections, Section{Heading: p.Heading, Focus: p.Focus, State: StatePending})
	}

	return v
}

// carried is the newest version's sections and sources, to change one part of.
func carried(latest Version) Version {
	v := Version{Outline: latest.Outline, Sources: slices.Clone(latest.Sources), Sections: make([]Section, len(latest.Sections))}
	v.Outline.Sections = slices.Clone(latest.Outline.Sections)

	for i, sec := range latest.Sections {
		sec.Claims = slices.Clone(sec.Claims)
		v.Sections[i] = sec
	}

	return v
}

// start records v as the next version and writes its pending sections in the
// background.
func (s *Service) start(ctx context.Context, ownerID, labID, artifactID int64, v Version) error {
	v.Status, v.Model, v.Markdown, v.Error, v.Seconds, v.FinishedAt = StatusRunning, s.chat.Model(), "", "", 0, nil

	number, err := s.store.StartVersion(ctx, artifactID, v)
	if err != nil {
		return err
	}

	v.Number = number

	s.running.Add(1)

	go func() {
		defer s.running.Done()
		s.build(ownerID, labID, artifactID, v)
	}()

	return nil
}

func (s *Service) List(ctx context.Context, ownerID, labID int64) ([]Artifact, error) {
	if _, err := s.labs.Get(ctx, ownerID, labID); err != nil {
		return nil, err
	}

	return s.store.List(ctx, ownerID, labID)
}

// Get returns the artifact, one of its versions - 0 for the newest - and the
// numbers of all of them.
func (s *Service) Get(ctx context.Context, ownerID, artifactID int64, number int) (Artifact, Version, []int, error) {
	a, err := s.store.Get(ctx, ownerID, artifactID)
	if err != nil {
		return Artifact{}, Version{}, nil, err
	}

	v, err := s.store.Version(ctx, ownerID, artifactID, number)
	if err != nil {
		return Artifact{}, Version{}, nil, err
	}

	numbers, err := s.store.Versions(ctx, ownerID, artifactID)

	return a, v, numbers, err
}

func (s *Service) Delete(ctx context.Context, ownerID, artifactID int64) error {
	return s.store.Delete(ctx, ownerID, artifactID)
}

// SavedAnswer is an answer as the server produced it - never as a client
// sent it back, or anyone could write into an artifact.
type SavedAnswer struct {
	Question string
	Text     string
	Sources  []answer.Source
	Claims   []answer.Claim
	Model    string
}

// SaveAnswer keeps an answer, with its check, as an artifact of the lab.
func (s *Service) SaveAnswer(ctx context.Context, ownerID, labID int64, saved SavedAnswer) (Artifact, error) {
	question, err := clean(saved.Question, maxRequestRunes, "a question")
	if err != nil || question == "" || strings.TrimSpace(saved.Text) == "" {
		return Artifact{}, fmt.Errorf("%w: nothing to save", ErrInvalid)
	}

	if _, err := s.labs.Get(ctx, ownerID, labID); err != nil {
		return Artifact{}, err
	}

	title := cut(oneLine(question), maxTitleRunes)

	id, err := s.store.Create(ctx, ownerID, labID, KindAnswer, title, question)
	if err != nil {
		return Artifact{}, err
	}

	now := time.Now()
	v := Version{
		Status:     StatusDone,
		Outline:    Outline{Title: title},
		Sections:   []Section{{State: StateDone, Text: strings.TrimSpace(saved.Text), Claims: saved.Claims}},
		Sources:    []Source{},
		Model:      saved.Model,
		FinishedAt: &now,
	}

	for _, src := range saved.Sources {
		v.Sources = append(v.Sources, Source{N: src.N, URL: src.URL, Title: src.Title})
	}

	v.Markdown = Markdown(Artifact{Kind: KindAnswer, Title: title}, v)

	if _, err := s.store.StartVersion(ctx, id, v); err != nil {
		return Artifact{}, err
	}

	return s.store.Get(ctx, ownerID, id)
}

// Recover marks builds a previous process left running as failed.
func (s *Service) Recover(ctx context.Context) {
	n, err := s.store.Interrupt(ctx)
	if err != nil {
		s.logger.Warnw("Failed to mark interrupted artifact builds", "error", err)
		return
	}

	if n > 0 {
		s.logger.Infow("Marked artifact builds interrupted by a restart", "count", n)
	}
}

// Shutdown stops running builds, which record themselves as failed.
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

func (s *Service) checkOutline(o Outline) (Outline, error) {
	title, err := clean(o.Title, maxTitleRunes, "a title")
	if err != nil || title == "" {
		return Outline{}, fmt.Errorf("%w: a report needs a title of at most %d characters", ErrInvalid, maxTitleRunes)
	}

	if len(o.Sections) == 0 || len(o.Sections) > s.cfg.MaxSections {
		return Outline{}, fmt.Errorf("%w: a report has 1 to %d sections", ErrInvalid, s.cfg.MaxSections)
	}

	out := Outline{Title: oneLine(title), Model: o.Model}

	for _, p := range o.Sections {
		heading, err := clean(p.Heading, maxHeadingRunes, "a heading")
		if err != nil || heading == "" {
			return Outline{}, fmt.Errorf("%w: each section needs a heading of at most %d characters", ErrInvalid, maxHeadingRunes)
		}

		focus, err := clean(p.Focus, maxFocusRunes, "a focus")
		if err != nil {
			return Outline{}, err
		}

		if focus == "" {
			focus = heading
		}

		out.Sections = append(out.Sections, Planned{Heading: oneLine(heading), Focus: oneLine(focus)})
	}

	return out, nil
}

func clean(s string, limit int, what string) (string, error) {
	s = strings.TrimSpace(s)

	if utf8.RuneCountInString(s) > limit || strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\n' }) {
		return "", fmt.Errorf("%w: %s is at most %d characters of text", ErrInvalid, what, limit)
	}

	return s, nil
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func cut(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}

	return string([]rune(s)[:limit])
}
