package pageparser

import (
	"net/url"
	"strings"

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

func (p *ParserBasic) isURLCandidate(s string) bool {
	s = strings.TrimSpace(s)
	return s != ""
}
