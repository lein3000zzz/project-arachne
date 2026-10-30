package labs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
	"web-crawler/internal/llm"
	"web-crawler/internal/runs"
)

type Store interface {
	Create(ctx context.Context, ownerID int64, name, topic string) (Lab, error)
	Get(ctx context.Context, ownerID, labID int64) (Lab, error)
	List(ctx context.Context, ownerID int64) ([]Lab, error)
	Delete(ctx context.Context, ownerID, labID int64) error
	AddSource(ctx context.Context, ownerID, labID int64, src Source) (Source, error)
	Source(ctx context.Context, ownerID, labID, sourceID int64) (Source, error)
	Sources(ctx context.Context, ownerID, labID int64) ([]Source, error)
	RemoveSource(ctx context.Context, ownerID, labID, sourceID int64) error
	PageURLs(ctx context.Context, ownerID int64, labIDs []int64) ([]string, error)
	Pages(ctx context.Context, ownerID, labID int64, limit int) ([]Page, error)
}

type Runs interface {
	Prepare(req runs.Request) (runs.Request, error)
	Submit(ctx context.Context, req runs.Request, origin runs.Origin) (runs.Run, error)
	Get(ctx context.Context, id string) (runs.Run, error)
}

var ErrCrawlingDisabled = errors.New("crawling is disabled (crawl_api.enabled), so labs cannot gather sources")

const (
	maxNameRunes  = 80
	maxTopicRunes = 2000
)

type Limits struct {
	MaxSources int
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
	store  Store
	runs   Runs
	chat   llm.Client
	limits Limits
}

// NewService takes nil runs when crawling is off: labs can then be read and
// deleted, but gain no sources. Without a chat model, plans are drafted by
// reading the request literally.
func NewService(store Store, runs Runs, chat llm.Client, limits Limits) *Service {
	return &Service{store: store, runs: runs, chat: chat, limits: limits}
}

func (s *Service) List(ctx context.Context, ownerID int64) ([]Lab, error) {
	return s.store.List(ctx, ownerID)
}

// Create makes the lab and starts crawling its sources. The sources are all
// checked first, so a bad one leaves no half-made lab behind.
func (s *Service) Create(ctx context.Context, ownerID int64, name, topic string, sources []SourceRequest) (Detail, error) {
	name, err := cleanName(name)
	if err != nil {
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

	lab, err := s.store.Create(ctx, ownerID, name, topic)
	if err != nil {
		return Detail{}, err
	}

	if err := s.addAll(ctx, ownerID, lab.ID, prepared); err != nil {
		return Detail{}, err
	}

	return s.Get(ctx, ownerID, lab.ID)
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
	return s.store.RemoveSource(ctx, ownerID, labID, sourceID)
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
	return s.store.Delete(ctx, ownerID, labID)
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

	return req
}

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)

	if name == "" || utf8.RuneCountInString(name) > maxNameRunes || strings.ContainsFunc(name, unicode.IsControl) {
		return "", fmt.Errorf("%w: a name is 1 to %d characters on one line", ErrInvalid, maxNameRunes)
	}

	return name, nil
}

func cleanTopic(topic string) (string, error) {
	topic = strings.TrimSpace(topic)

	if utf8.RuneCountInString(topic) > maxTopicRunes {
		return "", fmt.Errorf("%w: a topic is at most %d characters", ErrInvalid, maxTopicRunes)
	}

	return topic, nil
}
