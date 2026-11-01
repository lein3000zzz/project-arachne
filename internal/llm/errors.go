package llm

import "errors"

var (
	ErrInvalidConfig = errors.New("invalid chat model config")
	ErrRequest       = errors.New("chat request failed")
	ErrMalformed     = errors.New("malformed chat response")
	// Neither the server nor the user who asked has a chat model.
	ErrNoModel = errors.New("there is no chat model: the server runs none, and none is set in your settings")
)
