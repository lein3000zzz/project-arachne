package processor

import "errors"

var (
	ErrRunLimitExceeded = errors.New("run limit exceeded")
	ErrRunNotActive     = errors.New("run not active")
	ErrInvalidURL       = errors.New("not a crawlable URL")
)
