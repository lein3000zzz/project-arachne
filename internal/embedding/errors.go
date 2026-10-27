package embedding

import "errors"

var (
	ErrInvalidConfig    = errors.New("invalid embedding config")
	ErrRequest          = errors.New("embedding request failed")
	ErrMalformed        = errors.New("malformed embedding response")
	ErrDimensionIgnored = errors.New("endpoint ignored the requested dimensions")
)
