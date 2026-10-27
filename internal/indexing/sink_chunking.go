package indexing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync/atomic"
	"web-crawler/internal/chunker"
	"web-crawler/internal/documents"

	"go.uber.org/zap"
)

type ChunkingSink struct {
	logger  *zap.SugaredLogger
	chunker chunker.Chunker
	hashes  HashStore
	next    ChunkSink

	submitted atomic.Int64
	unchanged atomic.Int64
}

func NewChunkingSink(logger *zap.SugaredLogger, c chunker.Chunker, hashes HashStore, next ChunkSink) *ChunkingSink {
	return &ChunkingSink{
		logger:  logger,
		chunker: c,
		hashes:  hashes,
		next:    next,
	}
}

func (s *ChunkingSink) Submit(ctx context.Context, doc *documents.Document) error {
	if doc == nil {
		return ErrNilDocument
	}

	hash := documentHash(s.chunker.Fingerprint()+"|"+s.next.Fingerprint(), doc)

	stored, err := s.hashes.Get(ctx, doc.URL)

	switch {
	case err == nil && stored == hash:
		s.unchanged.Add(1)
		s.logger.Debugw("document unchanged, skipping", "url", doc.URL)

		return nil
	case err != nil && !errors.Is(err, ErrHashNotFound):
		s.logger.Warnw("document hash lookup failed, chunking anyway", "url", doc.URL, "err", err)
	}

	batch := &Batch{Document: doc, Hash: hash, Chunks: s.chunker.Chunk(doc)}

	if err := s.next.Submit(ctx, batch); err != nil {
		return fmt.Errorf("submitting chunks for %s: %w", doc.URL, err)
	}

	s.submitted.Add(1)

	// Recorded only once the batch was accepted: a failed submit leaves no hash
	// behind, so the next crawl of this page tries again.
	if err := s.hashes.Set(ctx, doc.URL, hash); err != nil {
		s.logger.Warnw("failed to record document hash", "url", doc.URL, "err", err)
	}

	return nil
}

func (s *ChunkingSink) Shutdown(ctx context.Context) error {
	s.logger.Infow("chunking sink stopped",
		"submitted", s.submitted.Load(),
		"unchanged", s.unchanged.Load(),
	)

	return errors.Join(s.next.Shutdown(ctx), s.hashes.Shutdown(ctx))
}

// The fingerprint covers the chunker settings and the destination, so changing
// either re-indexes every page instead of leaving the index split across layouts.
func documentHash(fingerprint string, doc *documents.Document) string {
	sum := sha256.Sum256([]byte(fingerprint + "\x00" + doc.Title + "\x00" + doc.Content))
	return hex.EncodeToString(sum[:])
}
