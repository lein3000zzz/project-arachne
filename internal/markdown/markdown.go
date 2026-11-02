// Package markdown renders what a model wrote - an answer, a report's section
// - as HTML safe to put in a page: citations linked to the sources, code
// highlighted, a mark after each checked claim.
package markdown

import (
	"bytes"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"web-crawler/internal/answer"
	"web-crawler/internal/cite"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

type Options struct {
	// Where citation n links, as a format: "#src-%d", "#ref-%d".
	CiteHref string
	Sources  int
	// Claim marks are named <Section>-<claim>.
	Section int
	// The text's claims in the order answer.Split finds them, with their
	// verdicts - none yet is a pending mark. Nil: no marks.
	Claims []answer.Claim
}

// Raw HTML in the Markdown is dropped (goldmark's default): the text comes
// from a model reading web pages.
var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(renderer.WithNodeRenderers(util.Prioritized(codeRenderer{}, 100))),
)

// A second line of defence, and target="_blank" on links out.
var policy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^[a-z0-9 -]+$`)).OnElements("span", "pre", "code")
	p.AddTargetBlankToFullyQualifiedLinks(true)

	return p
}()

// A claim's mark goes where its id sits between these: private-use
// characters no model writes and neither goldmark nor bluemonday touches.
// They are swapped for the marks after sanitising, so the marks are the only
// markup not sanitised - and it is ours.
const markOpen, markClose = "", ""

var markers = regexp.MustCompile(markOpen + `(\d+)-(\d+)` + markClose)

var verdictMarks = map[answer.Verdict]string{
	answer.Supported: "✓", answer.Partly: "~", answer.Unsupported: "✗", answer.Unchecked: "?", "": "·",
}

// Render renders text. Marks go after its claims only when the claims given
// are the ones the text holds: no marks rather than marks on the wrong
// sentences.
func Render(text string, o Options) string {
	parts, claims := answer.Split(text, o.Sources)
	marked := o.Claims != nil && len(o.Claims) == len(claims)

	var b strings.Builder

	for _, p := range parts {
		if p.Claim < 0 || !marked {
			b.WriteString(p.Text)
			continue
		}

		// A table row's mark goes in its last cell: past the closing pipe, a
		// table drops it.
		at := len(strings.TrimRight(p.Text, " \t\n"))
		if strings.HasSuffix(p.Text[:at], "|") {
			at = len(strings.TrimRight(p.Text[:at-1], " \t"))
		}

		fmt.Fprintf(&b, "%s%s%d-%d%s%s", p.Text[:at], markOpen, o.Section, p.Claim, markClose, p.Text[at:])
	}

	linked := cite.Replace(b.String(), func(ns []int) string {
		var out strings.Builder

		for _, n := range ns {
			if n >= 1 && n <= o.Sources {
				fmt.Fprintf(&out, "[%d]("+o.CiteHref+")", n, n)
			} else {
				out.WriteString(`\[?\]`)
			}
		}

		return out.String()
	})

	var out bytes.Buffer
	if err := md.Convert([]byte(linked), &out); err != nil {
		return "<p>" + html.EscapeString(text) + "</p>"
	}

	return markers.ReplaceAllStringFunc(policy.Sanitize(out.String()), func(m string) string {
		ids := markers.FindStringSubmatch(m)
		i, _ := strconv.Atoi(ids[2])

		verdict := o.Claims[i].Verdict

		label := string(verdict)
		if verdict == "" {
			label = "pending"
		}

		return fmt.Sprintf(`<button type="button" class="mark %s" data-claim="%s-%s" aria-label="%s">%s</button>`,
			label, ids[1], ids[2], label, verdictMarks[verdict])
	})
}

type codeRenderer struct{}

func (codeRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindFencedCodeBlock, renderCode)
	reg.Register(ast.KindCodeBlock, renderCode)
}

// Token classes, not inline colours: the site's stylesheet colours them for
// light and dark alike.
var codeFormatter = chromahtml.New(chromahtml.WithClasses(true))

// renderCode highlights a code block in the language its fence names, and
// leaves it plain when it names none: guessing a language is often wrong.
func renderCode(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkSkipChildren, nil
	}

	var code strings.Builder

	lines := node.Lines()
	for i := range lines.Len() {
		seg := lines.At(i)
		code.Write(seg.Value(source))
	}

	lexer := lexers.Fallback
	if fenced, ok := node.(*ast.FencedCodeBlock); ok {
		if l := lexers.Get(string(fenced.Language(source))); l != nil && fenced.Language(source) != nil {
			lexer = l
		}
	}

	tokens, err := chroma.Coalesce(lexer).Tokenise(nil, code.String())
	if err == nil {
		err = codeFormatter.Format(w, styles.Fallback, tokens)
	}

	if err != nil {
		_, err = fmt.Fprintf(w, "<pre><code>%s</code></pre>", html.EscapeString(code.String()))
	}

	return ast.WalkSkipChildren, err
}
