package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeServer struct {
	t        *testing.T
	mu       sync.Mutex
	requests []embeddingRequest
	headers  []http.Header
	handler  func(req embeddingRequest, attempt int) (int, any)
	calls    atomic.Int32
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/embeddings" || r.Method != http.MethodPost {
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}

	var req embeddingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Fatalf("decode request: %v", err)
	}

	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.headers = append(f.headers, r.Header.Clone())
	f.mu.Unlock()

	status, body := f.handler(req, int(f.calls.Add(1)))
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// echo returns one vector per input: [len(input), dim-1 zeros...], so tests can
// check that output order follows input order.
func echo(dim int) func(embeddingRequest, int) (int, any) {
	return func(req embeddingRequest, _ int) (int, any) {
		data := make([]map[string]any, len(req.Input))
		for i, text := range req.Input {
			v := make([]float32, dim)
			v[0] = float32(len(text))
			data[i] = map[string]any{"index": i, "embedding": v}
		}
		return http.StatusOK, map[string]any{"data": data}
	}
}

func newClient(t *testing.T, handler func(embeddingRequest, int) (int, any), mutate func(*OpenAIConfig)) (*OpenAIClient, *fakeServer) {
	t.Helper()

	fake := &fakeServer{t: t, handler: handler}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	cfg := OpenAIConfig{
		BaseURL:        srv.URL + "/v1/",
		Model:          "test-model",
		BatchSize:      4,
		RequestTimeout: 5 * time.Second,
		MaxRetries:     3,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	c, err := NewOpenAIClient(cfg)
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}

	c.retryDelay = time.Millisecond

	return c, fake
}

func texts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%0*d", i+1, 0)
	}
	return out
}

func TestEmbedBatchesAndPreservesOrder(t *testing.T) {
	c, fake := newClient(t, echo(8), nil)

	vectors, err := c.Embed(context.Background(), texts(10))
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if len(fake.requests) != 3 {
		t.Errorf("10 inputs at batch size 4 need 3 requests, got %d", len(fake.requests))
	}

	for i, v := range vectors {
		if int(v[0]) != i+1 {
			t.Fatalf("vector %d belongs to input %d: order not preserved", i, int(v[0])-1)
		}
	}

	if fake.requests[0].Model != "test-model" || fake.requests[0].EncodingFormat != "float" {
		t.Errorf("request fields wrong: %+v", fake.requests[0])
	}
}

func TestEmbedUsesIndexNotArrayOrder(t *testing.T) {
	c, _ := newClient(t, func(req embeddingRequest, _ int) (int, any) {
		return http.StatusOK, map[string]any{"data": []map[string]any{
			{"index": 1, "embedding": []float32{2, 0}},
			{"index": 0, "embedding": []float32{1, 0}},
		}}
	}, nil)

	vectors, err := c.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if vectors[0][0] != 1 || vectors[1][0] != 2 {
		t.Errorf("vectors must be placed by index, got %v", vectors)
	}
}

func TestEmbedRetriesTransientFailures(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c, fake := newClient(t, func(req embeddingRequest, attempt int) (int, any) {
				if attempt < 3 {
					return status, map[string]any{"error": "busy"}
				}
				return echo(4)(req, attempt)
			}, nil)

			if _, err := c.Embed(context.Background(), []string{"x"}); err != nil {
				t.Fatalf("should succeed on the third attempt: %v", err)
			}

			if n := fake.calls.Load(); n != 3 {
				t.Errorf("want 3 attempts, got %d", n)
			}
		})
	}
}

func TestEmbedDoesNotRetryClientErrors(t *testing.T) {
	c, fake := newClient(t, func(embeddingRequest, int) (int, any) {
		return http.StatusBadRequest, map[string]any{"error": "model not found"}
	}, nil)

	_, err := c.Embed(context.Background(), []string{"x"})
	if !errors.Is(err, ErrRequest) {
		t.Fatalf("err = %v, want ErrRequest", err)
	}

	if n := fake.calls.Load(); n != 1 {
		t.Errorf("a 400 must not be retried, got %d attempts", n)
	}
}

func TestEmbedGivesUpAfterMaxRetries(t *testing.T) {
	c, fake := newClient(t, func(embeddingRequest, int) (int, any) {
		return http.StatusBadGateway, map[string]any{}
	}, func(cfg *OpenAIConfig) { cfg.MaxRetries = 2 })

	if _, err := c.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("expected an error")
	}

	if n := fake.calls.Load(); n != 3 {
		t.Errorf("max_retries 2 means 3 attempts, got %d", n)
	}
}

func TestEmbedStopsRetryingWhenContextEnds(t *testing.T) {
	c, _ := newClient(t, func(embeddingRequest, int) (int, any) {
		return http.StatusServiceUnavailable, map[string]any{}
	}, func(cfg *OpenAIConfig) { cfg.MaxRetries = 50 })
	c.retryDelay = time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := c.Embed(ctx, []string{"x"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("returned after %s: backoff ignored the context", elapsed)
	}
}

func TestEmbedRejectsMalformedResponses(t *testing.T) {
	cases := []struct {
		name string
		body any
	}{
		{"too few vectors", map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1}}}}},
		{"duplicate index", map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": []float32{1}}, {"index": 0, "embedding": []float32{1}},
		}}},
		{"index out of range", map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": []float32{1}}, {"index": 5, "embedding": []float32{1}},
		}}},
		{"empty vector", map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": []float32{}}, {"index": 1, "embedding": []float32{1}},
		}}},
		{"not json", "definitely not an embedding"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newClient(t, func(embeddingRequest, int) (int, any) { return http.StatusOK, tc.body }, nil)

			if _, err := c.Embed(context.Background(), []string{"a", "b"}); !errors.Is(err, ErrMalformed) {
				t.Errorf("err = %v, want ErrMalformed", err)
			}
		})
	}
}

func TestEmbedRejectsDimensionDriftAcrossBatches(t *testing.T) {
	c, _ := newClient(t, func(req embeddingRequest, attempt int) (int, any) {
		return echo(4+attempt)(req, attempt)
	}, func(cfg *OpenAIConfig) { cfg.BatchSize = 1 })

	if _, err := c.Embed(context.Background(), []string{"a", "b"}); !errors.Is(err, ErrMalformed) {
		t.Errorf("err = %v, want ErrMalformed", err)
	}
}

func TestAuthorizationHeader(t *testing.T) {
	withKey, fake := newClient(t, echo(2), func(cfg *OpenAIConfig) { cfg.APIKey = "secret" })
	if _, err := withKey.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatal(err)
	}

	if got := fake.headers[0].Get("Authorization"); got != "Bearer secret" {
		t.Errorf("Authorization = %q", got)
	}

	noKey, fake := newClient(t, echo(2), nil)
	if _, err := noKey.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatal(err)
	}

	if got := fake.headers[0].Get("Authorization"); got != "" {
		t.Errorf("no API key must mean no Authorization header, got %q", got)
	}
}

func TestProbe(t *testing.T) {
	native, fake := newClient(t, echo(1024), nil)

	dim, err := native.Probe(context.Background())
	if err != nil || dim != 1024 {
		t.Fatalf("Probe = %d, %v; want 1024", dim, err)
	}

	if fake.requests[0].Dimensions != 0 {
		t.Error("dimensions 0 must not be sent")
	}

	honoured, fake := newClient(t, echo(256), func(cfg *OpenAIConfig) { cfg.Dimensions = 256 })
	if dim, err := honoured.Probe(context.Background()); err != nil || dim != 256 {
		t.Fatalf("Probe = %d, %v; want 256", dim, err)
	}

	if fake.requests[0].Dimensions != 256 {
		t.Errorf("dimensions must be sent, got %d", fake.requests[0].Dimensions)
	}

	ignored, _ := newClient(t, echo(1024), func(cfg *OpenAIConfig) { cfg.Dimensions = 256 })
	if _, err := ignored.Probe(context.Background()); !errors.Is(err, ErrDimensionIgnored) {
		t.Errorf("err = %v, want ErrDimensionIgnored", err)
	}
}

func TestNewOpenAIClientValidates(t *testing.T) {
	base := OpenAIConfig{BaseURL: "http://x", Model: "m", BatchSize: 1, RequestTimeout: time.Second}

	for name, mutate := range map[string]func(*OpenAIConfig){
		"no base url":      func(c *OpenAIConfig) { c.BaseURL = "" },
		"no model":         func(c *OpenAIConfig) { c.Model = "" },
		"zero batch":       func(c *OpenAIConfig) { c.BatchSize = 0 },
		"negative dims":    func(c *OpenAIConfig) { c.Dimensions = -1 },
		"negative retries": func(c *OpenAIConfig) { c.MaxRetries = -1 },
		"zero timeout":     func(c *OpenAIConfig) { c.RequestTimeout = 0 },
	} {
		cfg := base
		mutate(&cfg)

		if _, err := NewOpenAIClient(cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: err = %v, want ErrInvalidConfig", name, err)
		}
	}
}
