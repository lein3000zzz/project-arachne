package pageparser

import (
	"bytes"
	"errors"
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

func (repo *ParserRepo) ParseLinks(body []byte) []string {
	seen := map[string]struct{}{}
	var links []string

	doc, err := html.Parse(bytes.NewReader(body))
	if err == nil {
		repo.extractLinksFromNode(doc, seen)
	}

	repo.regexFallback(body, seen)

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

func (repo *ParserRepo) extractFromSrcset(val string, seen map[string]struct{}) {
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
			seen[urlNormalized] = struct{}{}
		}
	}
}

func (repo *ParserRepo) extractLinksFromNode(node *html.Node, seen map[string]struct{}) {
	if node.Type == html.ElementNode {
		for _, attribute := range node.Attr {
			if _, valid := attrsWithURLs[attribute.Key]; valid {
				if attribute.Key == "srcset" || attribute.Key == "data-srcset" {
					repo.extractFromSrcset(attribute.Val, seen)
				} else {
					if urlNormalized := repo.normalizeURL(attribute.Val); urlNormalized != "" {
						seen[urlNormalized] = struct{}{}
					}
				}
			}
		}
	}

	for child := node.FirstChild; child != nil; child = child.NextSibling {
		repo.extractLinksFromNode(child, seen)
	}
}

func (repo *ParserRepo) regexFallback(body []byte, seen map[string]struct{}) {
	for _, regexMatch := range urlRegex.FindAll(body, -1) {
		url := strings.TrimSpace(string(regexMatch))
		for len(url) > 0 {
			last := url[len(url)-1]
			if strings.ContainsRune(".,;:!?)\"]}'", rune(last)) {
				url = url[:len(url)-1]
				continue
			}
			break
		}

		if url == "" {
			continue
		}

		if urlNormalized := repo.normalizeURL(url); urlNormalized != "" {
			seen[urlNormalized] = struct{}{}
		}
	}
}
