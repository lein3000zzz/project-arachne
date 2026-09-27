package webcrawler

import "errors"

var (
	ErrNotAllowedByRobots = errors.New("not allowed by robots.txt")
	ErrCacheHit           = errors.New("cache hit, skipped processing")
	ErrFetching           = errors.New("error fetching page")
	// ErrChildrenNotSent: some links of the page never reached the queue, so the
	// task must not complete - redelivery sends them again.
	ErrChildrenNotSent = errors.New("child tasks not sent")
)
