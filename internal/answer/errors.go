package answer

import "errors"

var (
	ErrInvalidConfig = errors.New("invalid answer config")
	// Nothing relevant to ground an answer in; the model is not asked at all.
	ErrNoSources = errors.New("nothing relevant found to answer from")
)
