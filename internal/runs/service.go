package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/urlnorm"
	"web-crawler/internal/utils"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Scope presets, besides an explicit URL prefix.
const (
	ScopeSite     = "site"
	ScopeSection  = "section"
	ScopeAnywhere = "anywhere"
)

type Request struct {
	URL      string `json:"url"`
	Depth    int    `json:"depth"`
	Links    int    `json:"links"`
	Scope    string `json:"scope"`
	UseCache bool   `json:"use_cache"`
}

// Origin says who asked for a run and for which lab source. It is never read
// from a request body: a source taken from one would let anyone crawl pages
// into another user's lab.
type Origin struct {
	UserID   int64
	SourceID int64
}

type Limits struct {
	DefaultDepth, MaxDepth int
	DefaultLinks, MaxLinks int
}

type Producer interface {
	ProduceSync(ctx context.Context, rs ...*kgo.Record) kgo.ProduceResults
}

type Service struct {
	store    *Store
	producer Producer
	limits   Limits
}

func NewService(store *Store, producer Producer, limits Limits) *Service {
	return &Service{store: store, producer: producer, limits: limits}
}

func (s *Service) List(ctx context.Context, limit int) ([]Run, error) {
	return s.store.List(ctx, limit)
}

func (s *Service) Get(ctx context.Context, id string) (Run, error) {
	return s.store.Get(ctx, id)
}

// Prepare checks a request and fills in its defaults - canonical URL, depth,
// links, scope - without submitting anything.
func (s *Service) Prepare(req Request) (Request, error) {
	run, err := s.build(req)
	if err != nil {
		return Request{}, err
	}

	if req.Scope == "" {
		req.Scope = ScopeSite
	}

	return Request{URL: run.StartURL, Depth: run.MaxDepth, Links: run.MaxLinks, Scope: req.Scope, UseCache: req.UseCache}, nil
}

// Submit records the run, then puts it on the runs topic, where the crawler takes
// it as it would one from `make crawl`.
func (s *Service) Submit(ctx context.Context, req Request, origin Origin) (Run, error) {
	run, err := s.build(req)
	if err != nil {
		return Run{}, err
	}

	if err := s.store.Submitted(ctx, run, origin); err != nil {
		return Run{}, err
	}

	payload, err := json.Marshal(run)
	if err != nil {
		return Run{}, err
	}

	if err := s.producer.ProduceSync(ctx, &kgo.Record{Value: payload}).FirstErr(); err != nil {
		_ = s.store.Finished(context.WithoutCancel(ctx), run, "could not be queued")
		return Run{}, fmt.Errorf("queueing the run: %w", err)
	}

	return s.store.Get(ctx, run.ID)
}

func (s *Service) build(req Request) (*config.Run, error) {
	start, ok := urlnorm.FromUserInput(req.URL)
	if !ok {
		return nil, fmt.Errorf("%w: %q is not a web address", ErrInvalidRequest, req.URL)
	}

	depth, links := req.Depth, req.Links
	if depth == 0 {
		depth = s.limits.DefaultDepth
	}

	if links == 0 {
		links = s.limits.DefaultLinks
	}

	if depth < 1 || depth > s.limits.MaxDepth {
		return nil, fmt.Errorf("%w: depth must be in [1, %d]", ErrInvalidRequest, s.limits.MaxDepth)
	}

	if links < 1 || links > s.limits.MaxLinks {
		return nil, fmt.Errorf("%w: links must be in [1, %d]", ErrInvalidRequest, s.limits.MaxLinks)
	}

	scope, err := scopeFor(start, req.Scope)
	if err != nil {
		return nil, err
	}

	id, err := utils.GenerateID()
	if err != nil {
		return nil, err
	}

	return &config.Run{
		ID:           id,
		UseCacheFlag: req.UseCache,
		MaxDepth:     depth,
		MaxLinks:     links,
		StartURL:     start,
		Scope:        scope,
	}, nil
}

// scopeFor turns a preset into a URL prefix of the canonical start URL. The
// default is the site: a crawl that wanders off to every linked site is rarely
// what anyone means by "crawl this".
func scopeFor(start, scope string) (string, error) {
	u, err := url.Parse(start)
	if err != nil {
		return "", err
	}

	switch scope {
	case "", ScopeSite:
		return u.Scheme + "://" + u.Host + "/", nil
	case ScopeSection:
		return u.Scheme + "://" + u.Host + u.Path[:strings.LastIndex(u.Path, "/")+1], nil
	case ScopeAnywhere:
		return "", nil
	}

	prefix, ok := urlnorm.FromUserInput(scope)
	if !ok || !strings.HasPrefix(start, prefix) {
		return "", fmt.Errorf("%w: scope %q does not contain the start URL", ErrInvalidRequest, scope)
	}

	return prefix, nil
}
