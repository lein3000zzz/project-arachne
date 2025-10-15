package pageparser

import (
	"encoding/json"
	"net/url"
	"strings"
)

// TODO Я знаю, что этот код рефактора требует

func (p *ParserBasic) ExtractLinksFromJSON(baseURL string, data []byte) ([]string, error) {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		if p.Logger != nil {
			p.Logger.Errorw("json parse error", "err", err)
		}
		return nil, err
	}

	var base *url.URL
	if baseURL != "" {
		if u, err := url.Parse(baseURL); err == nil {
			base = u
		} else if p.Logger != nil {
			p.Logger.Warnw("invalid base URL, skipping resolution", "base", baseURL, "err", err)
		}
	}

	seen := make(map[string]struct{})
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}

		if matches := urlRegex.FindAllString(s, -1); len(matches) > 0 {
			for _, m := range matches {
				p.resolveAndAdd(m, seen, base)
			}
		} else {
			if p.looksLikeRelativePath(s) {
				p.resolveAndAdd(s, seen, base)
			}
		}
	}

	p.walkJSON(root, add)

	out := make([]string, 0, len(seen))
	for u := range seen {
		out = append(out, u)
	}

	return out, nil
}

func (p *ParserBasic) walkJSON(v any, onString func(string)) {
	switch x := v.(type) {
	case map[string]any:
		for _, vv := range x {
			p.walkJSON(vv, onString)
		}
	case []any:
		for _, vv := range x {
			p.walkJSON(vv, onString)
		}
	case string:
		onString(x)
	default:

	}
}

func (p *ParserBasic) looksLikeRelativePath(s string) bool {
	if s == "" {
		return false
	}

	if strings.HasPrefix(s, "data:") || strings.ContainsAny(s, " \t\n\"<>") {
		return false
	}

	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") {
		return true
	}

	if strings.HasPrefix(s, "?") || strings.HasPrefix(s, "#") {
		return true
	}

	if strings.Contains(s, "/") {
		return true
	}

	// Вот это для случаев с image.png и прочими чудесами
	if i := strings.LastIndexByte(s, '.'); i > 0 && i < len(s)-1 {
		return true
	}
	return false
}
