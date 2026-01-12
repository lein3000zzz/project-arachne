package pageparser

import (
	"regexp"
)

type PageParser interface {
	ParseHTML(body []byte, base string) []string
	ExtractLinksFromJS(baseURL, src string) ([]string, error)
	ExtractLinksFromJSON(baseURL string, data []byte) ([]string, error)
}

var urlRegex = regexp.MustCompile(`https?://[^\s"'<>]+`)

var attrsWithURLs = map[string]struct{}{
	"href":        {},
	"src":         {},
	"srcset":      {},
	"data-src":    {},
	"data-srcset": {},
	"data-href":   {},
	"action":      {},
	"formaction":  {},
	"poster":      {},
	"cite":        {},
	"background":  {},
	"manifest":    {},
	"longdesc":    {},
	"ping":        {},
	"data":        {},
	"codebase":    {},
	"archive":     {},
	"dynsrc":      {},
	"lowsrc":      {},
}
