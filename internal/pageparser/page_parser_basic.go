package pageparser

import (
	"net/url"
	"strings"
	"web-crawler/internal/urlnorm"

	"go.uber.org/zap"
)

type ParserBasic struct {
	Logger *zap.SugaredLogger
}

func NewParserRepo(logger *zap.SugaredLogger) *ParserBasic {
	return &ParserBasic{
		Logger: logger,
	}
}

func (p *ParserBasic) resolveAndAdd(raw string, seen map[string]struct{}, base *url.URL) {
	canonical, ok := urlnorm.Canonical(raw, base)
	if !ok {
		return
	}

	seen[canonical] = struct{}{}
}

func (p *ParserBasic) isURLCandidate(s string) bool {
	s = strings.TrimSpace(s)
	return s != ""
}
