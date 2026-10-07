package parser

import (
	"net/url"
	"strings"
	"web-crawler/internal/urlnorm"
)

func normalizeURL(raw string, base *url.URL) string {
	canonical, _ := urlnorm.Canonical(raw, base)
	return canonical
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
