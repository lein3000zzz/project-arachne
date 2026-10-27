package retrieval

import "errors"

var (
	ErrInvalidConfig  = errors.New("invalid retriever config")
	ErrInvalidRequest = errors.New("invalid search request")
	ErrEmptyQuery     = errors.New("empty query")
	ErrUnknownMode    = errors.New("unknown search mode")
	ErrNoReranker     = errors.New("reranking requested, but no reranker is configured")
)
