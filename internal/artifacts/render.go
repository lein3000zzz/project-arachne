package artifacts

import (
	"fmt"
	"html"
	"strconv"
	"strings"
	"web-crawler/internal/answer"
	"web-crawler/internal/markdown"
	"web-crawler/internal/uploads"
)

// Heading is an entry of a rendered artifact's table of contents.
type Heading struct {
	ID   string `json:"id"`
	Text string `json:"text"`
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
			b.WriteString(renderConflicts(sec.Conflicts))
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
// source list, and marks after its claims.
func renderText(section int, sec Section, sources int) string {
	return markdown.Render(sec.Text, markdown.Options{CiteHref: "#ref-%d", Sources: sources, Section: section, Claims: sec.Claims})
}

// renderConflicts is the box of points on which the section's sources
// disagree, as the Look up page shows it under an answer.
func renderConflicts(conflicts []answer.Conflict) string {
	if len(conflicts) == 0 {
		return ""
	}

	head := "Two sources disagree"
	if len(conflicts) > 1 {
		head = fmt.Sprintf("Sources disagree on %d points", len(conflicts))
	}

	var b strings.Builder

	fmt.Fprintf(&b, `<aside class="conflicts"><p class="conflicts-head">%s</p><ul>`, head)

	for _, c := range conflicts {
		fmt.Fprintf(&b, `<li><span class="note">on %s: </span><a href="#ref-%d">[%d]</a> <q>%s</q><span class="note"> — but </span><a href="#ref-%d">[%d]</a> <q>%s</q></li>`,
			html.EscapeString(c.About), c.A, c.A, html.EscapeString(c.QuoteA), c.B, c.B, html.EscapeString(c.QuoteB))
	}

	b.WriteString("</ul></aside>")

	return b.String()
}

func hostOf(raw string) string {
	host := raw[strings.Index(raw, "://")+3:]
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}

	return host
}
