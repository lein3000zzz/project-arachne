// Package rerank scores query-document pairs with a cross-encoder over the
// rerank API that llama.cpp's server, vLLM, Jina and Cohere share: POST /rerank
// with a query and documents, scores back by document index.
package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 8 * time.Second
	errorBodyLimit = 512
)

type Config struct {
	// Up to and including the version segment, e.g. http://reranker:8080/v1.
	BaseURL        string
	APIKey         string
	Model          string
	RequestTimeout time.Duration
	MaxRetries     int
}

type Client struct {
	cfg        Config
	http       *http.Client
	retryDelay time.Duration
}

func New(cfg Config) (*Client, error) {
	switch {
	case cfg.BaseURL == "":
		return nil, fmt.Errorf("%w: base URL is empty", ErrInvalidConfig)
	case cfg.Model == "":
		return nil, fmt.Errorf("%w: model is empty", ErrInvalidConfig)
	case cfg.RequestTimeout <= 0:
		return nil, fmt.Errorf("%w: request timeout must be > 0", ErrInvalidConfig)
	case cfg.MaxRetries < 0:
		return nil, fmt.Errorf("%w: max retries must be >= 0", ErrInvalidConfig)
	}

	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")

	return &Client{
		cfg:        cfg,
		http:       &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport), Timeout: cfg.RequestTimeout},
		retryDelay: retryBaseDelay,
	}, nil
}

func (c *Client) Model() string {
	return c.cfg.Model
}

type request struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n"`
}

type response struct {
	Results []struct {
		Index          int      `json:"index"`
		RelevanceScore *float64 `json:"relevance_score"`
	} `json:"results"`
}

// Rerank returns one score per document, in document order; higher is more
// relevant. Scores are only comparable within one call.
func (c *Client) Rerank(ctx context.Context, query string, documents []string) ([]float64, error) {
	if len(documents) == 0 {
		return []float64{}, nil
	}

	payload, err := json.Marshal(request{Model: c.cfg.Model, Query: query, Documents: documents, TopN: len(documents)})
	if err != nil {
		return nil, err
	}

	var lastErr error

	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, c.backoff(attempt)); err != nil {
				return nil, errors.Join(lastErr, err)
			}
		}

		scores, retryable, err := c.once(ctx, payload, len(documents))
		if err == nil {
			return scores, nil
		}

		lastErr = err
		if !retryable || ctx.Err() != nil {
			return nil, err
		}
	}

	return nil, fmt.Errorf("giving up after %d attempts: %w", c.cfg.MaxRetries+1, lastErr)
}

func (c *Client) once(ctx context.Context, payload []byte, want int) ([]float64, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/rerank", bytes.NewReader(payload))
	if err != nil {
		return nil, false, err
	}

	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("%w: %w", ErrRequest, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, true, fmt.Errorf("%w: reading body: %w", ErrRequest, err)
	}

	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return nil, retryable, fmt.Errorf("%w: HTTP %d: %s", ErrRequest, resp.StatusCode, truncate(body))
	}

	scores, err := decode(body, want)

	return scores, false, err
}

// decode places scores by index: providers return results sorted by relevance,
// not in document order.
func decode(body []byte, want int) ([]float64, error) {
	var parsed response
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}

	if len(parsed.Results) != want {
		return nil, fmt.Errorf("%w: sent %d documents, got %d scores", ErrMalformed, want, len(parsed.Results))
	}

	scores := make([]float64, want)
	seen := make([]bool, want)

	for _, r := range parsed.Results {
		if r.Index < 0 || r.Index >= want || seen[r.Index] || r.RelevanceScore == nil {
			return nil, fmt.Errorf("%w: bad, duplicate or unscored index %d", ErrMalformed, r.Index)
		}

		seen[r.Index] = true
		scores[r.Index] = *r.RelevanceScore
	}

	return scores, nil
}

func (c *Client) backoff(attempt int) time.Duration {
	delay := min(c.retryDelay<<(attempt-1), retryMaxDelay)
	return delay/2 + rand.N(delay/2+1)
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func truncate(body []byte) string {
	if len(body) > errorBodyLimit {
		body = body[:errorBodyLimit]
	}

	return strings.TrimSpace(string(body))
}
