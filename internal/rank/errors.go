package rank

import "errors"

var (
	ErrInvalidGraph   = errors.New("invalid graph")
	ErrInvalidOptions = errors.New("invalid pagerank options")
)
