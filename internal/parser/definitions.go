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

// Variables, not constants, only so tests can lower them.
var (
	// A body past this is parsed in part: a DOM costs many times the page's
	// bytes, and every crawl worker may be parsing at once. A page has said
	// what it has to say long before this.
	maxParseBytes = 4 << 20
	// JSON decodes into maps many times its size; a cut one would not decode,
	// so past this its links are not looked for.
	maxJSONBytes = 4 << 20
)

type bodyKind int

const (
	kindHTML bodyKind = iota
	kindJSON
	kindJS
	kindText
	// PDFs, images, archives: nothing here reads them, and their bytes are no
	// one's text.
	kindBinary
)

const htmlSniffLimit = 1024

// Every chunk embeds the title, and the vector store bounds chunk text in bytes.
const maxTitleRunes = 512

const maxJSONDepth = 32
