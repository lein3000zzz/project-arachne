package urlnorm

import (
	"net/url"
	"testing"
)

func TestCanonicalCollapsesSpellingsOfOneAddress(t *testing.T) {
	groups := map[string][]string{
		"https://example.com/": {
			"https://example.com",
			"https://example.com/",
			"https://EXAMPLE.com",
			"https://example.com.",
			"https://example.com:443/",
			"https://example.com/#top",
			"https://example.com/?",
			"https://user:secret@example.com/",
			"https://example.com/?utm_source=x&utm_medium=y",
			"https://example.com/./",
			"HTTPS://Example.Com",
		},
		"http://example.com/": {
			"http://example.com",
			"http://example.com:80",
			"http://EXAMPLE.COM/",
		},
		"https://example.com/docs/guide?page=2&sort=asc": {
			"https://example.com/docs/guide?page=2&sort=asc",
			"https://example.com/docs/./guide?page=2&sort=asc",
			"https://example.com/docs/x/../guide?page=2&sort=asc",
			"https://example.com/docs/guide?page=2&utm_campaign=z&sort=asc",
			"https://example.com/docs/guide?page=2&fbclid=abc&sort=asc#section",
		},
		"https://github.com/golang": {
			`https://github.com/golang","https://other.example/`,
		},
		"https://xn--e1afmkfd.xn--p1ai/": {
			"https://пример.рф",
			"https://ПРИМЕР.РФ/",
			"https://xn--e1afmkfd.xn--p1ai",
		},
	}

	for want, spellings := range groups {
		for _, raw := range spellings {
			got, ok := Canonical(raw, nil)
			if !ok || got != want {
				t.Errorf("Canonical(%q) = %q, %v; want %q", raw, got, ok, want)
			}
		}
	}
}

func TestCanonicalKeepsWhatOnlyTheServerCanDecide(t *testing.T) {
	distinct := [][2]string{
		{"https://example.com/doc", "https://example.com/doc/"},
		{"http://example.com/", "https://example.com/"},
		{"https://example.com/", "https://www.example.com/"},
		{"https://example.com/a?x=1&y=2", "https://example.com/a?y=2&x=1"},
		{"https://example.com/Path", "https://example.com/path"},
		{"https://example.com:8443/", "https://example.com/"},
	}

	for _, pair := range distinct {
		a, _ := Canonical(pair[0], nil)
		b, _ := Canonical(pair[1], nil)

		if a == b {
			t.Errorf("%q and %q were merged into %q; only a redirect may say they are the same", pair[0], pair[1], a)
		}
	}
}

func TestCanonicalResolvesAgainstTheBase(t *testing.T) {
	base, _ := url.Parse("https://example.com/docs/guide/")

	cases := map[string]string{
		"intro":                  "https://example.com/docs/guide/intro",
		"../api":                 "https://example.com/docs/api",
		"/top":                   "https://example.com/top",
		"//CDN.example.net/a.js": "https://cdn.example.net/a.js",
		"?q=1":                   "https://example.com/docs/guide/?q=1",
	}

	for raw, want := range cases {
		if got, ok := Canonical(raw, base); !ok || got != want {
			t.Errorf("Canonical(%q, base) = %q, %v; want %q", raw, got, ok, want)
		}
	}
}

func TestCanonicalRejectsWhatCannotBeFetched(t *testing.T) {
	for _, raw := range []string{
		"", "   ", "#top", "mailto:a@example.com", "javascript:void(0)", "tel:+1",
		"data:text/plain,hi", "ftp://example.com/", "https://", "/relative-without-base",
		"http://exa mple.com/",
		"https://&/",
		`https://proxy.golang.org,direct"`,
		"https://a.example.com,https://b.example.com",
	} {
		if got, ok := Canonical(raw, nil); ok {
			t.Errorf("Canonical(%q) = %q, want rejected", raw, got)
		}
	}
}

func TestFromUserInput(t *testing.T) {
	cases := map[string]string{
		"example.com":          "https://example.com/",
		"example.com/":         "https://example.com/",
		"  Example.com/docs ":  "https://example.com/docs",
		"http://example.com":   "http://example.com/",
		"https://example.com/": "https://example.com/",
		"localhost:8080/x":     "https://localhost:8080/x",
		"example.com:443":      "https://example.com/",
	}

	for raw, want := range cases {
		if got, ok := FromUserInput(raw); !ok || got != want {
			t.Errorf("FromUserInput(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}

	for _, raw := range []string{"mailto:a@example.com", "javascript:void(0)", "ftp://example.com/", "tel:+15551234", ""} {
		if got, ok := FromUserInput(raw); ok {
			t.Errorf("FromUserInput(%q) = %q, want rejected", raw, got)
		}
	}
}

func TestCanonicalIsIdempotent(t *testing.T) {
	for _, raw := range []string{
		"https://EXAMPLE.com:443/a/./b/../c?utm_x=1&k=v#f",
		"https://пример.рф/путь?q=значение",
		"http://example.com",
	} {
		once, _ := Canonical(raw, nil)
		twice, _ := Canonical(once, nil)

		if once != twice {
			t.Errorf("not idempotent: %q -> %q -> %q", raw, once, twice)
		}
	}
}
