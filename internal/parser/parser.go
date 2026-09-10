package parser

import "context"

type Parser interface {
	Parse(ctx context.Context, params *ParseParams) (*ParseResult, error)
}

type ParseParams struct {
	Body        []byte
	BaseURL     string
	ContentType string
}

type ParseResult struct {
	Title    string
	Content  string
	Links    []Link
	Warnings []Warning
}

type LinkKind string

const (
	LinkPage  LinkKind = "page"
	LinkOther LinkKind = "other"
)

type Link struct {
	URL  string
	Kind LinkKind
}

type Warning struct {
	Stage   string
	Message string
}

func (r *ParseResult) URLsOfKind(kind LinkKind) []string {
	if r == nil {
		return nil
	}

	out := make([]string, 0, len(r.Links))
	for _, link := range r.Links {
		if link.Kind == kind {
			out = append(out, link.URL)
		}
	}

	return out
}
