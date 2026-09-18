package pageparser

import (
	"context"
	"strings"
	"web-crawler/internal/parser"
	"web-crawler/internal/utils"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
)

type LegacyAdapter struct {
	logger *zap.SugaredLogger
	parser PageParser
}

func NewLegacyAdapter(logger *zap.SugaredLogger) *LegacyAdapter {
	return &LegacyAdapter{
		logger: logger,
		parser: NewParserRepo(logger),
	}
}

func (a *LegacyAdapter) Parse(ctx context.Context, params *parser.ParseParams) (*parser.ParseResult, error) {
	if params == nil {
		return nil, parser.ErrNilParams
	}

	if len(params.Body) == 0 {
		return nil, parser.ErrEmptyBody
	}

	_, span := otel.Tracer(legacyTracerName).Start(ctx, "Parse")
	defer span.End()

	links, err := a.extract(params)
	if err != nil {
		return nil, err
	}

	span.SetAttributes(
		attribute.String("base_url", params.BaseURL),
		attribute.Int("links", len(links)),
	)

	return &parser.ParseResult{Links: dedupe(links)}, nil
}

// This parser predates the page/subresource split and walks one attribute
// allowlist for both, so nothing here can tell them apart. Marking every link
// followable reproduces what v0 actually crawled; the alternative - marking them
// all LinkOther - would leave the frontier empty and stop every crawl at depth 0.
func dedupe(links []string) []parser.Link {
	seen := make(map[string]struct{}, len(links))
	out := make([]parser.Link, 0, len(links))

	for _, link := range links {
		if link == "" {
			continue
		}

		if _, exists := seen[link]; exists {
			continue
		}

		seen[link] = struct{}{}
		out = append(out, parser.Link{URL: link, Kind: parser.LinkPage})
	}

	return out
}

func (a *LegacyAdapter) extract(params *parser.ParseParams) ([]string, error) {
	// v0 dispatched on the URL suffix rather than the content type; kept as-is so
	// the legacy engine behaves the way it did.
	if strings.HasSuffix(strings.TrimSuffix(params.BaseURL, "/"), ".js") {
		baseURL, err := utils.GetBaseURL(params.BaseURL)
		if err != nil {
			return nil, err
		}

		return a.parser.ExtractLinksFromJS(baseURL, string(params.Body))
	}

	links := a.parser.ParseHTML(params.Body, params.BaseURL)

	jsonLinks, err := a.parser.ExtractLinksFromJSON(params.BaseURL, params.Body)
	if err != nil {
		a.logger.Warnw("legacy json extraction failed", "url", params.BaseURL, "err", err)
	}

	return append(links, jsonLinks...), nil
}
