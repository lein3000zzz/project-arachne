// Package llm talks to chat models over the two protocols that cover the field:
// OpenAI's chat completions (OpenAI itself, Ollama, vLLM, OpenRouter, and most
// hosted providers) and Anthropic's Messages API. Every call streams.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	ProviderOpenAICompatible = "openai-compatible"
	ProviderOpenAI           = "openai"
	ProviderAnthropic        = "anthropic"
)

var Providers = []string{ProviderOpenAICompatible, ProviderOpenAI, ProviderAnthropic}

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// The result of a tool call, answering the call named by ToolCallID.
	RoleTool Role = "tool"
)

type Message struct {
	Role    Role
	Content string
	// The calls an assistant turn made, sent back with it as they came.
	ToolCalls  []ToolCall
	ToolCallID string
}

// Tool is a function the model may call. Parameters is a JSON Schema object.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

type Request struct {
	System   string
	Messages []Message
	// MaxTokens bounds the generated text, not the prompt.
	MaxTokens int
	// Nil leaves the provider's default: some models reject any other value.
	Temperature *float64
	// Asks for a single JSON object where the protocol can enforce it (OpenAI's
	// JSON mode). Anthropic's cannot: the prompt must ask, and the caller parse
	// leniently.
	JSON  bool
	Tools []Tool
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type Response struct {
	Text       string
	ToolCalls  []ToolCall
	StopReason string
	Usage      Usage
}

// Client generates a reply. onDelta, which may be nil, receives the text as it
// arrives; an error from it aborts the request.
type Client interface {
	Stream(ctx context.Context, req Request, onDelta func(string) error) (Response, error)
	Model() string
}

type Config struct {
	Provider string
	// Up to and including the version segment: https://api.openai.com/v1,
	// http://ollama:11434/v1, https://api.anthropic.com/v1.
	BaseURL    string
	APIKey     string
	Model      string
	MaxRetries int
}

func New(cfg Config) (Client, error) {
	switch {
	case cfg.BaseURL == "":
		return nil, fmt.Errorf("%w: base URL is empty", ErrInvalidConfig)
	case cfg.Model == "":
		return nil, fmt.Errorf("%w: model is empty", ErrInvalidConfig)
	case cfg.MaxRetries < 0:
		return nil, fmt.Errorf("%w: max retries must be >= 0", ErrInvalidConfig)
	}

	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")

	base := transport{
		cfg: cfg,
		// No client timeout: a reply streams for as long as it takes. The caller's
		// context bounds it.
		http:       &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)},
		retryDelay: retryBaseDelay,
	}

	switch cfg.Provider {
	case ProviderOpenAICompatible, ProviderOpenAI:
		return &openAIClient{transport: base, completionTokens: cfg.Provider == ProviderOpenAI}, nil
	case ProviderAnthropic:
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("%w: the Anthropic API needs a key", ErrInvalidConfig)
		}

		return &anthropicClient{transport: base}, nil
	}

	return nil, fmt.Errorf("%w: provider %q is not one of %v", ErrInvalidConfig, cfg.Provider, Providers)
}

const (
	retryBaseDelay = time.Second
	retryMaxDelay  = 16 * time.Second
	errorBodyLimit = 512
)

type transport struct {
	cfg        Config
	http       *http.Client
	retryDelay time.Duration
}

func (t *transport) Model() string {
	return t.cfg.Model
}

// attemptFunc runs one request. It reports whether anything reached onDelta:
// once text has been handed out, a retry would hand it out twice.
type attemptFunc func(ctx context.Context, onDelta func(string) error) (resp Response, streamed, retryable bool, err error)

func (t *transport) withRetries(ctx context.Context, onDelta func(string) error, attempt attemptFunc) (Response, error) {
	if onDelta == nil {
		onDelta = func(string) error { return nil }
	}

	var lastErr error

	for i := 0; i <= t.cfg.MaxRetries; i++ {
		if i > 0 {
			if err := sleep(ctx, t.backoff(i)); err != nil {
				return Response{}, errors.Join(lastErr, err)
			}
		}

		resp, streamed, retryable, err := attempt(ctx, onDelta)
		if err == nil {
			return resp, nil
		}

		lastErr = err
		if streamed || !retryable || ctx.Err() != nil {
			return resp, err
		}
	}

	return Response{}, fmt.Errorf("giving up after %d attempts: %w", t.cfg.MaxRetries+1, lastErr)
}

func (t *transport) backoff(attempt int) time.Duration {
	delay := min(t.retryDelay<<(attempt-1), retryMaxDelay)
	return delay/2 + rand.N(delay/2+1)
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
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
