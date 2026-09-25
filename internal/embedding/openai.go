package embedding

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

type OpenAIConfig struct {
	BaseURL        string
	APIKey         string
	Model          string
	Dimensions     int
	BatchSize      int
	RequestTimeout time.Duration
	MaxRetries     int
}

// OpenAIClient speaks the OpenAI /v1/embeddings protocol, which Ollama, vLLM,
// TEI and the hosted providers all implement.
type OpenAIClient struct {
	cfg        OpenAIConfig
	http       *http.Client
	retryDelay time.Duration
}

type embeddingRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
	Dimensions     int      `json:"dimensions,omitempty"`
}

type embeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func NewOpenAIClient(cfg OpenAIConfig) (*OpenAIClient, error) {
	switch {
	case cfg.BaseURL == "":
		return nil, fmt.Errorf("%w: base URL is empty", ErrInvalidConfig)
	case cfg.Model == "":
		return nil, fmt.Errorf("%w: model is empty", ErrInvalidConfig)
	case cfg.BatchSize <= 0:
		return nil, fmt.Errorf("%w: batch size must be > 0", ErrInvalidConfig)
	case cfg.Dimensions < 0:
		return nil, fmt.Errorf("%w: dimensions must be >= 0", ErrInvalidConfig)
	case cfg.MaxRetries < 0:
		return nil, fmt.Errorf("%w: max retries must be >= 0", ErrInvalidConfig)
	case cfg.RequestTimeout <= 0:
		return nil, fmt.Errorf("%w: request timeout must be > 0", ErrInvalidConfig)
	}

	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")

	return &OpenAIClient{
		cfg: cfg,
		http: &http.Client{
			Timeout:   cfg.RequestTimeout,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
		retryDelay: retryBaseDelay,
	}, nil
}

func (c *OpenAIClient) Model() string {
	return c.cfg.Model
}

// Probe embeds one string to learn the vector size the endpoint actually returns,
// and fails if it disagrees with an explicitly requested one.
func (c *OpenAIClient) Probe(ctx context.Context) (int, error) {
	vectors, err := c.Embed(ctx, []string{"probe"})
	if err != nil {
		return 0, err
	}

	dim := len(vectors[0])
	if c.cfg.Dimensions > 0 && dim != c.cfg.Dimensions {
		return 0, fmt.Errorf("%w: asked for %d, got %d", ErrDimensionIgnored, c.cfg.Dimensions, dim)
	}

	return dim, nil
}

func (c *OpenAIClient) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))

	for start := 0; start < len(texts); start += c.cfg.BatchSize {
		batch := texts[start:min(start+c.cfg.BatchSize, len(texts))]

		vectors, err := c.embedWithRetry(ctx, batch)
		if err != nil {
			return nil, err
		}

		out = append(out, vectors...)
	}

	if err := consistentDimensions(out); err != nil {
		return nil, err
	}

	return out, nil
}

func (c *OpenAIClient) embedWithRetry(ctx context.Context, batch []string) ([][]float32, error) {
	var lastErr error

	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, c.backoff(attempt)); err != nil {
				return nil, errors.Join(lastErr, err)
			}
		}

		vectors, retryable, err := c.embedOnce(ctx, batch)
		if err == nil {
			return vectors, nil
		}

		lastErr = err
		if !retryable || ctx.Err() != nil {
			return nil, err
		}
	}

	return nil, fmt.Errorf("giving up after %d attempts: %w", c.cfg.MaxRetries+1, lastErr)
}

func (c *OpenAIClient) embedOnce(ctx context.Context, batch []string) ([][]float32, bool, error) {
	payload, err := json.Marshal(embeddingRequest{
		Model:          c.cfg.Model,
		Input:          batch,
		EncodingFormat: "float",
		Dimensions:     c.cfg.Dimensions,
	})
	if err != nil {
		return nil, false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/embeddings", bytes.NewReader(payload))
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

	vectors, err := decode(body, len(batch))
	if err != nil {
		return nil, false, err
	}

	return vectors, false, nil
}

// decode places vectors by their index field: the protocol does not promise the
// data array comes back in input order.
func decode(body []byte, want int) ([][]float32, error) {
	var parsed embeddingResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}

	if len(parsed.Data) != want {
		return nil, fmt.Errorf("%w: sent %d inputs, got %d vectors", ErrMalformed, want, len(parsed.Data))
	}

	vectors := make([][]float32, want)

	for _, item := range parsed.Data {
		if item.Index < 0 || item.Index >= want || vectors[item.Index] != nil {
			return nil, fmt.Errorf("%w: bad or duplicate index %d", ErrMalformed, item.Index)
		}

		if len(item.Embedding) == 0 {
			return nil, fmt.Errorf("%w: empty vector at index %d", ErrMalformed, item.Index)
		}

		vectors[item.Index] = item.Embedding
	}

	return vectors, nil
}

func consistentDimensions(vectors [][]float32) error {
	for i, v := range vectors {
		if len(v) != len(vectors[0]) {
			return fmt.Errorf("%w: vector %d has %d dimensions, vector 0 has %d", ErrMalformed, i, len(v), len(vectors[0]))
		}
	}

	return nil
}

func (c *OpenAIClient) backoff(attempt int) time.Duration {
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
	s := strings.TrimSpace(string(body))
	if len(s) > errorBodyLimit {
		return s[:errorBodyLimit] + "…"
	}

	return s
}
