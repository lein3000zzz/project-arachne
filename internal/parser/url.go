package parser

import (
	"net/url"
	"strings"
)

func normalizeURL(raw string, base *url.URL) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") {
		return ""
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}

	if !parsed.IsAbs() {
		if base == nil {
			return ""
		}

		parsed = base.ResolveReference(parsed)
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" {
		return ""
	}

	parsed.Fragment = ""
	parsed.RawFragment = ""

	if parsed.Path == "" {
		parsed.Path = "/"
	}

	return parsed.String()
}

func parseBaseURL(raw string) *url.URL {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !parsed.IsAbs() {
		return nil
	}

	return parsed
}

func hostOf(base *url.URL) string {
	if base == nil {
		return ""
	}

	return base.Hostname()
}
