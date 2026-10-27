package llm

import "errors"

var (
	ErrInvalidConfig = errors.New("invalid chat model config")
	ErrRequest       = errors.New("chat request failed")
	ErrMalformed     = errors.New("malformed chat response")
)
