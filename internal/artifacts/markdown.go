package artifacts

import (
	"fmt"
	"strings"
	"web-crawler/internal/answer"
)

// Markdown is the artifact as a document of its own: sections under their
// headings, the sources it cites, and what its check could not bear out - a
// reader of the file alone still sees which claims stand on what.
func Markdown(a Artifact, v Version) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s\n\n", a.Title)

	var doubtful []answer.Claim

	for _, sec := range v.Sections {
		if sec.Heading != "" && a.Kind == KindReport {
			fmt.Fprintf(&b, "## %s\n\n", sec.Heading)
		}

		switch sec.State {
		case StateDone:
			b.WriteString(strings.TrimSpace(sec.Text))
			b.WriteString("\n\n")
		case StateThin:
			fmt.Fprintf(&b, "> Not written: %s It was to cover: %s\n\n", sec.Note, sec.Focus)
		default:
			fmt.Fprintf(&b, "> Not written: %s\n\n", sec.Note)
		}

		for _, c := range sec.Claims {
			if c.Verdict == answer.Unsupported || c.Verdict == answer.Partly {
				doubtful = append(doubtful, c)
			}
		}
	}

	if len(v.Sources) > 0 {
		b.WriteString("## Sources\n\n")

		for _, src := range v.Sources {
			title := src.Title
			if title == "" {
				title = src.URL
			}

			fmt.Fprintf(&b, "%d. [%s](<%s>)\n", src.N, escapeLinkText(title), src.URL)
		}

		b.WriteString("\n")
	}

	if len(doubtful) > 0 {
		b.WriteString("## Not borne out by the cited sources\n\n")

		for _, c := range doubtful {
			fmt.Fprintf(&b, "- %s (%s)\n", c.Text, c.Verdict)
		}

		b.WriteString("\n")
	}

	return strings.TrimRight(b.String(), "\n") + "\n"
}

var linkTextEscaper = strings.NewReplacer(`[`, `\[`, `]`, `\]`)

func escapeLinkText(s string) string {
	return linkTextEscaper.Replace(oneLine(s))
}
