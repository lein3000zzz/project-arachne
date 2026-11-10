package copies

import (
	"context"
	"errors"
	"strings"
	"web-crawler/internal/vectorstore"

	"go.uber.org/zap"
)

type BackfillStore interface {
	// Unsketched lists indexed pages whose review has no sketch yet.
	Unsketched(ctx context.Context, limit int) ([]string, error)
	SaveSketch(ctx context.Context, url string, sketch Sketch) error
}

type Chunks interface {
	Chunks(ctx context.Context, url string) ([]vectorstore.Row, error)
}

const backfillBatch = 200

// Backfill sketches the pages reviewed before reviews kept sketches, from the
// chunks stored for them: the same text the review read, cut in pieces. A
// page that cannot be sketched gets an empty one, so it is not asked for
// again.
func Backfill(ctx context.Context, logger *zap.SugaredLogger, store BackfillStore, chunks Chunks) {
	done := 0

	for ctx.Err() == nil {
		urls, err := store.Unsketched(ctx, backfillBatch)
		if err != nil {
			logger.Warnw("Listing pages to sketch failed", "error", err)
			return
		}

		if len(urls) == 0 {
			break
		}

		for _, url := range urls {
			rows, err := chunks.Chunks(ctx, url)

			switch {
			case errors.Is(err, vectorstore.ErrTooManyChunks):
				rows = nil
			case err != nil:
				// The next start goes on from here.
				if ctx.Err() == nil {
					logger.Warnw("Reading a page's chunks failed", "url", url, "error", err)
				}

				return
			}

			texts := make([]string, len(rows))
			for i, row := range rows {
				texts[i] = strings.TrimPrefix(row.Text, row.Title+"\n\n")
			}

			if err := store.SaveSketch(ctx, url, SketchOf(SetOf(texts...))); err != nil {
				logger.Warnw("Saving a page's sketch failed", "url", url, "error", err)
				return
			}

			done++
		}
	}

	if done > 0 {
		logger.Infow("Sketched pages reviewed before sketches were kept", "pages", done)
	}
}
