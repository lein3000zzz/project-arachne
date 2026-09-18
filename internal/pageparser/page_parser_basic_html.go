package pageparser

import (
	"bytes"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

func (p *ParserBasic) ParseHTML(body []byte, base string) []string {
	seen := map[string]struct{}{}
	var links []string

	var baseURL *url.URL
	if base != "" {
		if parsedURL, err := url.Parse(base); err == nil {
			baseURL = parsedURL
		} else if p.Logger != nil {
			p.Logger.Warnw("invalid base URL, skipping resolution", "base", base, "err", err)
		}
	}

	doc, err := html.Parse(bytes.NewReader(body))
	if err == nil {
		p.extractLinksFromNode(doc, seen, baseURL)
	}

	p.regexFallback(body, seen, baseURL)

	for u := range seen {
		links = append(links, u)
	}

	return links
}

func (p *ParserBasic) normalizeURL(url string) string {
	url = strings.TrimSpace(url)
	url = strings.Trim(url, `"'`)

	if url == "" {
		return ""
	}

	if i := strings.Index(url, "#"); i >= 0 {
		url = url[:i]
	}
	url = strings.TrimSpace(url)

	if url == "" {
		return ""
	}
	if strings.HasPrefix(url, "//") {
		url = "http:" + url
	}

	if !p.checkAllowedPrefixes(url) {
		if i := strings.Index(url, ":"); i >= 0 {
			if j := strings.IndexAny(url, "/?#"); j == -1 || i < j {
				return ""
			}
		}
	}

	url = p.trimTrailingPunctuation(url)

	url = strings.TrimSpace(url)
	if url == "" {
		return ""
	}
	return url
}

func (p *ParserBasic) trimTrailingPunctuation(url string) string {
	return strings.TrimRightFunc(url, func(r rune) bool {
		switch r {
		case '.', ',', ';', ':', '!', '?', ')', ']', '}', '\'', '"':
			return true
		default:
			return false
		}
	})
}

func (p *ParserBasic) checkAllowedPrefixes(url string) bool {
	urlLower := strings.ToLower(url)
	allowedPrefixes := []string{"http://", "https://", "/", "./", "../"}
	for _, prefix := range allowedPrefixes {
		if strings.HasPrefix(urlLower, prefix) {
			return true
		}
	}
	return false
}

func (p *ParserBasic) extractFromSrcset(val string, seen map[string]struct{}, base *url.URL) {
	parts := strings.Split(val, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		if urlNormalized := p.normalizeURL(fields[0]); urlNormalized != "" {
			p.resolveAndAdd(urlNormalized, seen, base)
		}
	}
}

func (p *ParserBasic) extractLinksFromNode(node *html.Node, seen map[string]struct{}, base *url.URL) {
	if node == nil {
		return
	}

	if node.Type == html.ElementNode {
		if strings.EqualFold(node.Data, "script") {
			scriptType := attr(node, "type")

			if p.isJavaScriptType(scriptType) {
				if attr(node, "src") == "" {
					var sb strings.Builder

					for c := node.FirstChild; c != nil; c = c.NextSibling {
						if c.Type == html.TextNode {
							sb.WriteString(c.Data)
						}
					}

					js := strings.TrimSpace(sb.String())
					if js != "" {
						baseStr := ""

						if base != nil {
							baseStr = base.String()
						}

						if jsLinks, err := p.ExtractLinksFromJS(baseStr, js); err == nil {
							for _, u := range jsLinks {
								seen[u] = struct{}{}
							}
						} else {
							p.Logger.Debugw("inline JS parse error", "err", err)
						}

					}
				}
			}
		}

		for _, attribute := range node.Attr {
			if _, valid := attrsWithURLs[attribute.Key]; !valid {
				continue
			}

			switch attribute.Key {
			case "srcset", "data-srcset":
				p.extractFromSrcset(attribute.Val, seen, base)
			default:
				if urlNormalized := p.normalizeURL(attribute.Val); urlNormalized != "" {
					p.resolveAndAdd(urlNormalized, seen, base)
				}
			}
		}
	}

	for child := node.FirstChild; child != nil; child = child.NextSibling {
		p.extractLinksFromNode(child, seen, base)
	}
}

func (p *ParserBasic) isJavaScriptType(t string) bool {
	t = strings.TrimSpace(strings.ToLower(t))
	switch t {
	case "", "module", "text/javascript", "application/javascript", "application/ecmascript", "text/ecmascript":
		return true
	default:
		return strings.HasSuffix(t, "javascript") || strings.HasSuffix(t, "ecmascript")
	}
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			return a.Val
		}
	}
	return ""
}

func (p *ParserBasic) regexFallback(body []byte, seen map[string]struct{}, base *url.URL) {
	for _, regexMatch := range urlRegex.FindAll(body, -1) {
		foundURL := strings.TrimSpace(string(regexMatch))

		foundURL = p.trimTrailingPunctuation(foundURL)

		if foundURL == "" {
			continue
		}

		if urlNormalized := p.normalizeURL(foundURL); urlNormalized != "" {
			p.resolveAndAdd(urlNormalized, seen, base)
		}
	}
}
