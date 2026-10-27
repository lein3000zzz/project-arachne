package rerank

import "errors"

var (
	ErrInvalidConfig = errors.New("invalid reranker config")
	ErrRequest       = errors.New("rerank request failed")
	ErrMalformed     = errors.New("malformed rerank response")
)
