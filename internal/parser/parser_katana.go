package parser

import (
	"context"
	"encoding/json"
	"mime"
	"net/url"
	"strings"

	katanaparser "github.com/projectdiscovery/katana/pkg/engine/parser"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
)

type KatanaParser struct {
	logger *zap.SugaredLogger
	links  *katanaparser.Parser
}

func NewKatanaParser(logger *zap.SugaredLogger) (*KatanaParser, error) {
	if logger == nil {
		return nil, ErrNilParams
	}

	return &KatanaParser{
		logger: logger,
		links:  newLinkParser(),
	}, nil
}

func (p *KatanaParser) Parse(ctx context.Context, params *ParseParams) (*ParseResult, error) {
	if params == nil {
		return nil, ErrNilParams
	}

	if len(params.Body) == 0 {
		return nil, ErrEmptyBody
	}

	_, span := otel.Tracer(tracerName).Start(ctx, "Parse")
	defer span.End()

	mediaType := normalizeContentType(params.ContentType)
	base := parseBaseURL(params.BaseURL)

	span.SetAttributes(
		attribute.String("base_url", params.BaseURL),
		attribute.String("content_type", mediaType),
		attribute.Int("body_size", len(params.Body)),
	)

	result := p.parse(params.Body, base, mediaType)

	span.SetAttributes(
		attribute.Int("links", len(result.Links)),
		attribute.Int("page_links", len(result.URLsOfKind(LinkPage))),
		attribute.Int("content_size", len(result.Content)),
		attribute.Int("warnings", len(result.Warnings)),
	)

	return result, nil
}

func (p *KatanaParser) parse(body []byte, base *url.URL, mediaType string) *ParseResult {
	acc := newAccumulator(base)

	var title, content string

	switch classifyBody(mediaType, body) {
	case kindHTML:
		doc := documentFrom(body, acc)
		title, content = p.extractContent(body, base, doc, acc)
		p.extractLinks(body, base, mediaType, doc, acc)
	case kindJSON:
		p.extractJSONLinks(body, acc)
	case kindJS:
		p.extractLinks(body, base, mediaType, nil, acc)
	case kindText:
		content = normalizeWhitespace(string(body))
		p.extractLinks(body, base, mediaType, nil, acc)
	}

	return &ParseResult{
		Title:    title,
		Content:  content,
		Links:    acc.links,
		Warnings: acc.warnings,
	}
}

func (p *KatanaParser) extractJSONLinks(body []byte, acc *accumulator) {
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		acc.warn(stageJSON, err.Error())
		p.logger.Debugw("failed to decode json body", "err", err)

		return
	}

	walkJSON(decoded, 0, func(s string) {
		if looksLikeURL(s) {
			acc.add(s, LinkOther)
		}
	})
}

func walkJSON(node any, depth int, onString func(string)) {
	if depth > maxJSONDepth {
		return
	}

	switch typed := node.(type) {
	case string:
		onString(typed)
	case []any:
		for _, item := range typed {
			walkJSON(item, depth+1, onString)
		}
	case map[string]any:
		for _, value := range typed {
			walkJSON(value, depth+1, onString)
		}
	}
}

type accumulator struct {
	base     *url.URL
	index    map[string]int
	links    []Link
	warnings []Warning
}

func newAccumulator(base *url.URL) *accumulator {
	return &accumulator{
		base:  base,
		index: make(map[string]int),
		links: make([]Link, 0),
	}
}

func (a *accumulator) add(raw string, kind LinkKind) {
	normalized := normalizeURL(raw, a.base)
	if normalized == "" {
		return
	}

	if i, exists := a.index[normalized]; exists {
		if kind == LinkPage {
			a.links[i].Kind = LinkPage
		}

		return
	}

	a.index[normalized] = len(a.links)
	a.links = append(a.links, Link{URL: normalized, Kind: kind})
}

func (a *accumulator) warn(stage, message string) {
	a.warnings = append(a.warnings, Warning{Stage: stage, Message: message})
}

func normalizeContentType(contentType string) string {
	contentType = strings.TrimSpace(strings.ToLower(contentType))
	if contentType == "" {
		return ""
	}

	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil {
		return mediaType
	}

	if idx := strings.IndexByte(contentType, ';'); idx >= 0 {
		return strings.TrimSpace(contentType[:idx])
	}

	return contentType
}

func classifyBody(mediaType string, body []byte) bodyKind {
	switch {
	case isHTML(mediaType):
		return kindHTML
	case isJSON(mediaType):
		return kindJSON
	case isJavaScript(mediaType):
		return kindJS
	case strings.HasPrefix(mediaType, contentTypeText):
		return kindText
	}

	if json.Valid(body) {
		return kindJSON
	}

	if looksLikeHTML(body) {
		return kindHTML
	}

	return kindText
}

func isHTML(mediaType string) bool {
	return mediaType == contentTypeHTML || mediaType == contentTypeXHTML
}

func isJSON(mediaType string) bool {
	return mediaType == contentTypeJSON || strings.HasSuffix(mediaType, suffixJSON)
}

func isJavaScript(mediaType string) bool {
	return strings.Contains(mediaType, contentTypeJavaScript) || strings.HasSuffix(mediaType, suffixECMAScript)
}

func looksLikeHTML(body []byte) bool {
	if len(body) > htmlSniffLimit {
		body = body[:htmlSniffLimit]
	}

	head := strings.ToLower(string(body))

	return strings.Contains(head, "<html") ||
		strings.Contains(head, "<!doctype html") ||
		strings.Contains(head, "<body")
}
