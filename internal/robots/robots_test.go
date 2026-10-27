package robots

import (
	"errors"
	"net/http"
	"testing"
	"web-crawler/internal/networker"

	"go.uber.org/zap"
)

type fakeFetcher struct {
	status int
	body   string
	err    error
	calls  int
}

func (f *fakeFetcher) Fetch(string) (*networker.FetchResult, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}

	return &networker.FetchResult{Status: f.status, Body: []byte(f.body)}, nil
}

func checker(f *fakeFetcher) *Checker {
	return NewChecker(zap.NewNop().Sugar(), f, NewMemoryCache(), "project-arachne", 0)
}

func TestRulesMatchOurProductName(t *testing.T) {
	c := checker(&fakeFetcher{status: 200, body: "User-agent: project-arachne\nDisallow: /private\n\nUser-agent: *\nDisallow: /\n"})

	if c.Allowed("https://example.com/private/page") {
		t.Error("a Disallow in our own group must apply")
	}

	if !c.Allowed("https://example.com/public") {
		t.Error("our own group allows /public; the catch-all group must not apply to us")
	}
}

func TestCatchAllAppliesWhenWeAreNotNamed(t *testing.T) {
	c := checker(&fakeFetcher{status: 200, body: "User-agent: SomeOtherBot\nDisallow: /\n\nUser-agent: *\nDisallow: /admin\n"})

	if c.Allowed("https://example.com/admin") {
		t.Error("the catch-all group must apply when no group names us")
	}

	if !c.Allowed("https://example.com/docs") {
		t.Error("another bot's ban must not apply to us")
	}
}

func TestAvailabilityFollowsRFC9309(t *testing.T) {
	cases := []struct {
		name    string
		fetcher *fakeFetcher
		allowed bool
		cached  bool
	}{
		{"missing", &fakeFetcher{status: http.StatusNotFound}, true, true},
		{"forbidden", &fakeFetcher{status: http.StatusForbidden, body: "<html>denied</html>"}, true, true},
		{"server error", &fakeFetcher{status: http.StatusServiceUnavailable, body: "<html>down</html>"}, false, false},
		{"network failure", &fakeFetcher{err: errors.New("dial tcp: no such host")}, false, false},
	}

	for _, tc := range cases {
		c := checker(tc.fetcher)

		if got := c.Allowed("https://example.com/page"); got != tc.allowed {
			t.Errorf("%s: allowed = %v, want %v", tc.name, got, tc.allowed)
		}

		c.Allowed("https://example.com/other")

		if cached := tc.fetcher.calls == 1; cached != tc.cached {
			t.Errorf("%s: robots.txt fetched %d times; cached = %v, want %v", tc.name, tc.fetcher.calls, cached, tc.cached)
		}
	}
}
