package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newClient(t *testing.T, provider, url string) Client {
	t.Helper()

	c, err := New(Config{Provider: provider, BaseURL: url + "/v1/", APIKey: "secret", Model: "m", MaxRetries: 2})
	if err != nil {
		t.Fatal(err)
	}

	switch c := c.(type) {
	case *openAIClient:
		c.retryDelay = time.Millisecond
	case *anthropicClient:
		c.retryDelay = time.Millisecond
	}

	return c
}

func sse(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")

	for _, e := range events {
		fmt.Fprint(w, e, "\n\n")
		w.(http.Flusher).Flush()
	}
}

func collect(t *testing.T, c Client, req Request) (Response, []string, error) {
	t.Helper()

	var deltas []string

	resp, err := c.Stream(context.Background(), req, func(d string) error {
		deltas = append(deltas, d)
		return nil
	})

	return resp, deltas, err
}

func TestOpenAICompatibleStreams(t *testing.T) {
	var got map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("path %s auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}

		_ = json.NewDecoder(r.Body).Decode(&got)

		sse(w,
			`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
			`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
			`data: {"choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":2}}`,
			`data: [DONE]`)
	}))
	defer srv.Close()

	temp := 0.2

	resp, deltas, err := collect(t, newClient(t, ProviderOpenAICompatible, srv.URL), Request{
		System: "be brief", Messages: []Message{{Role: RoleUser, Content: "hi"}}, MaxTokens: 64, Temperature: &temp,
	})
	if err != nil {
		t.Fatal(err)
	}

	if resp.Text != "Hello" || strings.Join(deltas, "|") != "Hel|lo" || resp.StopReason != "stop" ||
		resp.Usage != (Usage{InputTokens: 12, OutputTokens: 2}) {
		t.Errorf("resp %+v deltas %v", resp, deltas)
	}

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 || got["max_tokens"] != float64(64) || got["max_completion_tokens"] != nil || got["temperature"] != 0.2 {
		t.Errorf("request body = %v", got)
	}
}

func TestOpenAIUsesMaxCompletionTokensAndOmitsTemperature(t *testing.T) {
	var got map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		sse(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`, `data: [DONE]`)
	}))
	defer srv.Close()

	if _, _, err := collect(t, newClient(t, ProviderOpenAI, srv.URL), Request{MaxTokens: 64}); err != nil {
		t.Fatal(err)
	}

	if got["max_completion_tokens"] != float64(64) || got["max_tokens"] != nil {
		t.Errorf("request body = %v", got)
	}

	if _, present := got["temperature"]; present {
		t.Error("an unset temperature must not be sent: reasoning models reject it")
	}

	if _, present := got["response_format"]; present {
		t.Error("JSON mode sent unasked")
	}

	if _, _, err := collect(t, newClient(t, ProviderOpenAICompatible, srv.URL), Request{MaxTokens: 64, JSON: true}); err != nil {
		t.Fatal(err)
	}

	if format, _ := got["response_format"].(map[string]any); format["type"] != "json_object" {
		t.Errorf("JSON mode: response_format = %v", got["response_format"])
	}
}

func TestAnthropicStreams(t *testing.T) {
	var got map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "secret" || r.Header.Get("Anthropic-Version") != anthropicVersion {
			t.Errorf("path %s headers %v", r.URL.Path, r.Header)
		}

		_ = json.NewDecoder(r.Body).Decode(&got)

		sse(w,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":30}}}",
			"event: ping\ndata: {\"type\":\"ping\"}",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\"}",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"При\"}}",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"вет\"}}",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}")
	}))
	defer srv.Close()

	resp, deltas, err := collect(t, newClient(t, ProviderAnthropic, srv.URL), Request{
		System: "be brief", Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if resp.Text != "Привет" || len(deltas) != 2 || resp.StopReason != "end_turn" || resp.Usage != (Usage{InputTokens: 30, OutputTokens: 4}) {
		t.Errorf("resp %+v deltas %v", resp, deltas)
	}

	if got["system"] != "be brief" || got["max_tokens"] != float64(anthropicDefaultMaxTokens) {
		t.Errorf("request body = %v: system goes top-level, max_tokens is mandatory", got)
	}
}

func TestRetriesOnlyBeforeTextIsHandedOut(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "overloaded", http.StatusServiceUnavailable)
			return
		}

		sse(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`, `data: [DONE]`)
	}))
	defer srv.Close()

	resp, _, err := collect(t, newClient(t, ProviderOpenAICompatible, srv.URL), Request{})
	if err != nil || resp.Text != "ok" || calls.Load() != 2 {
		t.Errorf("text %q err %v calls %d: a 503 before streaming must be retried", resp.Text, err, calls.Load())
	}

	calls.Store(0)

	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		sse(w, `data: {"choices":[{"delta":{"content":"partial"}}]}`)
	}))
	defer cut.Close()

	resp, deltas, err := collect(t, newClient(t, ProviderOpenAICompatible, cut.URL), Request{})
	if err == nil || !errors.Is(err, ErrRequest) || calls.Load() != 1 || len(deltas) != 1 || resp.Text != "partial" {
		t.Errorf("err %v calls %d deltas %v: a stream cut after text must fail without a retry", err, calls.Load(), deltas)
	}
}

func TestErrorsAreReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			sse(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}")
			return
		}

		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"model not found"}}`)
	}))
	defer srv.Close()

	if _, _, err := collect(t, newClient(t, ProviderOpenAICompatible, srv.URL), Request{}); err == nil || !strings.Contains(err.Error(), "model not found") {
		t.Errorf("openai 400: %v", err)
	}

	if _, _, err := collect(t, newClient(t, ProviderAnthropic, srv.URL), Request{}); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("anthropic error event: %v", err)
	}
}

func TestNewValidates(t *testing.T) {
	for _, cfg := range []Config{
		{Provider: "gemini", BaseURL: "http://x", Model: "m"},
		{Provider: ProviderOpenAI, Model: "m"},
		{Provider: ProviderOpenAI, BaseURL: "http://x"},
		{Provider: ProviderAnthropic, BaseURL: "http://x", Model: "m"},
	} {
		if _, err := New(cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%+v: %v", cfg, err)
		}
	}
}
