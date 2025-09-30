package pageparser

import (
	"bytes"
	"errors"
	"net/url"
	"strings"

	"go.uber.org/zap"
	"golang.org/x/net/html"
)

type ParserRepo struct {
	Logger *zap.SugaredLogger
}

func NewParserRepo(logger *zap.SugaredLogger) *ParserRepo {
	return &ParserRepo{
		Logger: logger,
	}
}

var (
	ErrEmptyURL = errors.New("empty URL")
)

func (repo *ParserRepo) ParseLinks(body []byte, base string) []string {
	seen := map[string]struct{}{}
	var links []string

	var baseURL *url.URL
	if base != "" {
		if parsedURL, err := url.Parse(base); err == nil {
			baseURL = parsedURL
		} else if repo.Logger != nil {
			repo.Logger.Warnw("invalid base URL, skipping resolution", "base", base, "err", err)
		}
	}

	doc, err := html.Parse(bytes.NewReader(body))
	if err == nil {
		repo.extractLinksFromNode(doc, seen, baseURL)
	}

	repo.regexFallback(body, seen, baseURL)

	for u := range seen {
		links = append(links, u)
	}

	return links
}

func (repo *ParserRepo) normalizeURL(url string) string {
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

	if !repo.checkAllowedPrefixes(url) {
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

func (repo *ParserRepo) checkAllowedPrefixes(url string) bool {
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

func (repo *ParserRepo) extractFromSrcset(val string, seen map[string]struct{}, base *url.URL) {
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
		if urlNormalized := repo.normalizeURL(fields[0]); urlNormalized != "" {
			repo.resolveAndAdd(urlNormalized, seen, base)
		}
	}
}

func (repo *ParserRepo) extractLinksFromNode(node *html.Node, seen map[string]struct{}, base *url.URL) {
	if node.Type == html.ElementNode {
		for _, attribute := range node.Attr {
			if _, valid := attrsWithURLs[attribute.Key]; valid {
				if attribute.Key == "srcset" || attribute.Key == "data-srcset" {
					repo.extractFromSrcset(attribute.Val, seen, base)
				} else {
					if urlNormalized := repo.normalizeURL(attribute.Val); urlNormalized != "" {
						repo.resolveAndAdd(urlNormalized, seen, base)
					}
				}
			}
		}
	}

	for child := node.FirstChild; child != nil; child = child.NextSibling {
		repo.extractLinksFromNode(child, seen, base)
	}
}

func (repo *ParserRepo) regexFallback(body []byte, seen map[string]struct{}, base *url.URL) {
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

		if urlNormalized := repo.normalizeURL(foundURL); urlNormalized != "" {
			repo.resolveAndAdd(urlNormalized, seen, base)
		}
	}
}

func (repo *ParserRepo) resolveAndAdd(raw string, seen map[string]struct{}, base *url.URL) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return
	}

	if !parsed.IsAbs() && base != nil {
		resolved := base.ResolveReference(parsed)
		seen[resolved.String()] = struct{}{}
		return
	}

	seen[parsed.String()] = struct{}{}
}
