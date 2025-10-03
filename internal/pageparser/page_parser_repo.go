package pageparser

import (
	"bytes"
	"errors"
	"net/url"
	"strings"

	"go.uber.org/zap"
	"golang.org/x/net/html"
)

type ParserBasic struct {
	Logger *zap.SugaredLogger
}

func NewParserRepo(logger *zap.SugaredLogger) *ParserBasic {
	return &ParserBasic{
		Logger: logger,
	}
}

var (
	ErrEmptyURL = errors.New("empty URL")
)

func (p *ParserBasic) ParseLinks(body []byte, base string) []string {
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

	for len(url) > 0 {
		last := url[len(url)-1]
		if strings.ContainsRune(".,;:!?)\"]}'", rune(last)) {
			url = url[:len(url)-1]
			continue
		}
		break
	}

	url = strings.TrimSpace(url)
	if url == "" {
		return ""
	}
	return url
}

func (p *ParserBasic) checkAllowedPrefixes(url string) bool {
	urlLower := strings.ToLower(url)
	allowedPrefixes := []string{"http://", "https://", "/", "./", "../"}
	hasAllowed := false
	for _, prefix := range allowedPrefixes {
		if strings.HasPrefix(urlLower, prefix) {
			hasAllowed = true
			break
		}
	}
	return hasAllowed
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

func (p *ParserBasic) regexFallback(body []byte, seen map[string]struct{}, base *url.URL) {
	for _, regexMatch := range urlRegex.FindAll(body, -1) {
		foundURL := strings.TrimSpace(string(regexMatch))
		for len(foundURL) > 0 {
			last := foundURL[len(foundURL)-1]
			if strings.ContainsRune(".,;:!?)\"]}'", rune(last)) {
				foundURL = foundURL[:len(foundURL)-1]
				continue
			}
			break
		}

		if foundURL == "" {
			continue
		}

		if urlNormalized := p.normalizeURL(foundURL); urlNormalized != "" {
			p.resolveAndAdd(urlNormalized, seen, base)
		}
	}
}

func (p *ParserBasic) resolveAndAdd(raw string, seen map[string]struct{}, base *url.URL) {
	parsed, err := url.Parse(raw)
	if err != nil {
		p.Logger.Warnw("failed to parse url", "raw", raw, "err", err)
		return
	}

	if !parsed.IsAbs() && base != nil {
		parsed = base.ResolveReference(parsed)
	}

	if parsed.Path == "" {
		parsed.Path = "/"
	}

	// Чтобы /a и /a/ считалось как одно и то же, что не совсем корректно, но у этого могут быть юзкейсы
	// if parsed.Path != "/" && strings.HasSuffix(parsed.Path, "/") {
	// 	parsed.Path = strings.TrimRight(parsed.Path, "/")
	// }

	p.Logger.Infow("resolved url", "url", parsed.String())
	seen[parsed.String()] = struct{}{}
}
