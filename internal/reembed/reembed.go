// Package reembed copies the chunks of one collection into another with new
// vectors: after the embedding model changes, the index is rebuilt from what
// is stored, without crawling, reviewing or chunking anything again.
package reembed

import (
	"context"
	"fmt"
	"slices"
	"web-crawler/internal/embedding"
	"web-crawler/internal/vectorstore"

	"go.uber.org/zap"
)

type Source interface {
	URLs(ctx context.Context) ([]string, error)
	Chunks(ctx context.Context, url string) ([]vectorstore.Row, error)
}

type Target interface {
	Chunks(ctx context.Context, url string) ([]vectorstore.Row, error)
	Replace(ctx context.Context, url string, rows []vectorstore.Row) error
}

// Stats counts pages by what happened to them.
type Stats struct {
	Pages int
	// Embedded anew into the target.
	Copied int
	// In the target already, as the source has them: an earlier run copied them.
	Current int
	// In the target with other content: the indexer wrote them since the
	// switch, and they are newer than the copy would be.
	Newer  int
	Failed int
	Chunks int
}

type Config struct {
	// Chunks embedded per request: pages are grouped until they hold this many.
	Batch  int
	DryRun bool
	// Called after each group of pages.
	Progress func(Stats)
}

// Run copies every page of from that to lacks. A page to has is left alone,
// so a run can be stopped and started again, and runs beside the crawler.
// Text stored is the text that was embedded, title included: embedding it
// again gives what indexing would.
func Run(ctx context.Context, logger *zap.SugaredLogger, from Source, to Target, embedder embedding.Embedder, cfg Config) (Stats, error) {
	urls, err := from.URLs(ctx)
	if err != nil {
		return Stats{}, err
	}

	stats := Stats{Pages: len(urls)}

	type page struct {
		url  string
		rows []vectorstore.Row
	}

	var (
		group  []page
		chunks int
	)

	flush := func() {
		defer func() {
			group, chunks = group[:0], 0
			if cfg.Progress != nil {
				cfg.Progress(stats)
			}
		}()

		if len(group) == 0 {
			return
		}

		if cfg.DryRun {
			stats.Chunks += chunks
			return
		}

		var texts []string
		for _, p := range group {
			for _, r := range p.rows {
				texts = append(texts, r.Text)
			}
		}

		vectors, err := embedder.Embed(ctx, texts)
		if err != nil {
			logger.Warnw("Embedding a group of pages failed; they are left for the next run", "pages", len(group), "error", err)
			stats.Failed += len(group)
			stats.Copied -= len(group)

			return
		}

		next := 0

		for _, p := range group {
			for i := range p.rows {
				p.rows[i].Vector = vectors[next]
				next++
			}

			if err := to.Replace(ctx, p.url, p.rows); err != nil {
				logger.Warnw("Storing a page failed; it is left for the next run", "url", p.url, "error", err)
				stats.Failed++
				stats.Copied--

				continue
			}

			stats.Chunks += len(p.rows)
		}
	}

	for _, url := range urls {
		if err := ctx.Err(); err != nil {
			return stats, err
		}

		rows, err := from.Chunks(ctx, url)
		if err != nil {
			logger.Warnw("Reading a page failed; it is left for the next run", "url", url, "error", err)
			stats.Failed++

			continue
		}

		// Gone since the listing: the indexer removed it.
		if len(rows) == 0 {
			continue
		}

		have, err := to.Chunks(ctx, url)
		if err != nil {
			logger.Warnw("Reading a page's copy failed; it is left for the next run", "url", url, "error", err)
			stats.Failed++

			continue
		}

		switch state(rows, have) {
		case current:
			stats.Current++
			continue
		case newer:
			stats.Newer++
			continue
		}

		stats.Copied++
		group = append(group, page{url: url, rows: rows})
		chunks += len(rows)

		if chunks >= cfg.Batch {
			flush()
		}
	}

	flush()

	if stats.Failed > 0 {
		return stats, fmt.Errorf("%d pages failed; run again to retry them", stats.Failed)
	}

	return stats, ctx.Err()
}

type copyState int

const (
	missing copyState = iota
	current
	newer
)

// state compares a page's chunks in the target with the source's. A content
// hash of the target's own is the indexer's work, done since the switch; the
// source's hash with other chunks is a copy cut short, to be done again.
func state(source, target []vectorstore.Row) copyState {
	if len(target) == 0 {
		return missing
	}

	hash := source[0].ContentHash

	for _, r := range target {
		if r.ContentHash != hash {
			return newer
		}
	}

	ids := func(rows []vectorstore.Row) []string {
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = r.ID
		}

		slices.Sort(out)

		return out
	}

	if slices.Equal(ids(source), ids(target)) {
		return current
	}

	return missing
}
