package indexing

import (
	"context"
	"errors"
	"testing"
	"time"
	"web-crawler/internal/chunker"
	"web-crawler/internal/documents"
	"web-crawler/internal/vectorstore"
)

type fakeEmbedder struct {
	calls [][]string
	err   error
}

func (f *fakeEmbedder) Model() string { return "fake" }

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls = append(f.calls, texts)
	if f.err != nil {
		return nil, f.err
	}

	out := make([][]float32, len(texts))
	for i, text := range texts {
		out[i] = []float32{float32(i), float32(len(text))}
	}

	return out, nil
}

type replaceCall struct {
	url  string
	rows []vectorstore.Row
}

type fakeStore struct {
	replaced []replaceCall
	err      error
	closed   bool
}

func (f *fakeStore) Collection() string { return "chunks_v1_fake_2" }

func (f *fakeStore) Replace(_ context.Context, url string, rows []vectorstore.Row) error {
	f.replaced = append(f.replaced, replaceCall{url: url, rows: rows})
	return f.err
}

func (f *fakeStore) Close(context.Context) error {
	f.closed = true
	return nil
}

func vectorBatch() *Batch {
	doc := &documents.Document{URL: "https://docs.example.com/page", Title: "Page", RunID: "run-1"}

	return &Batch{
		Document: doc,
		Hash:     "hash-1",
		Chunks: []chunker.Chunk{
			{ID: "aa01", URL: doc.URL, Title: doc.Title, Ordinal: 0, Text: "first"},
			{ID: "aa02", URL: doc.URL, Title: doc.Title, Ordinal: 1, Text: "second"},
		},
	}
}

func TestVectorSinkBuildsRowsFromChunks(t *testing.T) {
	embedder, store := &fakeEmbedder{}, &fakeStore{}
	sink := NewVectorSink(embedder, store)
	sink.now = func() time.Time { return time.UnixMilli(1700000000000) }

	if err := sink.Submit(context.Background(), vectorBatch()); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if len(embedder.calls) != 1 || embedder.calls[0][0] != "Page\n\nfirst" {
		t.Fatalf("chunks must be embedded with their title, got %q", embedder.calls)
	}

	if len(store.replaced) != 1 || store.replaced[0].url != "https://docs.example.com/page" {
		t.Fatalf("want one Replace for the page, got %+v", store.replaced)
	}

	rows := store.replaced[0].rows
	want := vectorstore.Row{
		ID: "aa02", URL: "https://docs.example.com/page", Host: "docs.example.com", Ordinal: 1,
		Title: "Page", Text: "Page\n\nsecond", ContentHash: "hash-1", RunID: "run-1",
		IndexedAt: 1700000000000, Vector: []float32{1, float32(len("Page\n\nsecond"))},
	}

	got := rows[1]
	if got.ID != want.ID || got.URL != want.URL || got.Host != want.Host || got.Ordinal != want.Ordinal ||
		got.Title != want.Title || got.Text != want.Text || got.ContentHash != want.ContentHash ||
		got.RunID != want.RunID || got.IndexedAt != want.IndexedAt ||
		got.Vector[0] != want.Vector[0] || got.Vector[1] != want.Vector[1] {
		t.Errorf("row =\n%+v\nwant\n%+v", got, want)
	}
}

func TestVectorSinkEmptyBatchClearsURLWithoutEmbedding(t *testing.T) {
	embedder, store := &fakeEmbedder{}, &fakeStore{}

	batch := vectorBatch()
	batch.Chunks = nil

	if err := NewVectorSink(embedder, store).Submit(context.Background(), batch); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if len(embedder.calls) != 0 {
		t.Error("nothing to embed, so the embedder must not be called")
	}

	if len(store.replaced) != 1 || len(store.replaced[0].rows) != 0 {
		t.Errorf("an empty batch must replace the URL's rows with nothing, got %+v", store.replaced)
	}
}

func TestVectorSinkEmbeddingFailureWritesNothing(t *testing.T) {
	embedder, store := &fakeEmbedder{err: errors.New("endpoint down")}, &fakeStore{}

	if err := NewVectorSink(embedder, store).Submit(context.Background(), vectorBatch()); err == nil {
		t.Fatal("expected an error")
	}

	if len(store.replaced) != 0 {
		t.Error("a failed embedding must leave the stored rows alone")
	}
}

func TestVectorSinkReturnsStoreErrors(t *testing.T) {
	store := &fakeStore{err: errors.New("milvus down")}

	if err := NewVectorSink(&fakeEmbedder{}, store).Submit(context.Background(), vectorBatch()); !errors.Is(err, store.err) {
		t.Errorf("err = %v, want the store error", err)
	}
}

func TestVectorSinkFingerprintIsTheCollection(t *testing.T) {
	store := &fakeStore{}
	sink := NewVectorSink(&fakeEmbedder{}, store)

	if sink.Fingerprint() != store.Collection() {
		t.Errorf("Fingerprint = %q, want %q", sink.Fingerprint(), store.Collection())
	}

	_ = sink.Shutdown(context.Background())
	if !store.closed {
		t.Error("Shutdown must close the store")
	}
}
