// Package endpoints keeps the chat model each user can bring: an OpenAI- or
// Anthropic-compatible endpoint, with its key sealed at rest. A request made
// for a user goes to their endpoint when they have one, else to the server's
// model.
package endpoints

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
	"web-crawler/internal/llm"
)

var (
	ErrNotSet      = errors.New("no endpoint of your own")
	ErrInvalid     = errors.New("invalid endpoint")
	ErrUnavailable = errors.New("own endpoints are not available: the server has no key to seal them with (ENDPOINT_KEY)")
	ErrTest        = errors.New("the endpoint did not answer")
)

var providers = []string{llm.ProviderOpenAICompatible, llm.ProviderOpenAI, llm.ProviderAnthropic}

// Endpoint is a user's endpoint as shown back to them: the key never is.
type Endpoint struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	Model    string `json:"model"`
	KeySet   bool   `json:"key_set"`
	// The key's last characters, to tell keys apart.
	KeyHint   string    `json:"key_hint,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Settings is what a user submits. A nil key keeps the saved one; an empty
// one removes it.
type Settings struct {
	Provider string  `json:"provider"`
	BaseURL  string  `json:"base_url"`
	Model    string  `json:"model"`
	APIKey   *string `json:"api_key,omitempty"`
}

type Store interface {
	// ErrNotSet when the user has none.
	Get(ctx context.Context, userID string) (Endpoint, []byte, error)
	Set(ctx context.Context, userID string, e Endpoint, sealedKey []byte) error
	Delete(ctx context.Context, userID string) error
}

type Config struct {
	// Let endpoints reach loopback and private addresses: only where every
	// user is trusted with the server's network.
	AllowPrivate bool
	// Hosts reachable whatever their address - a model server on the host,
	// host.docker.internal - and the only ones plain http may be used with.
	AllowedHosts []string
	TestTimeout  time.Duration
	MaxRetries   int
}

type Service struct {
	store     Store
	seal      *sealer
	cfg       Config
	transport http.RoundTripper

	mu      sync.Mutex
	clients map[string]cached
}

type cached struct {
	client llm.Client
	at     time.Time
}

// A change made through this process is seen at once; the expiry only covers
// a row changed by hand.
const cacheFor = 5 * time.Minute

// New takes the 32-byte key that seals users' API keys.
func New(store Store, key []byte, cfg Config) (*Service, error) {
	seal, err := newSealer(key)
	if err != nil {
		return nil, err
	}

	for i, h := range cfg.AllowedHosts {
		cfg.AllowedHosts[i] = strings.ToLower(strings.TrimSpace(h))
	}

	return &Service{store: store, seal: seal, cfg: cfg, transport: guardedTransport(cfg.AllowPrivate, cfg.AllowedHosts),
		clients: map[string]cached{}}, nil
}

func (s *Service) Get(ctx context.Context, userID string) (Endpoint, error) {
	e, _, err := s.store.Get(ctx, userID)
	return e, err
}

// Set checks and saves the user's endpoint; the next request they make uses
// it.
func (s *Service) Set(ctx context.Context, userID string, in Settings) (Endpoint, error) {
	e, err := s.check(ctx, in)
	if err != nil {
		return Endpoint{}, err
	}

	_, sealed, err := s.store.Get(ctx, userID)
	if err != nil && !errors.Is(err, ErrNotSet) {
		return Endpoint{}, err
	}

	if in.APIKey != nil {
		sealed = nil
		if key := strings.TrimSpace(*in.APIKey); key != "" {
			sealed = s.seal.seal(key, userID)
		}
	}

	if sealed != nil {
		key, err := s.seal.open(sealed, userID)
		if err != nil {
			return Endpoint{}, err
		}

		e.KeySet, e.KeyHint = true, hint(key)
	}

	if e.Provider == llm.ProviderAnthropic && !e.KeySet {
		return Endpoint{}, fmt.Errorf("%w: the Anthropic API needs a key", ErrInvalid)
	}

	if err := s.store.Set(ctx, userID, e, sealed); err != nil {
		return Endpoint{}, err
	}

	s.forget(userID)

	return s.Get(ctx, userID)
}

// Delete goes back to the server's model.
func (s *Service) Delete(ctx context.Context, userID string) error {
	defer s.forget(userID)
	return s.store.Delete(ctx, userID)
}

type TestResult struct {
	Model   string  `json:"model"`
	Reply   string  `json:"reply"`
	Seconds float64 `json:"seconds"`
}

// Test sends one short request with these settings - the saved key when none
// is given - without saving anything.
func (s *Service) Test(ctx context.Context, userID string, in Settings) (TestResult, error) {
	e, err := s.check(ctx, in)
	if err != nil {
		return TestResult{}, err
	}

	key := ""

	if in.APIKey != nil {
		key = strings.TrimSpace(*in.APIKey)
	} else if _, sealed, err := s.store.Get(ctx, userID); err == nil && sealed != nil {
		if key, err = s.seal.open(sealed, userID); err != nil {
			return TestResult{}, err
		}
	}

	client, err := s.newClient(e, key, 0)
	if err != nil {
		return TestResult{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.TestTimeout)
	defer cancel()

	started := time.Now()

	resp, err := client.Stream(ctx, llm.Request{
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: "Reply with the word OK and nothing else."}},
		MaxTokens:   16,
		Temperature: new(float64),
	}, nil)
	if err != nil {
		return TestResult{}, fmt.Errorf("%w: %w", ErrTest, err)
	}

	return TestResult{Model: e.Model, Reply: strings.TrimSpace(resp.Text), Seconds: time.Since(started).Seconds()}, nil
}

// Client is the user's own model, if they set one.
func (s *Service) Client(ctx context.Context, userID string) (llm.Client, bool, error) {
	s.mu.Lock()
	c, ok := s.clients[userID]
	s.mu.Unlock()

	if ok && time.Since(c.at) < cacheFor {
		return c.client, c.client != nil, nil
	}

	e, sealed, err := s.store.Get(ctx, userID)

	switch {
	case errors.Is(err, ErrNotSet):
		c = cached{at: time.Now()}
	case err != nil:
		return nil, false, err
	default:
		key := ""
		if sealed != nil {
			if key, err = s.seal.open(sealed, userID); err != nil {
				return nil, false, err
			}
		}

		client, err := s.newClient(e, key, s.cfg.MaxRetries)
		if err != nil {
			return nil, false, err
		}

		c = cached{client: client, at: time.Now()}
	}

	s.mu.Lock()
	s.clients[userID] = c
	s.mu.Unlock()

	return c.client, c.client != nil, nil
}

func (s *Service) forget(userID string) {
	s.mu.Lock()
	delete(s.clients, userID)
	s.mu.Unlock()
}

func (s *Service) newClient(e Endpoint, key string, retries int) (llm.Client, error) {
	return llm.New(llm.Config{Provider: e.Provider, BaseURL: e.BaseURL, APIKey: key, Model: e.Model, MaxRetries: retries,
		Transport: s.transport})
}

const (
	maxURLRunes   = 500
	maxModelRunes = 200
)

// check validates settings: a known provider, a model, and an address the
// server may call - https, and not inside its own network, unless the admin
// allowed the host.
func (s *Service) check(ctx context.Context, in Settings) (Endpoint, error) {
	e := Endpoint{Provider: strings.TrimSpace(in.Provider), BaseURL: strings.TrimRight(strings.TrimSpace(in.BaseURL), "/"),
		Model: strings.TrimSpace(in.Model)}

	if !slices.Contains(providers, e.Provider) {
		return Endpoint{}, fmt.Errorf("%w: the provider is one of %s", ErrInvalid, strings.Join(providers, ", "))
	}

	if e.Model == "" || utf8.RuneCountInString(e.Model) > maxModelRunes || strings.ContainsFunc(e.Model, unicode.IsControl) {
		return Endpoint{}, fmt.Errorf("%w: name the model, in at most %d characters", ErrInvalid, maxModelRunes)
	}

	u, err := url.Parse(e.BaseURL)
	if err != nil || utf8.RuneCountInString(e.BaseURL) > maxURLRunes || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Endpoint{}, fmt.Errorf("%w: the base URL is an address like https://api.openai.com/v1", ErrInvalid)
	}

	host := strings.ToLower(u.Hostname())
	allowed := s.cfg.AllowPrivate || slices.Contains(s.cfg.AllowedHosts, host)

	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && allowed:
	default:
		return Endpoint{}, fmt.Errorf("%w: use https - plain http would send your key unencrypted", ErrInvalid)
	}

	if !allowed {
		addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(addrs) == 0 {
			return Endpoint{}, fmt.Errorf("%w: %s could not be found", ErrInvalid, host)
		}

		for _, a := range addrs {
			if !public(a) {
				return Endpoint{}, fmt.Errorf("%w: %s is inside the server's network", ErrInvalid, host)
			}
		}
	}

	return e, nil
}

func hint(key string) string {
	r := []rune(key)
	if len(r) < 12 {
		return ""
	}

	return "…" + string(r[len(r)-4:])
}

func public(a netip.Addr) bool {
	a = a.Unmap()

	return a.IsValid() && a.IsGlobalUnicast() && !a.IsPrivate() && !a.IsLoopback() && !a.IsLinkLocalUnicast() &&
		!sharedSpace.Contains(a) && !thisNetwork.Contains(a)
}

var (
	// Carrier-grade NAT: not reachable from the internet, so not a public API.
	sharedSpace = netip.MustParsePrefix("100.64.0.0/10")
	thisNetwork = netip.MustParsePrefix("0.0.0.0/8")
)
