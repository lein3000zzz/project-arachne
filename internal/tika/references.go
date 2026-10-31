package tika

import (
	"regexp"
	"sort"
	"strings"
	"web-crawler/internal/domain/data"
)

var (
	markdownLink = regexp.MustCompile(`\[([^\]]*)\]\((https?://[^)\s]+)\)`)
	bareLink     = regexp.MustCompile(`https?://[^\s<>()\[\]"]+`)
	// Papers cite by identifier as often as by link: ResNet's reference list
	// has nine arXiv identifiers and no URLs.
	arxivID = regexp.MustCompile(`(?i)\barxiv:\s?(\d{4}\.\d{4,5})(?:v\d+)?`)
	doi     = regexp.MustCompile(`\b10\.\d{4,9}/[^\s"<>]+`)
	// The rest of a link a PDF broke at a line end - ".../master/ examples/rag/",
	// "forum?id= H1gx1CNKPH": a piece with a slash, dot, digit or the like in
	// it, which a word after a link has not.
	// "[12] K. He, ..." or "12. K. He, ...": an entry of a numbered reference
	// list, which starts with who wrote what.
	numberedEntry = regexp.MustCompile(`^(?:\[\d{1,4}\]|\d{1,4}\.) `)
	latexEscape   = regexp.MustCompile(`\\([&%$#_{}])`)
	linkRest      = regexp.MustCompile(`^[ \n]([^\s<>()\[\]"]*[/.\-_=?&%#\d][^\s<>()\[\]"]*)`)
)

const (
	maxLabelRunes = 200
	// How much of the label comes before the link: a citation names the work
	// before it gives the link.
	labelLead = 150
)

type found struct {
	at, end int
	url     string
}

// references lists what the text points to, in order, each once: its links,
// and the works it cites by arXiv identifier or DOI.
func references(text string) []data.Reference {
	var out []data.Reference

	seen := map[string]int{}

	for _, par := range strings.Split(text, "\n\n") {
		for _, f := range linksIn(par) {
			l := label(par, f.at)

			i, ok := seen[f.url]
			switch {
			case !ok:
				seen[f.url] = len(out)
				out = append(out, data.Reference{URL: f.url, Label: l})
			case out[i].Label == f.url:
				// A PDF's link annotation can come before the entry that cites it.
				out[i].Label = l
			}
		}
	}

	return out
}

func linksIn(par string) []found {
	var out []found

	// A bare link or an identifier inside a link already found is the same
	// reference: the URL of [text](url), the DOI in a doi.org link.
	add := func(at, end int, url string) {
		for _, f := range out {
			if at >= f.at && at < f.end {
				return
			}
		}

		out = append(out, found{at: at, end: end, url: url})
	}

	for _, m := range markdownLink.FindAllStringSubmatchIndex(par, -1) {
		add(m[0], m[1], trimLink(par[m[4]:m[5]]))
	}

	for _, m := range bareLink.FindAllStringIndex(par, -1) {
		link, end := unbroken(par, m[0], m[1])
		add(m[0], end, trimLink(link))
	}

	for _, m := range arxivID.FindAllStringSubmatchIndex(par, -1) {
		add(m[0], m[1], "https://arxiv.org/abs/"+par[m[2]:m[3]])
	}

	for _, m := range doi.FindAllStringIndex(par, -1) {
		id, end := unbroken(par, m[0], m[1])
		add(m[0], end, "https://doi.org/"+trimLink(id))
	}

	sort.Slice(out, func(i, j int) bool { return out[i].at < out[j].at })

	return out
}

// unbroken is the link at par[at:end], with its rest if a line end broke it.
// After a full stop the rest must have a slash in it: in a reference list, a
// link's full stop is more often followed by a year than by more of the link.
func unbroken(par string, at, end int) (string, int) {
	link := par[at:end]

	last := link[len(link)-1]
	if strings.IndexByte("/=%-_?&#.", last) < 0 {
		return link, end
	}

	rest := linkRest.FindStringSubmatchIndex(par[end:])
	if rest == nil || last == '.' && !strings.Contains(par[end+rest[2]:end+rest[3]], "/") {
		return link, end
	}

	return link + par[end+rest[2]:end+rest[3]], end + rest[1]
}

// trimLink takes off what the text put around a link: the sentence's full
// stop, a bracket closed after it, LaTeX's escapes.
func trimLink(link string) string {
	link = strings.ReplaceAll(link, `\`, "")

	for link != "" {
		last := link[len(link)-1]

		switch {
		case strings.IndexByte(".,;:!?'*", last) >= 0:
		case last == ')' && strings.Count(link, "(") < strings.Count(link, ")"):
		default:
			return link
		}

		link = link[:len(link)-1]
	}

	return link
}

// label is the paragraph around a reference, on one line, links shown as
// their text, cut at words to about maxLabelRunes: from its start for an entry
// of a reference list, else around the reference.
func label(par string, at int) string {
	whole := []rune(plain(par))
	if len(whole) <= maxLabelRunes {
		return string(whole)
	}

	if numberedEntry.MatchString(string(whole)) {
		return head(whole)
	}

	before, after := []rune(plain(par[:at])), []rune(plain(par[at:]))

	gap := ""
	if at > 0 && strings.ContainsAny(par[at-1:at], " \n\t") {
		gap = " "
	}

	lead := ""

	if len(before) > labelLead {
		cut := string(before[len(before)-labelLead:])
		if i := strings.IndexByte(cut, ' '); i >= 0 {
			cut = cut[i+1:]
		}

		before, lead = []rune(cut), "… "
	}

	return lead + head([]rune(strings.TrimSpace(string(before)+gap+string(after))))
}

// head cuts text to maxLabelRunes at a word.
func head(text []rune) string {
	if len(text) <= maxLabelRunes {
		return string(text)
	}

	cut := string(text[:maxLabelRunes])
	if i := strings.LastIndexByte(cut, ' '); i > 0 {
		cut = cut[:i]
	}

	return cut + " …"
}

func plain(s string) string {
	s = markdownLink.ReplaceAllStringFunc(s, func(link string) string {
		m := markdownLink.FindStringSubmatch(link)
		if strings.TrimSpace(m[1]) == "" {
			return m[2]
		}

		return m[1]
	})

	return oneLine(latexEscape.ReplaceAllString(s, "$1"))
}
