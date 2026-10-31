package runs

import (
	"strings"
	"testing"
	"time"
)

func TestScopeFor(t *testing.T) {
	cases := []struct {
		start, scope, want string
		ok                 bool
	}{
		{"https://go.dev/doc/tutorial/intro", "", "https://go.dev/", true},
		{"https://go.dev/doc/tutorial/intro", "site", "https://go.dev/", true},
		{"https://go.dev/doc/tutorial/intro", "section", "https://go.dev/doc/tutorial/", true},
		{"https://go.dev/doc/", "section", "https://go.dev/doc/", true},
		{"https://go.dev/", "section", "https://go.dev/", true},
		{"https://go.dev/doc/", "anywhere", "", true},
		{"https://go.dev/doc/x", "go.dev/doc/", "https://go.dev/doc/", true},
		{"https://go.dev/doc/x", "https://go.dev/blog/", "", false},
	}

	for _, tc := range cases {
		got, err := scopeFor(tc.start, tc.scope)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("scopeFor(%q, %q) = %q, %v; want %q", tc.start, tc.scope, got, err, tc.want)
		}
	}
}

func TestBuildValidates(t *testing.T) {
	s := &Service{limits: Limits{DefaultDepth: 3, MaxDepth: 5, DefaultLinks: 100, MaxLinks: 2000}}

	run, err := s.build(Request{URL: "go.dev/doc"})
	if err != nil {
		t.Fatal(err)
	}

	if run.StartURL != "https://go.dev/doc" || run.Scope != "https://go.dev/" || run.MaxDepth != 3 || run.MaxLinks != 100 || len(run.ID) != 40 || run.UseCacheFlag {
		t.Errorf("run = %+v", run)
	}

	for _, req := range []Request{
		{URL: "mailto:a@b.c"},
		{URL: "go.dev", Depth: 6},
		{URL: "go.dev", Links: 2001},
		{URL: "go.dev", Depth: -1},
		// An uploaded file's address: only a lab's upload may crawl it, and an
		// upload only that.
		{URL: "https://uploads.arachne.invalid/" + strings.Repeat("ab", 20)},
		{URL: "go.dev", Upload: true},
	} {
		if _, err := s.build(req); err == nil {
			t.Errorf("%+v must be rejected", req)
		}
	}

	if _, err := s.build(Request{URL: "https://uploads.arachne.invalid/" + strings.Repeat("ab", 20), Upload: true, Depth: 1, Links: 1}); err != nil {
		t.Errorf("a lab's upload: %v", err)
	}
}

func TestStatus(t *testing.T) {
	s := &Store{stallAfter: 10 * 60e9}
	now := mustTime("2026-10-27T12:00:00Z")
	started := mustTime("2026-10-27T11:55:00Z")
	old := mustTime("2026-10-27T11:00:00Z")

	cases := []struct {
		run  Run
		want Status
	}{
		{Run{}, StatusQueued},
		{Run{StartedAt: &started}, StatusRunning},
		{Run{StartedAt: &old}, StatusStalled},
		{Run{StartedAt: &old, LastFetch: &started}, StatusRunning},
		{Run{StartedAt: &old, FinishedAt: &started}, StatusFinished},
		{Run{FinishedAt: &started, Note: "could not be queued"}, StatusDropped},
		{Run{StartedAt: &old, FinishedAt: &started, DocsQueued: 5, DocsIndexed: 3}, StatusIndexing},
		{Run{StartedAt: &old, FinishedAt: &started, DocsQueued: 5, DocsIndexed: 4, DocsFailed: 1}, StatusFinished},
		{Run{StartedAt: &old, FinishedAt: &old, DocsQueued: 5, DocsIndexed: 3}, StatusFinished},
		{Run{StartedAt: &old, FinishedAt: &old, DocsQueued: 5, DocsIndexed: 3, IndexedAt: &started}, StatusIndexing},
	}

	for _, tc := range cases {
		if got := s.status(tc.run, now); got != tc.want {
			t.Errorf("%+v: %s, want %s", tc.run, got, tc.want)
		}
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}

	return t
}
