package parser

const tracerName = "parser"

const (
	stageContent = "content"
	stageLinks   = "links"
	stageJSON    = "json"
)

const (
	contentTypeHTML       = "text/html"
	contentTypeXHTML      = "application/xhtml+xml"
	contentTypeJSON       = "application/json"
	contentTypeJavaScript = "javascript"
	contentTypeText       = "text/plain"

	suffixJSON       = "+json"
	suffixECMAScript = "ecmascript"

	headerContentType = "Content-Type"
)

const selectorHyperlinks = `a[href], area[href], frame[src], iframe[src]`

const selectorNonIndexable = `script, style, noscript, template, iframe, svg, ` +
	`nav, footer, aside, form, [role="banner"], [role="navigation"], [role="complementary"], [aria-hidden="true"]`

const (
	attrHref = "href"
	attrSrc  = "src"
)

type bodyKind int

const (
	kindHTML bodyKind = iota
	kindJSON
	kindJS
	kindText
)

const htmlSniffLimit = 1024

const maxJSONDepth = 32
