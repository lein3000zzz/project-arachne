package parser

import "testing"

func TestNormalizeURL(t *testing.T) {
	base := parseBaseURL("https://example.com/docs/guide/")

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"absolute kept", "https://other.example.org/x", "https://other.example.org/x"},
		{"relative resolved against base", "../api", "https://example.com/docs/api"},
		{"rooted path", "/top", "https://example.com/top"},
		{"protocol relative takes base scheme", "//cdn.example.net/a.js", "https://cdn.example.net/a.js"},
		{"fragment stripped", "/page#section", "https://example.com/page"},
		{"bare host gets root path", "https://example.com", "https://example.com/"},
		{"empty", "", ""},
		{"anchor only", "#top", ""},
		{"mailto", "mailto:a@example.com", ""},
		{"javascript", "javascript:void(0)", ""},
		{"tel", "tel:+123456", ""},
		{"data uri", "data:text/plain;base64,aGk=", ""},
		{"whitespace trimmed", "  /spaced  ", "https://example.com/spaced"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeURL(tc.raw, base); got != tc.want {
				t.Errorf("normalizeURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestNormalizeURLWithoutBase(t *testing.T) {
	if got := normalizeURL("/relative", nil); got != "" {
		t.Errorf("relative URL with no base = %q, want empty", got)
	}

	if got := normalizeURL("https://example.com/x", nil); got != "https://example.com/x" {
		t.Errorf("absolute URL with no base = %q", got)
	}
}

func TestLooksLikeURL(t *testing.T) {
	yes := []string{"https://example.com", "http://a.b", "//cdn.example.com/x", "/api/v1", "./rel", "../up"}
	no := []string{"", "a plain sentence", "not-a-url", "value", `has"quote`, "trailing space "}

	for _, s := range yes {
		if !looksLikeURL(s) {
			t.Errorf("looksLikeURL(%q) = false, want true", s)
		}
	}

	for _, s := range no {
		if looksLikeURL(s) {
			t.Errorf("looksLikeURL(%q) = true, want false", s)
		}
	}
}
