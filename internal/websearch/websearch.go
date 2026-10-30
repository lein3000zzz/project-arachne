// Package websearch queries a SearXNG instance over its JSON API: the external
// search engines labs gather from.
package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"web-crawler/internal/urlnorm"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

var ErrUnavailable = errors.New("web search is unavailable")

type Result struct {
	URL     string   `json:"url"`
	Title   string   `json:"title"`
	Snippet string   `json:"snippet"`
	Engines []string `json:"engines"`
}

type Response struct {
	Results []Result `json:"results"`
	// Engines that did not answer, and why: blocked, a CAPTCHA, a timeout.
	Failed map[string]string `json:"failed,omitempty"`
}

type Client struct {
	base string
	http *http.Client

	mu       sync.Mutex
	engines  []string
	loadedAt time.Time
}

const (
	enginesTTL     = 10 * time.Minute
	errorBodyLimit = 512
)

func New(baseURL string, timeout time.Duration) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		http: &http.Client{Timeout: timeout, Transport: otelhttp.NewTransport(http.DefaultTransport)},
	}
}

type searxResult struct {
	URL     string   `json:"url"`
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Engine  string   `json:"engine"`
	Engines []string `json:"engines"`
}

type searxResponse struct {
	Results      []searxResult `json:"results"`
	Unresponsive [][]string    `json:"unresponsive_engines"`
}

// Search asks the given engines; none means the instance's defaults. Results
// keep SearXNG's order, canonical and each URL once; only web pages are kept.
func (c *Client) Search(ctx context.Context, query string, engines []string) (Response, error) {
	params := url.Values{"q": {query}, "format": {"json"}, "safesearch": {"0"}, "pageno": {"1"}}
	if len(engines) > 0 {
		params.Set("engines", strings.Join(engines, ","))
	}

	var raw searxResponse
	if err := c.get(ctx, "/search?"+params.Encode(), &raw); err != nil {
		return Response{}, err
	}

	resp := Response{Results: []Result{}}
	seen := map[string]bool{}

	for _, r := range raw.Results {
		link, ok := urlnorm.Canonical(r.URL, nil)
		if !ok || !strings.HasPrefix(link, "http") || seen[link] {
			continue
		}

		seen[link] = true

		found := r.Engines
		if len(found) == 0 && r.Engine != "" {
			found = []string{r.Engine}
		}

		resp.Results = append(resp.Results, Result{URL: link, Title: oneLine(r.Title), Snippet: oneLine(r.Content), Engines: found})
	}

	for _, u := range raw.Unresponsive {
		if len(u) == 0 {
			continue
		}

		if resp.Failed == nil {
			resp.Failed = map[string]string{}
		}

		reason := ""
		if len(u) > 1 {
			reason = u[1]
		}

		resp.Failed[u[0]] = reason
	}

	return resp, nil
}

// Engines lists the instance's enabled general web engines - not its
// translators or currency converters - for a lab to choose from.
func (c *Client) Engines(ctx context.Context) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.engines != nil && time.Since(c.loadedAt) < enginesTTL {
		return c.engines, nil
	}

	var cfg struct {
		Engines []struct {
			Name       string   `json:"name"`
			Enabled    bool     `json:"enabled"`
			Categories []string `json:"categories"`
		} `json:"engines"`
	}

	if err := c.get(ctx, "/config", &cfg); err != nil {
		return nil, err
	}

	engines := []string{}

	for _, e := range cfg.Engines {
		general := slices.Contains(e.Categories, "general")
		utility := slices.Contains(e.Categories, "translate") || slices.Contains(e.Categories, "currency")

		if e.Enabled && general && !utility {
			engines = append(engines, e.Name)
		}
	}

	slices.Sort(engines)

	c.engines, c.loadedAt = engines, time.Now()

	return engines, nil
}

func (c *Client) get(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "application/json")
	// SearXNG's bot detection logs an error for every request without a client
	// address; ours come from inside the stack.
	req.Header.Set("X-Real-IP", "127.0.0.1")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		return fmt.Errorf("%w: HTTP %d: %s", ErrUnavailable, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}

	return nil
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
