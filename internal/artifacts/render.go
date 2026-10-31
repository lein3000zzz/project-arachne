package artifacts

import (
	"bytes"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"web-crawler/internal/answer"
	"web-crawler/internal/cite"
	"web-crawler/internal/uploads"

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

// Heading is an entry of a rendered artifact's table of contents.
type Heading struct {
	ID   string `json:"id"`
	Text string `json:"text"`
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
	answer.Supported: "✓", answer.Partly: "~", answer.Unsupported: "✗", answer.Unchecked: "?",
}

// Render turns a version into the HTML of its page - each section under its
// heading, citations linking to the source list, a mark after each checked
// claim - and the table of contents.
func Render(v Version) (string, []Heading) {
	var (
		b   strings.Builder
		toc []Heading
	)

	for i, sec := range v.Sections {
		id := "s" + strconv.Itoa(i+1)

		fmt.Fprintf(&b, `<section class="part" id="%s">`, id)

		if sec.Heading != "" {
			toc = append(toc, Heading{ID: id, Text: sec.Heading})
			fmt.Fprintf(&b, "<h2>%s</h2>", html.EscapeString(sec.Heading))
		}

		switch sec.State {
		case StateDone:
			b.WriteString(renderText(i, sec, len(v.Sources)))
		case StateThin:
			fmt.Fprintf(&b, `<div class="missing"><p>%s It was to cover: %s</p>`+
				`<button type="button" class="action small gather" data-focus="%s">Gather for this section</button></div>`,
				html.EscapeString(sec.Note), html.EscapeString(sec.Focus), html.EscapeString(sec.Focus))
		case StateFailed:
			fmt.Fprintf(&b, `<p class="note">Not written: %s</p>`, html.EscapeString(sec.Note))
		default:
			fmt.Fprintf(&b, `<p class="note pending">%s…</p>`, html.EscapeString(string(sec.State)))
		}

		b.WriteString("</section>")
	}

	if len(v.Sources) > 0 {
		toc = append(toc, Heading{ID: "sources", Text: "Sources"})
		b.WriteString(`<section class="part" id="sources"><h2>Sources</h2><ol class="refs">`)

		for _, src := range v.Sources {
			href, where := src.URL, hostOf(src.URL)
			if id, ok := uploads.ID(src.URL); ok {
				href, where = "/uploads/"+id, "your file"
			}

			title := src.Title
			if title == "" {
				title = src.URL
			}

			fmt.Fprintf(&b, `<li id="ref-%d"><a href="%s" target="_blank" rel="noopener noreferrer">%s</a> <span class="host">%s</span></li>`,
				src.N, html.EscapeString(href), html.EscapeString(title), html.EscapeString(where))
		}

		b.WriteString("</ol></section>")
	}

	return b.String(), toc
}

// renderText renders a section's Markdown with its citations as links to the
// source list, and marks after its claims when the check covered them all.
func renderText(section int, sec Section, sources int) string {
	parts, claims := answer.Split(sec.Text, sources)
	marked := len(sec.Claims) > 0 && len(sec.Claims) == len(claims)

	var text strings.Builder

	for _, p := range parts {
		if p.Claim < 0 || !marked {
			text.WriteString(p.Text)
			continue
		}

		body := strings.TrimRight(p.Text, " \t\n")
		fmt.Fprintf(&text, "%s%s%d-%d%s%s", body, markOpen, section, p.Claim, markClose, p.Text[len(body):])
	}

	linked := cite.Replace(text.String(), func(ns []int) string {
		var out strings.Builder

		for _, n := range ns {
			if n >= 1 && n <= sources {
				fmt.Fprintf(&out, "[%d](#ref-%d)", n, n)
			} else {
				out.WriteString(`\[?\]`)
			}
		}

		return out.String()
	})

	var out bytes.Buffer
	if err := md.Convert([]byte(linked), &out); err != nil {
		return "<p>" + html.EscapeString(sec.Text) + "</p>"
	}

	return markers.ReplaceAllStringFunc(policy.Sanitize(out.String()), func(m string) string {
		ids := markers.FindStringSubmatch(m)
		i, _ := strconv.Atoi(ids[2])

		verdict := sec.Claims[i].Verdict
		if verdict == "" {
			verdict = answer.Unchecked
		}

		return fmt.Sprintf(`<button type="button" class="mark %s" data-claim="%s-%s" aria-label="%s">%s</button>`,
			verdict, ids[1], ids[2], verdict, verdictMarks[verdict])
	})
}

func hostOf(raw string) string {
	host := raw[strings.Index(raw, "://")+3:]
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}

	return host
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
