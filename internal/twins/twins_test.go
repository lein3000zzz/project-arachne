package twins

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"web-crawler/internal/networker"
	"web-crawler/internal/pages"

	"go.uber.org/zap"
)

type fakeStore struct {
	twins    []pages.Twin
	mu       sync.Mutex
	resolved map[string]string
}

func (f *fakeStore) PlaceholderTwins(_ context.Context, limit int) ([]pages.Twin, error) {
	return f.twins[:min(limit, len(f.twins))], nil
}

func (f *fakeStore) ResolveTwin(_ context.Context, placeholder, identity string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.resolved[placeholder] = identity

	return nil
}

type fakeFetcher map[string]*networker.FetchResult

func (f fakeFetcher) Fetch(url string) (*networker.FetchResult, error) {
	res, ok := f[url]
	if !ok {
		return nil, errors.New("no such host")
	}

	return res, nil
}

type allowAll struct{ except string }

func (a allowAll) Allowed(url string) bool { return url != a.except }

func TestResolve(t *testing.T) {
	store := &fakeStore{resolved: map[string]string{}, twins: []pages.Twin{
		{Placeholder: "https://go.dev/learn", Fetched: []string{"https://go.dev/learn/"}},
		{Placeholder: "https://go.dev/help/", Fetched: []string{"https://go.dev/help"}},
		{Placeholder: "https://example.com/both", Fetched: []string{"https://example.com/both/"}},
		{Placeholder: "https://example.com/flaky", Fetched: []string{"https://example.com/flaky/"}},
		{Placeholder: "https://example.com/down", Fetched: []string{"https://example.com/down/"}},
		{Placeholder: "https://example.com/private", Fetched: []string{"https://example.com/private/"}},
	}}

	fetcher := fakeFetcher{
		"https://go.dev/learn":      {Status: 200, IdentityURL: "https://go.dev/learn/"},
		"https://go.dev/help/":      {Status: 200, IdentityURL: "https://go.dev/help"},
		"https://example.com/both":  {Status: 200, IdentityURL: "https://example.com/both"},
		"https://example.com/flaky": {Status: http.StatusServiceUnavailable, IdentityURL: "https://example.com/flaky"},
	}

	stats, err := Resolve(context.Background(), zap.NewNop().Sugar(), store, fetcher,
		allowAll{except: "https://example.com/private"}, 100, 3)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"https://go.dev/learn":     "https://go.dev/learn/",
		"https://go.dev/help/":     "https://go.dev/help",
		"https://example.com/both": "https://example.com/both",
	}

	if len(store.resolved) != len(want) {
		t.Errorf("resolved = %v, want %v", store.resolved, want)
	}

	for placeholder, identity := range want {
		if store.resolved[placeholder] != identity {
			t.Errorf("%s resolved to %q, want %q", placeholder, store.resolved[placeholder], identity)
		}
	}

	if stats != (Stats{Checked: 3, Folded: 2, Skipped: 3}) {
		t.Errorf("stats = %+v: a 503, a dead host and a robots.txt ban are retried next run, not recorded", stats)
	}
}
