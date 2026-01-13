package webcrawler

import "errors"

var (
	ErrNotAllowedByRobots = errors.New("not allowed by robots.txt")
	ErrCacheHit           = errors.New("cache hit, skipped processing")
	ErrFetching           = errors.New("error fetching page")
)
