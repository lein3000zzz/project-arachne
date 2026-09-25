package parser

import (
	"bytes"
	"net/url"
	"strings"

	"codeberg.org/readeck/go-readability/v2"
	"codeberg.org/readeck/go-readability/v2/render"
	"github.com/PuerkitoBio/goquery"
)

func (p *KatanaParser) extractContent(body []byte, base *url.URL, doc *goquery.Document, acc *accumulator) (string, string) {
	article, err := readability.FromReader(bytes.NewReader(body), base)
	if err != nil {
		acc.warn(stageContent, err.Error())
		p.logger.Debugw("readability failed, falling back", "err", err)

		return capTitle(fallbackTitle(doc)), fallbackText(doc)
	}

	if article.Node == nil {
		acc.warn(stageContent, "readability returned no content node")
		return capTitle(fallbackTitle(doc)), fallbackText(doc)
	}

	var buf bytes.Buffer
	if errRender := article.RenderText(&buf); errRender != nil {
		acc.warn(stageContent, errRender.Error())
		return capTitle(fallbackTitle(doc)), fallbackText(doc)
	}

	text := normalizeBlocks(buf.String())
	if text == "" {
		acc.warn(stageContent, "readability produced empty text")
		return capTitle(fallbackTitle(doc)), fallbackText(doc)
	}

	title := strings.TrimSpace(article.Title())
	if title == "" {
		title = fallbackTitle(doc)
	}

	return capTitle(title), text
}

func fallbackText(doc *goquery.Document) string {
	if doc == nil {
		return ""
	}

	// Clone: the same doc goes to katana afterwards and must not be mutated.
	stripped := doc.Clone()
	stripped.Find(selectorNonIndexable + ", head").Remove()

	var b strings.Builder
	for _, node := range stripped.Nodes {
		b.WriteString(render.InnerText(node))
		b.WriteString("\n\n")
	}

	return normalizeBlocks(b.String())
}

func fallbackTitle(doc *goquery.Document) string {
	if doc == nil {
		return ""
	}

	if title := strings.TrimSpace(doc.Find("title").First().Text()); title != "" {
		return title
	}

	return strings.TrimSpace(doc.Find("h1").First().Text())
}

func capTitle(title string) string {
	runes := []rune(title)
	if len(runes) <= maxTitleRunes {
		return title
	}

	return strings.TrimSpace(string(runes[:maxTitleRunes]))
}

func normalizeWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func normalizeBlocks(s string) string {
	paragraphs := make([]string, 0)
	lines := make([]string, 0)

	flush := func() {
		if len(lines) > 0 {
			paragraphs = append(paragraphs, strings.Join(lines, "\n"))
			lines = lines[:0]
		}
	}

	for _, line := range strings.Split(s, "\n") {
		line = normalizeWhitespace(line)
		if line == "" {
			flush()
			continue
		}

		lines = append(lines, line)
	}

	flush()

	return strings.Join(paragraphs, "\n\n")
}
