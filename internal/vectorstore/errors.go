package vectorstore

import "errors"

var (
	ErrInvalidConfig  = errors.New("invalid vector store config")
	ErrSchemaMismatch = errors.New("existing collection does not match")
	ErrInvalidRow     = errors.New("invalid row")
	ErrTooManyChunks  = errors.New("too many stored chunks for one URL")
)
