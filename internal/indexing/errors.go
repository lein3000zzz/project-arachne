package indexing

import "errors"

var (
	ErrHashNotFound = errors.New("document hash not found")
	ErrNilDocument  = errors.New("nil document")
	ErrQueueFull    = errors.New("indexing queue full")
	ErrSinkClosed   = errors.New("indexing sink closed")
)
