package indexing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"web-crawler/internal/chunker"
	"web-crawler/internal/documents"

	"go.uber.org/zap"
)

type memHashStore struct {
	mu          sync.Mutex
	hashes      map[string]string
	getErr      error
	setErr      error
	shutdownErr error
	shutdowns   int
}

func newMemHashStore() *memHashStore {
	return &memHashStore{hashes: make(map[string]string)}
}

func (m *memHashStore) Get(_ context.Context, url string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.getErr != nil {
		return "", m.getErr
	}

	hash, ok := m.hashes[url]
	if !ok {
		return "", ErrHashNotFound
	}

	return hash, nil
}

func (m *memHashStore) Set(_ context.Context, url, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.setErr != nil {
		return m.setErr
	}

	m.hashes[url] = hash

	return nil
}

func (m *memHashStore) Shutdown(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.shutdowns++

	return m.shutdownErr
}

func (m *memHashStore) stored(url string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	hash, ok := m.hashes[url]

	return hash, ok
}

type recordingSink struct {
	mu          sync.Mutex
	batches     []*Batch
	err         error
	shutdownErr error
	shutdowns   int
	fingerprint string
}

func (r *recordingSink) Fingerprint() string {
	return r.fingerprint
}

func (r *recordingSink) Submit(_ context.Context, batch *Batch) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.err != nil {
		return r.err
	}

	r.batches = append(r.batches, batch)

	return nil
}

func (r *recordingSink) Shutdown(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.shutdowns++

	return r.shutdownErr
}

func (r *recordingSink) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.batches)
}

func newChunker(t *testing.T, maxRunes int) *chunker.ParagraphChunker {
	t.Helper()

	c, err := chunker.NewParagraphChunker(chunker.Settings{MaxRunes: maxRunes, OverlapRunes: 20, MinDocumentRunes: 40})
	if err != nil {
		t.Fatalf("NewParagraphChunker: %v", err)
	}

	return c
}

func newSink(t *testing.T) (*ChunkingSink, *memHashStore, *recordingSink) {
	t.Helper()

	hashes, next := newMemHashStore(), &recordingSink{}

	return NewChunkingSink(zap.NewNop().Sugar(), newChunker(t, 200), hashes, next), hashes, next
}

func page(content string) *documents.Document {
	return &documents.Document{
		URL:     "https://example.com/doc",
		Title:   "Doc",
		Content: content,
	}
}

var body = func() string {
	var b strings.Builder
	for i := range 12 {
		fmt.Fprintf(&b, "Sentence %d has enough words to be worth indexing. ", i)
	}

	return b.String()
}()

func TestFirstSightChunksAndRecordsHash(t *testing.T) {
	sink, hashes, next := newSink(t)

	if err := sink.Submit(context.Background(), page(body)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if next.count() != 1 {
		t.Fatalf("want 1 batch downstream, got %d", next.count())
	}

	batch := next.batches[0]
	if len(batch.Chunks) < 2 {
		t.Errorf("fixture should need several chunks, got %d", len(batch.Chunks))
	}

	stored, ok := hashes.stored("https://example.com/doc")
	if !ok || stored != batch.Hash {
		t.Errorf("hash not recorded: stored=%q ok=%v batch=%q", stored, ok, batch.Hash)
	}
}

func TestUnchangedDocumentIsSkipped(t *testing.T) {
	sink, _, next := newSink(t)
	ctx := context.Background()

	for range 3 {
		if err := sink.Submit(ctx, page(body)); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}

	if next.count() != 1 {
		t.Errorf("an unchanged page must be chunked once, got %d batches", next.count())
	}
}

func TestChangesTriggerRechunking(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*documents.Document)
	}{
		{"content", func(d *documents.Document) { d.Content += " One more sentence at the end." }},
		{"title", func(d *documents.Document) { d.Title = "Renamed" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink, _, next := newSink(t)
			ctx := context.Background()

			if err := sink.Submit(ctx, page(body)); err != nil {
				t.Fatalf("Submit: %v", err)
			}

			changed := page(body)
			tc.mutate(changed)

			if err := sink.Submit(ctx, changed); err != nil {
				t.Fatalf("Submit: %v", err)
			}

			if next.count() != 2 {
				t.Errorf("a changed %s must be re-chunked, got %d batches", tc.name, next.count())
			}
		})
	}
}

func TestChunkerSettingsChangeInvalidatesStoredHashes(t *testing.T) {
	hashes, next := newMemHashStore(), &recordingSink{}
	ctx := context.Background()
	logger := zap.NewNop().Sugar()

	if err := NewChunkingSink(logger, newChunker(t, 200), hashes, next).Submit(ctx, page(body)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if err := NewChunkingSink(logger, newChunker(t, 300), hashes, next).Submit(ctx, page(body)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if next.count() != 2 {
		t.Errorf("new chunk settings must re-chunk an otherwise unchanged page, got %d batches", next.count())
	}
}

func TestNewDestinationInvalidatesStoredHashes(t *testing.T) {
	hashes := newMemHashStore()
	ctx := context.Background()
	logger := zap.NewNop().Sugar()

	old := &recordingSink{fingerprint: "chunks_v1_model_a_1024"}
	if err := NewChunkingSink(logger, newChunker(t, 200), hashes, old).Submit(ctx, page(body)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	moved := &recordingSink{fingerprint: "chunks_v1_model_b_1024"}
	if err := NewChunkingSink(logger, newChunker(t, 200), hashes, moved).Submit(ctx, page(body)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if moved.count() != 1 {
		t.Error("a new collection starts empty, so an unchanged page must still be indexed into it")
	}
}

func TestDownstreamFailureLeavesHashUnrecorded(t *testing.T) {
	sink, hashes, next := newSink(t)
	ctx := context.Background()

	next.err = errors.New("vector store unavailable")

	if err := sink.Submit(ctx, page(body)); err == nil {
		t.Fatal("a downstream failure must be returned")
	}

	if _, ok := hashes.stored("https://example.com/doc"); ok {
		t.Fatal("hash recorded for a batch that was never accepted")
	}

	next.err = nil

	if err := sink.Submit(ctx, page(body)); err != nil {
		t.Fatalf("retry: %v", err)
	}

	if next.count() != 1 {
		t.Errorf("the retry must deliver the batch, got %d", next.count())
	}
}

func TestHashLookupOutageStillChunks(t *testing.T) {
	sink, hashes, next := newSink(t)
	hashes.getErr = errors.New("connection refused")

	if err := sink.Submit(context.Background(), page(body)); err != nil {
		t.Fatalf("a hash store outage must not stop indexing: %v", err)
	}

	if next.count() != 1 {
		t.Errorf("want the batch delivered despite the outage, got %d", next.count())
	}
}

func TestHashWriteFailureIsNotFatal(t *testing.T) {
	sink, hashes, next := newSink(t)
	hashes.setErr = errors.New("read-only replica")

	if err := sink.Submit(context.Background(), page(body)); err != nil {
		t.Errorf("the batch was delivered, so failing to record the hash is not an error: %v", err)
	}

	if next.count() != 1 {
		t.Errorf("want 1 batch, got %d", next.count())
	}
}

func TestPageWithoutIndexableTextEmitsEmptyBatch(t *testing.T) {
	sink, hashes, next := newSink(t)

	if err := sink.Submit(context.Background(), page("Loading…")); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if next.count() != 1 || len(next.batches[0].Chunks) != 0 {
		t.Fatalf("want one empty batch so stale chunks for the URL can be dropped, got %+v", next.batches)
	}

	if _, ok := hashes.stored("https://example.com/doc"); !ok {
		t.Error("the empty version must still be recorded, or it is re-sent on every crawl")
	}
}

func TestNilDocumentIsRejected(t *testing.T) {
	sink, _, next := newSink(t)

	if err := sink.Submit(context.Background(), nil); !errors.Is(err, ErrNilDocument) {
		t.Errorf("err = %v, want ErrNilDocument", err)
	}

	if next.count() != 0 {
		t.Error("nothing should reach downstream")
	}
}

func TestShutdownStopsBothDependencies(t *testing.T) {
	sink, hashes, next := newSink(t)
	hashes.shutdownErr = errors.New("hash store")
	next.shutdownErr = errors.New("chunk sink")

	err := sink.Shutdown(context.Background())

	if hashes.shutdowns != 1 || next.shutdowns != 1 {
		t.Errorf("both dependencies must be shut down: hashes=%d next=%d", hashes.shutdowns, next.shutdowns)
	}

	if !errors.Is(err, hashes.shutdownErr) || !errors.Is(err, next.shutdownErr) {
		t.Errorf("both errors must surface, got %v", err)
	}
}

func TestConcurrentSubmits(t *testing.T) {
	sink, _, next := newSink(t)

	var wg sync.WaitGroup
	for i := range 40 {
		wg.Go(func() {
			doc := page(body)
			doc.URL = fmt.Sprintf("https://example.com/%d", i%10)

			if err := sink.Submit(context.Background(), doc); err != nil {
				t.Errorf("Submit: %v", err)
			}
		})
	}

	wg.Wait()

	if next.count() < 10 {
		t.Errorf("every distinct URL must be chunked at least once, got %d batches", next.count())
	}
}
