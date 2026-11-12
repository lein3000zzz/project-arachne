package facts

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"
	"web-crawler/internal/vectorstore"

	"go.uber.org/zap"
)

// Store is where the ledger keeps what it digested.
type Store interface {
	Due(ctx context.Context, limit int) ([]Due, error)
	Version(ctx context.Context, url, reviewHash, indexHash string, planned []int, chunks []ChunkText) error
	Undigested(ctx context.Context, url, indexHash string, ordinals []int) ([]int, error)
	SaveChunk(ctx context.Context, url, indexHash string, d Digested) error
	Lead(ctx context.Context) (release func(), ok bool, err error)
}

// Chunks reads a page's indexed chunks back.
type Chunks interface {
	Chunks(ctx context.Context, url string) ([]vectorstore.Row, error)
}

type DigestConfig struct {
	// Chunks of a page digested as soon as it is indexed, spread over it;
	// the rest when an answer reads them.
	MaxChunks int
	// How often due pages are looked for.
	Every time.Duration
}

// Digester turns the pages of labs into facts, a chunk at a time, in the
// background: the model's time is spent once, when a page is indexed, not
// every time a question is asked.
type Digester struct {
	logger  *zap.SugaredLogger
	store   Store
	chunks  Chunks
	extract *Extractor
	cfg     DigestConfig
	wanted  chan want
}

type want struct {
	url      string
	ordinals []int
}

// Pages looked at per sweep; due pages beyond wait for the next.
const sweepBatch = 20

// After the model failed, how long before it is asked again.
const modelBackoff = time.Minute

func NewDigester(logger *zap.SugaredLogger, store Store, chunks Chunks, extract *Extractor, cfg DigestConfig) *Digester {
	cfg.MaxChunks = max(cfg.MaxChunks, 1)
	cfg.Every = cmp.Or(cfg.Every, 30*time.Second)

	return &Digester{logger: logger, store: store, chunks: chunks, extract: extract, cfg: cfg, wanted: make(chan want, 64)}
}

// Want asks for chunks of a page an answer is reading, ahead of the sweep.
// It never blocks: when too much is wanted already, the sweep gets to it.
func (d *Digester) Want(url string, ordinals []int) {
	select {
	case d.wanted <- want{url, ordinals}:
	default:
	}
}

// Run digests until ctx ends, in the one process holding the lock.
func (d *Digester) Run(ctx context.Context) {
	for {
		release, ok, err := d.store.Lead(ctx)
		if err != nil && ctx.Err() == nil {
			d.logger.Warnw("Failed to take the digesting lock", "error", err)
		}

		if ok {
			d.lead(ctx)
			release()
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(d.cfg.Every):
		}
	}
}

func (d *Digester) lead(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		var err error

		select {
		case <-ctx.Done():
			return
		case w := <-d.wanted:
			err = d.chunksOf(ctx, Due{URL: w.url}, w.ordinals)
		case <-timer.C:
			err = d.Sweep(ctx)
		}

		wait := d.cfg.Every
		if err != nil && ctx.Err() == nil {
			d.logger.Warnw("Digesting pages into facts failed; trying again later", "error", err)
			wait = max(wait, modelBackoff)
		}

		timer.Reset(wait)
	}
}

// Sweep digests the due pages it finds, as far as their planned chunks.
func (d *Digester) Sweep(ctx context.Context) error {
	due, err := d.store.Due(ctx, sweepBatch)
	if err != nil {
		return err
	}

	for _, page := range due {
		if err := d.chunksOf(ctx, page, nil); err != nil {
			return err
		}
	}

	return nil
}

// chunksOf records the page's indexed version and digests the chunks given,
// or its planned ones when none are. A page whose new version is reviewed but
// not indexed yet is left for a later sweep.
func (d *Digester) chunksOf(ctx context.Context, page Due, ordinals []int) error {
	rows, err := d.chunks.Chunks(ctx, page.URL)
	if err != nil {
		return err
	}

	if len(rows) == 0 {
		return nil
	}

	slices.SortFunc(rows, func(a, b vectorstore.Row) int { return cmp.Compare(a.Ordinal, b.Ordinal) })

	indexHash := rows[0].ContentHash
	indexedAt := slices.MaxFunc(rows, func(a, b vectorstore.Row) int { return cmp.Compare(a.IndexedAt, b.IndexedAt) }).IndexedAt

	if page.ReviewHash != "" {
		if time.UnixMilli(indexedAt).Before(page.ReviewedAt) {
			return nil
		}

		texts := make([]ChunkText, len(rows))
		for i, r := range rows {
			texts[i] = ChunkText{Ordinal: int(r.Ordinal), Text: r.Text}
		}

		if err := d.store.Version(ctx, page.URL, page.ReviewHash, indexHash, Plan(len(rows), d.cfg.MaxChunks), texts); err != nil {
			return err
		}
	}

	if ordinals == nil {
		ordinals = Plan(len(rows), d.cfg.MaxChunks)
	}

	todo, err := d.store.Undigested(ctx, page.URL, indexHash, ordinals)
	if err != nil {
		return err
	}

	for _, n := range todo {
		i := slices.IndexFunc(rows, func(r vectorstore.Row) bool { return int(r.Ordinal) == n })
		if i < 0 {
			continue
		}

		if err := d.digest(ctx, page.URL, indexHash, rows[i]); err != nil {
			return err
		}
	}

	return nil
}

func (d *Digester) digest(ctx context.Context, url, indexHash string, row vectorstore.Row) error {
	started := time.Now()
	res, err := d.extract.Extract(ctx, row.Title, row.Text)

	dg := Digested{Ordinal: int(row.Ordinal), TextHash: TextHash(row.Text), Facts: res.Facts, Rejected: len(res.Rejected),
		Seconds: time.Since(started).Seconds(), Model: res.Model}

	switch {
	case errors.Is(err, ErrReply):
		// Asked again, the model would say the same: recorded, not retried.
		dg.Error = err.Error()
	case err != nil:
		return err
	}

	return d.store.SaveChunk(ctx, url, indexHash, dg)
}

// Plan picks which of a page's chunks are digested at once: all of a short
// page; of a long one its first two, its last, and the rest spread between,
// as a document says what it is about near its start and its end.
func Plan(chunks, most int) []int {
	if chunks <= most {
		out := make([]int, chunks)
		for i := range out {
			out[i] = i
		}

		return out
	}

	picked := map[int]bool{0: true, chunks - 1: true}
	if most > 2 {
		picked[1] = true
	}

	for k := 1; k < most-1 && len(picked) < most; k++ {
		picked[k*(chunks-1)/(most-1)] = true
	}

	// Spread picks that fell on one another leave room: the first chunks
	// not picked fill it.
	for n := 2; len(picked) < most && n < chunks; n++ {
		picked[n] = true
	}

	out := make([]int, 0, len(picked))
	for n := range picked {
		out = append(out, n)
	}

	slices.Sort(out)

	return out
}
