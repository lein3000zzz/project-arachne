package parser

import "errors"

var (
	ErrNilParams = errors.New("nil parse params")
	ErrEmptyBody = errors.New("empty body")
)
