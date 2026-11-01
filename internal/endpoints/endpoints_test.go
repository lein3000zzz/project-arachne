package endpoints

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"web-crawler/internal/auth"
	"web-crawler/internal/llm"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func TestKeysAreSealedToTheirUser(t *testing.T) {
	s, err := newSealer(testKey)
	if err != nil {
		t.Fatal(err)
	}

	sealed := s.seal("sk-test-1234", 1)

	if got, err := s.open(sealed, 1); err != nil || got != "sk-test-1234" {
		t.Errorf("open = %q, %v", got, err)
	}

	if _, err := s.open(sealed, 2); err == nil {
		t.Error("a key sealed for user 1 opened for user 2")
	}

	tampered := append([]byte{}, sealed...)
	tampered[len(tampered)-1] ^= 1

	if _, err := s.open(tampered, 1); err == nil {
		t.Error("a tampered key opened")
	}

	other, _ := newSealer([]byte("fedcba9876543210fedcba9876543210"))
	if _, err := other.open(sealed, 1); err == nil {
		t.Error("another ENDPOINT_KEY opened it")
	}

	if _, err := newSealer([]byte("short")); !errors.Is(err, ErrUnavailable) {
		t.Errorf("a short key: %v", err)
	}
}

func TestOnlyPublicAddressesArePublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.17.0.2": false, "192.168.1.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.1.2.3": false, "::1": false, "fc00::1": false, "fe80::1": false, "::ffff:10.0.0.1": false,
	} {
		if got := public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("public(%s) = %v", addr, got)
		}
	}
}

type memoryStore struct {
	mu   sync.Mutex
	rows map[int64]row
}

type row struct {
	e      Endpoint
	sealed []byte
}

func (m *memoryStore) Get(_ context.Context, id int64) (Endpoint, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.rows[id]
	if !ok {
		return Endpoint{}, nil, ErrNotSet
	}

	r.e.KeySet = r.sealed != nil

	return r.e, r.sealed, nil
}

func (m *memoryStore) Set(_ context.Context, id int64, e Endpoint, sealed []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.rows[id] = row{e: e, sealed: sealed}

	return nil
}

func (m *memoryStore) Delete(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.rows, id)

	return nil
}

func newService(t *testing.T, allowed ...string) (*Service, *memoryStore) {
	t.Helper()

	store := &memoryStore{rows: map[int64]row{}}

	s, err := New(store, testKey, Config{AllowedHosts: allowed, TestTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	return s, store
}

func ptr(s string) *string { return &s }

// openAI answers like an OpenAI-compatible server, recording the key it was
// sent.
func openAI(t *testing.T, reply string) (*httptest.Server, *string) {
	t.Helper()

	var gotKey string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", reply)
	}))
	t.Cleanup(srv.Close)

	return srv, &gotKey
}

func TestSettingsAreChecked(t *testing.T) {
	s, _ := newService(t, "model-box.lan")
	ctx := context.Background()

	for _, in := range []Settings{
		{Provider: "ollama", BaseURL: "https://api.openai.com/v1", Model: "m"},
		{Provider: "openai", BaseURL: "https://api.openai.com/v1"},
		{Provider: "openai", BaseURL: "http://api.openai.com/v1", Model: "m"},
		{Provider: "openai", BaseURL: "https://user:pass@api.openai.com/v1", Model: "m"},
		{Provider: "openai", BaseURL: "https://api.openai.com/v1?x=1", Model: "m"},
		{Provider: "openai-compatible", BaseURL: "https://localhost:11434/v1", Model: "m"},
		{Provider: "openai-compatible", BaseURL: "http://127.0.0.1:8200/v1", Model: "m"},
		{Provider: "openai-compatible", BaseURL: "not a url", Model: "m"},
	} {
		if _, err := s.check(ctx, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", in, err)
		}
	}

	// An allowed host may be private, and plain http.
	if e, err := s.check(ctx, Settings{Provider: "openai-compatible", BaseURL: "http://Model-Box.lan:1234/v1/", Model: " m "}); err != nil ||
		e.BaseURL != "http://Model-Box.lan:1234/v1" || e.Model != "m" {
		t.Errorf("allowed host: %+v, %v", e, err)
	}
}

func TestKeysAreKeptReplacedAndRemoved(t *testing.T) {
	s, store := newService(t, "model-box.lan")
	ctx := context.Background()
	base := Settings{Provider: "openai-compatible", BaseURL: "http://model-box.lan/v1", Model: "qwen3"}

	in := base
	in.APIKey = ptr("sk-abcdefghijklmnop")

	e, err := s.Set(ctx, 1, in)
	if err != nil || !e.KeySet || e.KeyHint != "…mnop" {
		t.Fatalf("set: %+v, %v", e, err)
	}

	if strings.Contains(string(store.rows[1].sealed), "sk-abc") {
		t.Error("the key is stored in the clear")
	}

	in = base
	in.Model = "qwen3-big"

	if e, _ = s.Set(ctx, 1, in); !e.KeySet || e.Model != "qwen3-big" {
		t.Errorf("no key given keeps it: %+v", e)
	}

	in.APIKey = ptr("")
	if e, _ = s.Set(ctx, 1, in); e.KeySet || e.KeyHint != "" {
		t.Errorf("an empty key removes it: %+v", e)
	}

	if _, err := s.Set(ctx, 1, Settings{Provider: "anthropic", BaseURL: "http://model-box.lan/v1", Model: "claude"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("anthropic without a key: %v", err)
	}

	if err := s.Delete(ctx, 1); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Get(ctx, 1); !errors.Is(err, ErrNotSet) {
		t.Errorf("after delete: %v", err)
	}
}

func TestTheEndpointIsTestedAndUsedByItsUser(t *testing.T) {
	srv, gotKey := openAI(t, "OK")
	host := mustHost(t, srv.URL)

	blocked, _ := newService(t)
	if _, err := blocked.Test(context.Background(), 1, Settings{Provider: "openai-compatible", BaseURL: srv.URL, Model: "m"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a loopback endpoint without the admin's leave: %v", err)
	}

	s, _ := newService(t, host)
	ctx := context.Background()
	in := Settings{Provider: "openai-compatible", BaseURL: srv.URL + "/v1", Model: "m", APIKey: ptr("sk-abcdefghijklmnop")}

	res, err := s.Test(ctx, 1, in)
	if err != nil || res.Reply != "OK" || *gotKey != "Bearer sk-abcdefghijklmnop" {
		t.Fatalf("test: %+v, %v, key %q", res, err, *gotKey)
	}

	if _, err := s.Set(ctx, 1, in); err != nil {
		t.Fatal(err)
	}

	in.APIKey = nil
	*gotKey = ""

	if _, err := s.Test(ctx, 1, in); err != nil || *gotKey != "Bearer sk-abcdefghijklmnop" {
		t.Errorf("a test without a key uses the saved one: %v, %q", err, *gotKey)
	}

	fallback := &named{name: "server-model"}
	r := NewRouter(s, fallback)

	mine := auth.WithUser(ctx, auth.User{ID: 1})
	if resp, err := r.Stream(mine, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}, MaxTokens: 5}, nil); err != nil ||
		resp.Text != "OK" || r.ModelFor(mine) != "m" {
		t.Errorf("user 1 gets their endpoint: %+v, %v, model %q", resp, err, r.ModelFor(mine))
	}

	theirs := auth.WithUser(ctx, auth.User{ID: 2})
	if resp, _ := r.Stream(theirs, llm.Request{}, nil); resp.Text != "server" || r.ModelFor(theirs) != "server-model" {
		t.Errorf("user 2 gets the server's: %+v", resp)
	}

	if resp, _ := r.Stream(ctx, llm.Request{}, nil); resp.Text != "server" {
		t.Errorf("no user gets the server's: %+v", resp)
	}

	if _, err := NewRouter(s, nil).Stream(theirs, llm.Request{}, nil); !errors.Is(err, llm.ErrNoModel) {
		t.Errorf("no server model, none of theirs: %v", err)
	}

	if err := s.Delete(ctx, 1); err != nil {
		t.Fatal(err)
	}

	if resp, _ := r.Stream(mine, llm.Request{}, nil); resp.Text != "server" {
		t.Errorf("after delete, the server's at once: %+v", resp)
	}
}

func TestTheGuardRefusesToDialInside(t *testing.T) {
	srv, _ := openAI(t, "OK")

	client := &http.Client{Transport: guardedTransport(false, nil), Timeout: 5 * time.Second}
	if _, err := client.Get(srv.URL); err == nil || !strings.Contains(err.Error(), "inside the server's network") {
		t.Errorf("dialled loopback: %v", err)
	}

	allowed := &http.Client{Transport: guardedTransport(false, []string{mustHost(t, srv.URL)}), Timeout: 5 * time.Second}
	if resp, err := allowed.Get(srv.URL); err != nil {
		t.Errorf("an allowed host: %v", err)
	} else {
		resp.Body.Close()
	}
}

type named struct{ name string }

func (n *named) Model() string { return n.name }

func (n *named) Stream(context.Context, llm.Request, func(string) error) (llm.Response, error) {
	return llm.Response{Text: "server"}, nil
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}

	return u.Hostname()
}
