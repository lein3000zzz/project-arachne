// Package twins asks servers about pages the crawler never fetched but whose URL
// differs from a fetched page's only by scheme or a trailing slash. Left alone,
// links to such a placeholder count for it instead of the page they mean, and a
// template link repeated on every page of a site splits that page's rank.
package twins

import (
	"context"
	"net/http"
	"sync"
	"web-crawler/internal/networker"
	"web-crawler/internal/pages"
	"web-crawler/internal/urlnorm"

	"go.uber.org/zap"
)

type Store interface {
	PlaceholderTwins(ctx context.Context, limit int) ([]pages.Twin, error)
	ResolveTwin(ctx context.Context, placeholder, identity string) error
}

type Fetcher interface {
	Fetch(url string) (*networker.FetchResult, error)
}

type Robots interface {
	Allowed(url string) bool
}

type Stats struct {
	Checked, Folded, Skipped int
}

// Resolve checks up to limit placeholders, workers at a time. A placeholder that
// cannot be fetched now - robots.txt, network - is left for the next run.
func Resolve(ctx context.Context, logger *zap.SugaredLogger, store Store, fetcher Fetcher, robots Robots, limit, workers int) (Stats, error) {
	twins, err := store.PlaceholderTwins(ctx, limit)
	if err != nil {
		return Stats{}, err
	}

	var (
		mu    sync.Mutex
		stats Stats
		first error
		wg    sync.WaitGroup
	)

	work := make(chan pages.Twin)

	for i := 0; i < workers; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for twin := range work {
				target, skipped, err := resolve(ctx, store, fetcher, robots, twin)

				mu.Lock()
				switch {
				case err != nil:
					if first == nil {
						first = err
					}
				case skipped:
					stats.Skipped++
				default:
					stats.Checked++
					if target != "" {
						stats.Folded++
						logger.Infow("Placeholder is an alias", "url", twin.Placeholder, "of", target)
					}
				}
				mu.Unlock()
			}
		}()
	}

	for _, twin := range twins {
		if ctx.Err() != nil {
			break
		}

		work <- twin
	}

	close(work)
	wg.Wait()

	if first == nil {
		first = ctx.Err()
	}

	return stats, first
}

// resolve returns the page the placeholder turned out to be, or "" when it is
// none other than itself.
func resolve(ctx context.Context, store Store, fetcher Fetcher, robots Robots, twin pages.Twin) (target string, skipped bool, err error) {
	if !robots.Allowed(twin.Placeholder) {
		return "", true, nil
	}

	res, err := fetcher.Fetch(twin.Placeholder)
	if err != nil {
		return "", true, nil
	}

	identity, ok := urlnorm.Canonical(res.IdentityURL, nil)
	if !ok {
		identity = twin.Placeholder
	}

	// Without a redirect, a server error or rate limit says nothing yet; marking
	// the placeholder checked would stop it from ever being asked again.
	if identity == twin.Placeholder && (res.Status >= http.StatusInternalServerError || res.Status == http.StatusTooManyRequests) {
		return "", true, nil
	}

	if err := store.ResolveTwin(ctx, twin.Placeholder, identity); err != nil {
		return "", false, err
	}

	if identity == twin.Placeholder {
		return "", false, nil
	}

	return identity, false, nil
}
