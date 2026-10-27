// Package robots decides whether a URL may be fetched, by the site's robots.txt,
// following RFC 9309.
package robots

import (
	"net/http"
	"sync"
	"time"
	"web-crawler/internal/networker"
	"web-crawler/internal/utils"

	"github.com/jimsmart/grobotstxt"
	"go.uber.org/zap"
)

type Fetcher interface {
	Fetch(url string) (*networker.FetchResult, error)
}

// Cache holds each origin's robots.txt; an empty value is a site without one.
type Cache interface {
	Get(key string) (string, error)
	Set(key string, value any, ttl time.Duration) error
}

type Checker struct {
	logger  *zap.SugaredLogger
	fetcher Fetcher
	cache   Cache
	agent   string
	ttl     time.Duration
}

// NewChecker matches rules against agent, which must be the bare product token:
// the matcher ignores a group naming anything longer.
func NewChecker(logger *zap.SugaredLogger, fetcher Fetcher, cache Cache, agent string, ttl time.Duration) *Checker {
	return &Checker{logger: logger, fetcher: fetcher, cache: cache, agent: agent, ttl: ttl}
}

// Allowed applies the rules a site publishes. A robots.txt that does not exist
// (4xx) allows everything; one that cannot be read (5xx, network failure) allows
// nothing and is not cached, so the next check asks again.
func (c *Checker) Allowed(rawURL string) bool {
	origin, err := utils.GetBaseURL(rawURL)
	if err != nil {
		return false
	}

	if rules, err := c.cache.Get(origin); err == nil {
		return grobotstxt.AgentAllowed(rules, c.agent, rawURL)
	}

	res, err := c.fetcher.Fetch(origin + "/robots.txt")
	if err != nil {
		c.logger.Warnw("robots.txt unreachable, not fetching", "origin", origin, "err", err)
		return false
	}

	var rules string

	switch {
	case res.Status >= 200 && res.Status < 300:
		rules = string(res.Body)
	case res.Status >= http.StatusInternalServerError:
		c.logger.Warnw("robots.txt unreachable, not fetching", "origin", origin, "status", res.Status)
		return false
	}

	if err := c.cache.Set(origin, rules, c.ttl); err != nil {
		c.logger.Warnw("Failed to cache robots.txt", "origin", origin, "err", err)
	}

	return grobotstxt.AgentAllowed(rules, c.agent, rawURL)
}

// MemoryCache is a Cache for short-lived processes; it ignores the TTL.
type MemoryCache struct {
	mu    sync.Mutex
	rules map[string]string
}

func NewMemoryCache() *MemoryCache {
	return &MemoryCache{rules: make(map[string]string)}
}

func (m *MemoryCache) Get(key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rules, ok := m.rules[key]
	if !ok {
		return "", ErrNotCached
	}

	return rules, nil
}

func (m *MemoryCache) Set(key string, value any, _ time.Duration) error {
	rules, _ := value.(string)

	m.mu.Lock()
	m.rules[key] = rules
	m.mu.Unlock()

	return nil
}
