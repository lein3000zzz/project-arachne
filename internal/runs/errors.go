package runs

import "errors"

var (
	ErrNotFound       = errors.New("no such run")
	ErrInvalidRequest = errors.New("invalid crawl request")
)
