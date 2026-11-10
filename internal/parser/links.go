package parser

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
	katanaparser "github.com/projectdiscovery/katana/pkg/engine/parser"
	katananav "github.com/projectdiscovery/katana/pkg/navigation"
	"mvdan.cc/xurls/v2"
)

var looseURLs = xurls.Strict()

func newLinkParser() *katanaparser.Parser {
	p := katanaparser.NewResponseParser()
	// jsluice stays off: it parses each inline script with a pure-Go
	// tree-sitter, whose GLR stacks can explode on ordinary scripts - a 110 KB
	// one on a 245 KB blog page took the crawler past 4 GiB in seconds
	// (2026-11-09), and the crash put the same task back in a loop. Scripts
	// are still read for URLs by katana's regular expressions.
	p.InitWithOptions(&katanaparser.Options{
		ScrapeJSResponses: true,
		DisableRedirects:  true,
	})

	return p
}

func (p *KatanaParser) extractLinks(body []byte, base *url.URL, mediaType string, doc *goquery.Document, acc *accumulator) {
	if doc != nil {
		extractHyperlinks(doc, acc)
	}

	text := string(body)
	if doc != nil && len(doc.Nodes) > 0 {
		text = ScanText(doc.Nodes[0])
	}

	if base != nil {
		for _, request := range p.links.ParseResponse(katanaResponse(text, base, mediaType, doc)) {
			if request != nil {
				acc.add(request.URL, LinkOther)
			}
		}
	}

	for _, raw := range looseURLs.FindAllString(text, -1) {
		acc.add(raw, LinkOther)
	}
}

func extractHyperlinks(doc *goquery.Document, acc *accumulator) {
	doc.Find(selectorHyperlinks).Each(func(_ int, selection *goquery.Selection) {
		target, exists := selection.Attr(attrHref)
		if !exists {
			target, exists = selection.Attr(attrSrc)
		}

		if !exists {
			return
		}

		acc.add(target, LinkPage)
	})
}

func katanaResponse(body string, base *url.URL, mediaType string, doc *goquery.Document) *katananav.Response {
	header := http.Header{}
	header.Set(headerContentType, mediaType)

	return &katananav.Response{
		Resp: &http.Response{
			Request: &http.Request{URL: base},
			Header:  header,
		},
		Reader:       doc,
		Body:         body,
		StatusCode:   http.StatusOK,
		RootHostname: hostOf(base),
	}
}

func documentFrom(body []byte, acc *accumulator) *goquery.Document {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		acc.warn(stageLinks, err.Error())
		return nil
	}

	return doc
}

func looksLikeURL(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 2048 || strings.ContainsAny(s, " \t\r\n\"'<>{}|\\^`") {
		return false
	}

	switch {
	case strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "https://"), strings.HasPrefix(s, "//"):
		return true
	case strings.HasPrefix(s, "/"), strings.HasPrefix(s, "./"), strings.HasPrefix(s, "../"):
		return true
	}

	return false
}
