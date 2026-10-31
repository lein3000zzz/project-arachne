package labs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
	"web-crawler/internal/llm"
	"web-crawler/internal/runs"
)

type Store interface {
	Create(ctx context.Context, ownerID int64, name, topic string, engines []string) (Lab, error)
	SetEngines(ctx context.Context, ownerID, labID int64, engines []string) (Lab, error)
	Get(ctx context.Context, ownerID, labID int64) (Lab, error)
	List(ctx context.Context, ownerID int64) ([]Lab, error)
	Delete(ctx context.Context, ownerID, labID int64) error
	AddSource(ctx context.Context, ownerID, labID int64, src Source) (Source, error)
	Source(ctx context.Context, ownerID, labID, sourceID int64) (Source, error)
	Sources(ctx context.Context, ownerID, labID int64) ([]Source, error)
	RemoveSource(ctx context.Context, ownerID, labID, sourceID int64) error
	PageURLs(ctx context.Context, ownerID int64, labIDs []int64) ([]string, error)
	Pages(ctx context.Context, ownerID, labID int64, limit int) ([]Page, error)
	References(ctx context.Context, ownerID, labID int64) ([]Document, error)
}

type Runs interface {
	Prepare(req runs.Request) (runs.Request, error)
	Submit(ctx context.Context, req runs.Request, origin runs.Origin) (runs.Run, error)
	Get(ctx context.Context, id string) (runs.Run, error)
}

var (
	ErrCrawlingDisabled = errors.New("crawling is disabled (crawl_api.enabled), so labs cannot gather sources")
	ErrUploadsDisabled  = errors.New("uploading is not available: it needs crawling (crawl_api.enabled)")
)

const (
	maxNameRunes  = 80
	maxTopicRunes = 2000
)

type Limits struct {
	MaxSources int
	// What a new lab may search when its creator did not choose.
	DefaultEngines []string
	// Whether the crawler reads documents (documents.enabled): without, only
	// text files are worth uploading.
	Documents bool
}

// Uploads keeps uploaded files, under addresses no web page can have.
type Uploads interface {
	Save(ctx context.Context, ownerID int64, name, contentType string, data []byte) (url string, err error)
	// Forget deletes, of these addresses, the files no lab of the owner holds
	// any more, with their pages and chunks.
	Forget(ctx context.Context, ownerID int64, urls []string) error
}

// File is an uploaded file, as received.
type File struct {
	Name        string
	ContentType string
	Data        []byte
}

const (
	maxEngines     = 12
	maxEngineRunes = 40
)

// Found is a page the model found by searching and chose for a lab.
type Found struct {
	URL         string
	Query       string
	Engines     []string
	Reason      string
	GatheringID int64
}

type SourceRequest struct {
	URL   string `json:"url"`
	Scope string `json:"scope,omitempty"`
	Depth int    `json:"depth,omitempty"`
	Links int    `json:"links,omitempty"`
}

type SourceStatus struct {
	Source
	// The latest crawl, absent before the first.
	Run *runs.Run `json:"run,omitempty"`
}

type Detail struct {
	Lab
	Sources []SourceStatus `json:"sources"`
}

type Service struct {
	store   Store
	runs    Runs
	chat    llm.Client
	limits  Limits
	uploads Uploads
}

// NewService takes nil runs when crawling is off: labs can then be read and
// deleted, but gain no sources. Without a chat model, plans are drafted by
// reading the request literally.
func NewService(store Store, runs Runs, chat llm.Client, limits Limits) *Service {
	return &Service{store: store, runs: runs, chat: chat, limits: limits}
}

// KeepsUploads lets users upload files to their labs.
func (s *Service) KeepsUploads(u Uploads) *Service {
	s.uploads = u
	return s
}

func (s *Service) List(ctx context.Context, ownerID int64) ([]Lab, error) {
	return s.store.List(ctx, ownerID)
}

// Create makes the lab and starts crawling its sources. The sources are all
// checked first, so a bad one leaves no half-made lab behind. Nil engines take
// the defaults; an empty list means the lab never searches the web.
func (s *Service) Create(ctx context.Context, ownerID int64, name, topic string, sources []SourceRequest, engines []string) (Detail, error) {
	name, err := cleanName(name)
	if err != nil {
		return Detail{}, err
	}

	if engines == nil {
		engines = s.limits.DefaultEngines
	}

	if engines, err = cleanEngines(engines); err != nil {
		return Detail{}, err
	}

	topic, err = cleanTopic(topic)
	if err != nil {
		return Detail{}, err
	}

	prepared, err := s.prepare(sources, nil)
	if err != nil {
		return Detail{}, err
	}

	lab, err := s.store.Create(ctx, ownerID, name, topic, engines)
	if err != nil {
		return Detail{}, err
	}

	if err := s.addAll(ctx, ownerID, lab.ID, prepared); err != nil {
		return Detail{}, err
	}

	return s.Get(ctx, ownerID, lab.ID)
}

func (s *Service) SetEngines(ctx context.Context, ownerID, labID int64, engines []string) (Lab, error) {
	engines, err := cleanEngines(engines)
	if err != nil {
		return Lab{}, err
	}

	return s.store.SetEngines(ctx, ownerID, labID, engines)
}

// AddFound adds a page found by searching as a source of its own - that page
// alone, its links not followed - and starts crawling it.
func (s *Service) AddFound(ctx context.Context, ownerID, labID int64, f Found) (Source, error) {
	existing, err := s.store.Sources(ctx, ownerID, labID)
	if err != nil {
		return Source{}, err
	}

	prepared, err := s.prepare([]SourceRequest{{URL: f.URL, Scope: ScopePage}}, existing)
	if err != nil {
		return Source{}, err
	}

	src := prepared[0]
	src.Kind, src.Query, src.Engines, src.Reason, src.GatheringID = KindSearch, f.Query, f.Engines, f.Reason, f.GatheringID

	added, err := s.store.AddSource(ctx, ownerID, labID, src)
	if err != nil {
		return Source{}, err
	}

	_, err = s.submit(ctx, ownerID, added)

	return added, err
}

// References are what the lab's documents link to or cite.
func (s *Service) References(ctx context.Context, ownerID, labID int64) ([]Document, error) {
	return s.store.References(ctx, ownerID, labID)
}

// AddReferences adds references of the lab's documents as sources of their
// own - each page alone, its links not followed - and starts crawling them.
// Only what one of the lab's documents gives can be added this way.
func (s *Service) AddReferences(ctx context.Context, ownerID, labID int64, picks []Cited) (Detail, error) {
	if len(picks) == 0 {
		return Detail{}, fmt.Errorf("%w: pick at least one reference", ErrInvalid)
	}

	docs, err := s.store.References(ctx, ownerID, labID)
	if err != nil {
		return Detail{}, err
	}

	citations := map[Cited]string{}

	for _, doc := range docs {
		for _, ref := range doc.References {
			citations[Cited{Document: doc.URL, URL: ref.URL}] = ref.Label
		}
	}

	requests := make([]SourceRequest, len(picks))

	for i, pick := range picks {
		if _, ok := citations[pick]; !ok {
			return Detail{}, fmt.Errorf("%w: %s is not a reference of a document in this lab", ErrInvalid, pick.URL)
		}

		requests[i] = SourceRequest{URL: pick.URL, Scope: ScopePage}
	}

	existing, err := s.store.Sources(ctx, ownerID, labID)
	if err != nil {
		return Detail{}, err
	}

	prepared, err := s.prepare(requests, existing)
	if err != nil {
		return Detail{}, err
	}

	for i := range prepared {
		prepared[i].Kind, prepared[i].CitedBy, prepared[i].Citation = KindReference, picks[i].Document, citations[picks[i]]
	}

	if err := s.addAll(ctx, ownerID, labID, prepared); err != nil {
		return Detail{}, err
	}

	return s.Get(ctx, ownerID, labID)
}

// AddUploads keeps the files and adds each as a source of its own, crawled
// from the store rather than the web. A file the lab already holds is an
// error, like a link it has.
func (s *Service) AddUploads(ctx context.Context, ownerID, labID int64, files []File) (Detail, error) {
	if s.uploads == nil || s.runs == nil {
		return Detail{}, ErrUploadsDisabled
	}

	if len(files) == 0 {
		return Detail{}, fmt.Errorf("%w: no files", ErrInvalid)
	}

	existing, err := s.store.Sources(ctx, ownerID, labID)
	if err != nil {
		return Detail{}, err
	}

	if len(existing)+len(files) > s.limits.MaxSources {
		return Detail{}, fmt.Errorf("%w: a lab has at most %d sources", ErrInvalid, s.limits.MaxSources)
	}

	for _, f := range files {
		if len(f.Data) == 0 {
			return Detail{}, fmt.Errorf("%w: %s is empty", ErrInvalid, f.Name)
		}

		if !s.limits.Documents && !strings.HasPrefix(http.DetectContentType(f.Data), "text/") {
			return Detail{}, fmt.Errorf("%w: %s is not a text file, and reading documents is off (documents.enabled)", ErrInvalid, f.Name)
		}
	}

	seen := make(map[string]bool, len(existing))
	for _, src := range existing {
		seen[src.URL] = true
	}

	sources := make([]Source, 0, len(files))

	for _, f := range files {
		url, err := s.uploads.Save(ctx, ownerID, f.Name, f.ContentType, f.Data)
		if err != nil {
			return Detail{}, err
		}

		if seen[url] {
			return Detail{}, fmt.Errorf("%w: %s", ErrSourceExists, f.Name)
		}

		seen[url] = true
		sources = append(sources, Source{Kind: KindUpload, URL: url, Scope: ScopePage, MaxDepth: 1, MaxLinks: 1, Filename: f.Name})
	}

	if err := s.addAll(ctx, ownerID, labID, sources); err != nil {
		return Detail{}, err
	}

	return s.Get(ctx, ownerID, labID)
}

func (s *Service) AddSources(ctx context.Context, ownerID, labID int64, sources []SourceRequest) (Detail, error) {
	existing, err := s.store.Sources(ctx, ownerID, labID)
	if err != nil {
		return Detail{}, err
	}

	prepared, err := s.prepare(sources, existing)
	if err != nil {
		return Detail{}, err
	}

	if err := s.addAll(ctx, ownerID, labID, prepared); err != nil {
		return Detail{}, err
	}

	return s.Get(ctx, ownerID, labID)
}

// Refresh crawls a source again; pages it no longer reaches stay in the lab.
func (s *Service) Refresh(ctx context.Context, ownerID, labID, sourceID int64) (SourceStatus, error) {
	if s.runs == nil {
		return SourceStatus{}, ErrCrawlingDisabled
	}

	src, err := s.store.Source(ctx, ownerID, labID, sourceID)
	if err != nil {
		return SourceStatus{}, err
	}

	run, err := s.submit(ctx, ownerID, src)
	if err != nil {
		return SourceStatus{}, err
	}

	src.LastRunID = run.ID

	return SourceStatus{Source: src, Run: &run}, nil
}

func (s *Service) RemoveSource(ctx context.Context, ownerID, labID, sourceID int64) error {
	src, err := s.store.Source(ctx, ownerID, labID, sourceID)
	if err != nil {
		return err
	}

	if err := s.store.RemoveSource(ctx, ownerID, labID, sourceID); err != nil {
		return err
	}

	return s.forget(ctx, ownerID, []Source{src})
}

func (s *Service) Get(ctx context.Context, ownerID, labID int64) (Detail, error) {
	lab, err := s.store.Get(ctx, ownerID, labID)
	if err != nil {
		return Detail{}, err
	}

	sources, err := s.store.Sources(ctx, ownerID, labID)
	if err != nil {
		return Detail{}, err
	}

	detail := Detail{Lab: lab, Sources: make([]SourceStatus, len(sources))}

	for i, src := range sources {
		detail.Sources[i].Source = src

		if src.LastRunID == "" || s.runs == nil {
			continue
		}

		run, err := s.runs.Get(ctx, src.LastRunID)

		switch {
		case err == nil:
			detail.Sources[i].Run = &run
		case !errors.Is(err, runs.ErrNotFound):
			return Detail{}, err
		}
	}

	return detail, nil
}

func (s *Service) Delete(ctx context.Context, ownerID, labID int64) error {
	sources, err := s.store.Sources(ctx, ownerID, labID)
	if err != nil {
		return err
	}

	if err := s.store.Delete(ctx, ownerID, labID); err != nil {
		return err
	}

	return s.forget(ctx, ownerID, sources)
}

// forget deletes the uploaded files among these sources that the owner's
// labs no longer hold. A crawled page outlives its lab in the shared base; an
// uploaded file was never shared, so it goes.
func (s *Service) forget(ctx context.Context, ownerID int64, sources []Source) error {
	var urls []string

	for _, src := range sources {
		if src.Kind == KindUpload {
			urls = append(urls, src.URL)
		}
	}

	if len(urls) == 0 || s.uploads == nil {
		return nil
	}

	return s.uploads.Forget(ctx, ownerID, urls)
}

func (s *Service) Pages(ctx context.Context, ownerID, labID int64, limit int) ([]Page, error) {
	return s.store.Pages(ctx, ownerID, labID, limit)
}

// PageURLs returns the pages a search restricted to these labs may return.
func (s *Service) PageURLs(ctx context.Context, ownerID int64, labIDs []int64) ([]string, error) {
	ids := slices.Clone(labIDs)
	slices.Sort(ids)

	return s.store.PageURLs(ctx, ownerID, slices.Compact(ids))
}

// prepare checks every request against the crawl limits and fills in its
// defaults; a URL the lab has, or that comes twice, is an error.
func (s *Service) prepare(sources []SourceRequest, existing []Source) ([]Source, error) {
	if len(sources) == 0 {
		return nil, nil
	}

	if s.runs == nil {
		return nil, ErrCrawlingDisabled
	}

	if len(existing)+len(sources) > s.limits.MaxSources {
		return nil, fmt.Errorf("%w: a lab has at most %d sources", ErrInvalid, s.limits.MaxSources)
	}

	seen := make(map[string]bool, len(existing)+len(sources))
	for _, src := range existing {
		seen[src.URL] = true
	}

	prepared := make([]Source, 0, len(sources))

	for _, req := range sources {
		r, err := s.runs.Prepare(runRequest(Source{URL: req.URL, Scope: req.Scope, MaxDepth: req.Depth, MaxLinks: req.Links}))
		if err != nil {
			return nil, fmt.Errorf("%w: source %q: %w", ErrInvalid, req.URL, err)
		}

		if seen[r.URL] {
			return nil, fmt.Errorf("%w: %s", ErrSourceExists, r.URL)
		}

		seen[r.URL] = true

		scope := r.Scope
		if req.Scope == ScopePage {
			scope = ScopePage
		}

		prepared = append(prepared, Source{URL: r.URL, Scope: scope, MaxDepth: r.Depth, MaxLinks: r.Links})
	}

	return prepared, nil
}

// addAll records each source, then queues its crawl. A source whose crawl
// could not be queued stays, with no run, and can be refreshed.
func (s *Service) addAll(ctx context.Context, ownerID, labID int64, sources []Source) error {
	for _, src := range sources {
		added, err := s.store.AddSource(ctx, ownerID, labID, src)
		if err != nil {
			return err
		}

		if _, err := s.submit(ctx, ownerID, added); err != nil {
			return err
		}
	}

	return nil
}

func (s *Service) submit(ctx context.Context, ownerID int64, src Source) (runs.Run, error) {
	return s.runs.Submit(ctx, runRequest(src), runs.Origin{UserID: ownerID, SourceID: src.ID})
}

// runRequest is the crawl a source asks for. A page source is one page: no
// depth, a limit of one. Lab crawls skip the page cache: a cached page is not
// saved again, so it would never join the lab.
func runRequest(src Source) runs.Request {
	req := runs.Request{URL: src.URL, Scope: src.Scope, Depth: src.MaxDepth, Links: src.MaxLinks}

	if src.Scope == ScopePage {
		req.Scope, req.Depth, req.Links = runs.ScopeSection, 1, 1
	}

	req.Upload = src.Kind == KindUpload

	return req
}

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)

	if name == "" || utf8.RuneCountInString(name) > maxNameRunes || strings.ContainsFunc(name, unicode.IsControl) {
		return "", fmt.Errorf("%w: a name is 1 to %d characters on one line", ErrInvalid, maxNameRunes)
	}

	return name, nil
}

func cleanEngines(engines []string) ([]string, error) {
	out := []string{}

	for _, e := range engines {
		e = strings.TrimSpace(e)
		if e == "" || utf8.RuneCountInString(e) > maxEngineRunes || strings.ContainsFunc(e, unicode.IsControl) || strings.Contains(e, ",") {
			return nil, fmt.Errorf("%w: engine %q is not an engine name", ErrInvalid, e)
		}

		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}

	if len(out) > maxEngines {
		return nil, fmt.Errorf("%w: at most %d engines", ErrInvalid, maxEngines)
	}

	return out, nil
}

func cleanTopic(topic string) (string, error) {
	topic = strings.TrimSpace(topic)

	if utf8.RuneCountInString(topic) > maxTopicRunes {
		return "", fmt.Errorf("%w: a topic is at most %d characters", ErrInvalid, maxTopicRunes)
	}

	return topic, nil
}
