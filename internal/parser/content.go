package parser

import (
	"bytes"
	"net/url"
	"strings"

	"codeberg.org/readeck/go-readability/v2"
	"github.com/PuerkitoBio/goquery"
)

func (p *KatanaParser) extractContent(body []byte, base *url.URL, doc *goquery.Document, acc *accumulator) (string, string) {
	article, err := readability.FromReader(bytes.NewReader(body), base)
	if err != nil {
		acc.warn(stageContent, err.Error())
		p.logger.Debugw("readability failed, falling back", "err", err)

		return fallbackTitle(doc), fallbackText(doc)
	}

	if article.Node == nil {
		acc.warn(stageContent, "readability returned no content node")
		return fallbackTitle(doc), fallbackText(doc)
	}

	var buf bytes.Buffer
	if errRender := article.RenderText(&buf); errRender != nil {
		acc.warn(stageContent, errRender.Error())
		return fallbackTitle(doc), fallbackText(doc)
	}

	text := normalizeWhitespace(buf.String())
	if text == "" {
		acc.warn(stageContent, "readability produced empty text")
		return fallbackTitle(doc), fallbackText(doc)
	}

	title := strings.TrimSpace(article.Title())
	if title == "" {
		title = fallbackTitle(doc)
	}

	return title, text
}

func fallbackText(doc *goquery.Document) string {
	if doc == nil {
		return ""
	}

	// Clone: the same doc goes to katana afterwards and must not be mutated.
	stripped := doc.Clone()
	stripped.Find(selectorNonIndexable).Remove()

	return normalizeWhitespace(stripped.Text())
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

func normalizeWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
