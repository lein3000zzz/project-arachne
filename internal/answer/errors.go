package answer

import "errors"

var (
	ErrInvalidConfig = errors.New("invalid answer config")
	// Nothing to ground an answer in; the model is not asked at all.
	ErrNoSources = errors.New("search found nothing to answer from")
)
