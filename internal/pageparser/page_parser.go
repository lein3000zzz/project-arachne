package pageparser

import "regexp"

type PageParser interface {
	ParseLinks(body []byte) []string
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
