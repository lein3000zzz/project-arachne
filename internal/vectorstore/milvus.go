package vectorstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/index"
	"github.com/milvus-io/milvus/client/v2/milvusclient"
)

// Part of every collection name. Bump when the schema or the analyzer changes:
// stored rows would no longer be comparable, so a new collection is built instead
// of migrating the old one.
const schemaVersion = "v1"

const (
	FieldID          = "chunk_id"
	FieldURL         = "page_url"
	FieldHost        = "host"
	FieldOrdinal     = "ordinal"
	FieldTitle       = "title"
	FieldText        = "text"
	FieldSparse      = "text_sparse"
	FieldDense       = "dense"
	FieldContentHash = "content_hash"
	FieldRunID       = "run_id"
	FieldIndexedAt   = "indexed_at"
)

// Varchar limits are in bytes, not runes.
const (
	maxIDBytes    = 64
	maxURLBytes   = 4096
	maxHostBytes  = 255
	maxTitleBytes = 4096
	maxTextBytes  = 65535
	maxHashBytes  = 64
	maxRunIDBytes = 64
)

const (
	hnswM              = 16
	hnswEfConstruction = 200
	existingIDsLimit   = 16384
	writeBatchSize     = 500
)

// Russian and English stemming chained: each stemmer leaves the other
// language's words untouched, so one field serves a mixed corpus.
var analyzerParams = map[string]any{
	"tokenizer": "standard",
	"filter": []any{
		"lowercase",
		map[string]any{"type": "stop", "stop_words": []any{"_russian_", "_english_"}},
		map[string]any{"type": "stemmer", "language": "russian"},
		map[string]any{"type": "stemmer", "language": "english"},
	},
}

type MilvusConfig struct {
	Address    string
	Token      string
	Collection string
	Dimension  int
}

type Milvus struct {
	client     *milvusclient.Client
	collection string
	dimension  int
}

func CollectionName(prefix, model string, dimension int) string {
	return fmt.Sprintf("%s_%s_%s_%d", prefix, schemaVersion, sanitize(model), dimension)
}

func NewMilvus(ctx context.Context, cfg MilvusConfig) (*Milvus, error) {
	switch {
	case cfg.Address == "":
		return nil, fmt.Errorf("%w: address is empty", ErrInvalidConfig)
	case cfg.Collection == "":
		return nil, fmt.Errorf("%w: collection is empty", ErrInvalidConfig)
	case cfg.Dimension <= 0:
		return nil, fmt.Errorf("%w: dimension must be > 0", ErrInvalidConfig)
	}

	client, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: cfg.Address, APIKey: cfg.Token})
	if err != nil {
		return nil, fmt.Errorf("connecting to milvus at %s: %w", cfg.Address, err)
	}

	m := &Milvus{client: client, collection: cfg.Collection, dimension: cfg.Dimension}

	if err := m.ensureCollection(ctx); err != nil {
		return nil, errors.Join(err, client.Close(ctx))
	}

	return m, nil
}

func (m *Milvus) Collection() string {
	return m.collection
}

func (m *Milvus) Close(ctx context.Context) error {
	return m.client.Close(ctx)
}

func (m *Milvus) Replace(ctx context.Context, url string, rows []Row) error {
	for i := range rows {
		if err := m.validate(url, &rows[i]); err != nil {
			return err
		}
	}

	existing, err := m.existingIDs(ctx, url)
	if err != nil {
		return err
	}

	for start := 0; start < len(rows); start += writeBatchSize {
		if err := m.upsert(ctx, rows[start:min(start+writeBatchSize, len(rows))]); err != nil {
			return err
		}
	}

	keep := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		keep[row.ID] = struct{}{}
	}

	stale := make([]string, 0)
	for _, id := range existing {
		if _, ok := keep[id]; !ok {
			stale = append(stale, id)
		}
	}

	for start := 0; start < len(stale); start += writeBatchSize {
		if err := m.delete(ctx, stale[start:min(start+writeBatchSize, len(stale))]); err != nil {
			return err
		}
	}

	return nil
}

func (m *Milvus) validate(url string, row *Row) error {
	switch {
	case row.URL != url:
		return fmt.Errorf("%w: row for %q passed with url %q", ErrInvalidRow, row.URL, url)
	case !isHexID(row.ID):
		return fmt.Errorf("%w: id %q is not a hex id of at most %d chars", ErrInvalidRow, row.ID, maxIDBytes)
	case len(row.Vector) != m.dimension:
		return fmt.Errorf("%w: vector has %d dimensions, collection has %d", ErrInvalidRow, len(row.Vector), m.dimension)
	case len(row.URL) > maxURLBytes, len(row.Host) > maxHostBytes, len(row.Title) > maxTitleBytes,
		len(row.Text) > maxTextBytes, len(row.ContentHash) > maxHashBytes, len(row.RunID) > maxRunIDBytes:
		return fmt.Errorf("%w: a field of chunk %s exceeds its byte limit", ErrInvalidRow, row.ID)
	}

	return nil
}

func (m *Milvus) existingIDs(ctx context.Context, url string) ([]string, error) {
	// Strong: the previous version of this page may have been written moments ago,
	// and any row this query misses is never deleted.
	rs, err := m.client.Query(ctx, milvusclient.NewQueryOption(m.collection).
		WithFilter(FieldURL+" == {url}").
		WithTemplateParam("url", url).
		WithOutputFields(FieldID).
		WithLimit(existingIDsLimit).
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		return nil, fmt.Errorf("querying stored chunks for %s: %w", url, err)
	}

	column := rs.GetColumn(FieldID)
	if column == nil {
		return []string{}, nil
	}

	if column.Len() >= existingIDsLimit {
		return nil, fmt.Errorf("%w: %s has at least %d", ErrTooManyChunks, url, existingIDsLimit)
	}

	ids := make([]string, 0, column.Len())
	for i := 0; i < column.Len(); i++ {
		id, err := column.GetAsString(i)
		if err != nil {
			return nil, fmt.Errorf("reading stored chunk id: %w", err)
		}

		ids = append(ids, id)
	}

	return ids, nil
}

func (m *Milvus) upsert(ctx context.Context, rows []Row) error {
	n := len(rows)
	ids, urls, hosts, titles, texts := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	hashes, runIDs := make([]string, n), make([]string, n)
	ordinals, indexedAt := make([]int32, n), make([]int64, n)
	vectors := make([][]float32, n)

	for i, row := range rows {
		ids[i], urls[i], hosts[i], titles[i], texts[i] = row.ID, row.URL, row.Host, row.Title, row.Text
		hashes[i], runIDs[i] = row.ContentHash, row.RunID
		ordinals[i], indexedAt[i] = row.Ordinal, row.IndexedAt
		vectors[i] = row.Vector
	}

	_, err := m.client.Upsert(ctx, milvusclient.NewColumnBasedInsertOption(m.collection).
		WithVarcharColumn(FieldID, ids).
		WithVarcharColumn(FieldURL, urls).
		WithVarcharColumn(FieldHost, hosts).
		WithInt32Column(FieldOrdinal, ordinals).
		WithVarcharColumn(FieldTitle, titles).
		WithVarcharColumn(FieldText, texts).
		WithFloatVectorColumn(FieldDense, m.dimension, vectors).
		WithVarcharColumn(FieldContentHash, hashes).
		WithVarcharColumn(FieldRunID, runIDs).
		WithInt64Column(FieldIndexedAt, indexedAt))
	if err != nil {
		return fmt.Errorf("upserting %d chunks: %w", n, err)
	}

	return nil
}

func (m *Milvus) delete(ctx context.Context, ids []string) error {
	// WithStringIDs quotes but does not escape; only ever hand it hex.
	for _, id := range ids {
		if !isHexID(id) {
			return fmt.Errorf("%w: refusing to delete by non-hex id %q", ErrInvalidRow, id)
		}
	}

	if _, err := m.client.Delete(ctx, milvusclient.NewDeleteOption(m.collection).WithStringIDs(FieldID, ids)); err != nil {
		return fmt.Errorf("deleting %d stale chunks: %w", len(ids), err)
	}

	return nil
}

func (m *Milvus) ensureCollection(ctx context.Context) error {
	has, err := m.client.HasCollection(ctx, milvusclient.NewHasCollectionOption(m.collection))
	if err != nil {
		return fmt.Errorf("checking collection %s: %w", m.collection, err)
	}

	if has {
		if err := m.checkDimension(ctx); err != nil {
			return err
		}
	} else if err := m.create(ctx); err != nil {
		return err
	}

	task, err := m.client.LoadCollection(ctx, milvusclient.NewLoadCollectionOption(m.collection))
	if err != nil {
		return fmt.Errorf("loading collection %s: %w", m.collection, err)
	}

	return task.Await(ctx)
}

func (m *Milvus) checkDimension(ctx context.Context) error {
	collection, err := m.client.DescribeCollection(ctx, milvusclient.NewDescribeCollectionOption(m.collection))
	if err != nil {
		return fmt.Errorf("describing collection %s: %w", m.collection, err)
	}

	for _, field := range collection.Schema.Fields {
		if field.Name != FieldDense {
			continue
		}

		dim, err := field.GetDim()
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrSchemaMismatch, m.collection, err)
		}

		if int(dim) != m.dimension {
			return fmt.Errorf("%w: %s stores %d-dimensional vectors, embedder returns %d",
				ErrSchemaMismatch, m.collection, dim, m.dimension)
		}

		return nil
	}

	return fmt.Errorf("%w: %s has no %s field", ErrSchemaMismatch, m.collection, FieldDense)
}

func (m *Milvus) create(ctx context.Context) error {
	varchar := func(name string, maxBytes int64) *entity.Field {
		return entity.NewField().WithName(name).WithDataType(entity.FieldTypeVarChar).WithMaxLength(maxBytes)
	}

	schema := entity.NewSchema().
		WithName(m.collection).
		WithDynamicFieldEnabled(false).
		WithField(varchar(FieldID, maxIDBytes).WithIsPrimaryKey(true)).
		WithField(varchar(FieldURL, maxURLBytes)).
		WithField(varchar(FieldHost, maxHostBytes).WithIsPartitionKey(true)).
		WithField(entity.NewField().WithName(FieldOrdinal).WithDataType(entity.FieldTypeInt32)).
		WithField(varchar(FieldTitle, maxTitleBytes)).
		WithField(varchar(FieldText, maxTextBytes).WithEnableAnalyzer(true).WithAnalyzerParams(analyzerParams)).
		WithField(entity.NewField().WithName(FieldSparse).WithDataType(entity.FieldTypeSparseVector)).
		WithField(entity.NewField().WithName(FieldDense).WithDataType(entity.FieldTypeFloatVector).WithDim(int64(m.dimension))).
		WithField(varchar(FieldContentHash, maxHashBytes)).
		WithField(varchar(FieldRunID, maxRunIDBytes)).
		WithField(entity.NewField().WithName(FieldIndexedAt).WithDataType(entity.FieldTypeInt64)).
		WithFunction(entity.NewFunction().
			WithName("text_bm25").
			WithType(entity.FunctionTypeBM25).
			WithInputFields(FieldText).
			WithOutputFields(FieldSparse))

	err := m.client.CreateCollection(ctx, milvusclient.NewCreateCollectionOption(m.collection, schema).WithIndexOptions(
		milvusclient.NewCreateIndexOption(m.collection, FieldDense, index.NewHNSWIndex(entity.COSINE, hnswM, hnswEfConstruction)),
		milvusclient.NewCreateIndexOption(m.collection, FieldSparse, index.NewSparseInvertedIndex(entity.BM25, 0)),
		milvusclient.NewCreateIndexOption(m.collection, FieldURL, index.NewInvertedIndex()),
	))
	if err != nil {
		return fmt.Errorf("creating collection %s: %w", m.collection, err)
	}

	return nil
}

func isHexID(id string) bool {
	if id == "" || len(id) > maxIDBytes {
		return false
	}

	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}

	return true
}

func sanitize(s string) string {
	var b strings.Builder

	underscore := false
	for _, r := range strings.ToLower(s) {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
			underscore = false
		} else if !underscore {
			b.WriteByte('_')
			underscore = true
		}
	}

	return strings.Trim(b.String(), "_")
}
