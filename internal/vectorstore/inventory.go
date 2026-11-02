package vectorstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/milvusclient"
)

// Collection is one of the project's collections: there is one per embedding
// model and vector size that was ever used.
type Collection struct {
	Name      string
	Dimension int
	// From Milvus's statistics: deleted chunks count until a compaction.
	Rows int64
}

// Collections lists the collections of this schema under prefix.
func Collections(ctx context.Context, cfg MilvusConfig, prefix string) ([]Collection, error) {
	client, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: cfg.Address, APIKey: cfg.Token})
	if err != nil {
		return nil, fmt.Errorf("connecting to milvus at %s: %w", cfg.Address, err)
	}
	defer client.Close(ctx)

	names, err := client.ListCollections(ctx, milvusclient.NewListCollectionOption())
	if err != nil {
		return nil, fmt.Errorf("listing collections: %w", err)
	}

	var out []Collection

	for _, name := range names {
		if !strings.HasPrefix(name, prefix+"_"+schemaVersion+"_") {
			continue
		}

		m := &Milvus{client: client, collection: name}

		dim, err := m.storedDimension(ctx)
		if err != nil {
			return nil, err
		}

		stats, err := client.GetCollectionStats(ctx, milvusclient.NewGetCollectionStatsOption(name))
		if err != nil {
			return nil, fmt.Errorf("reading statistics of %s: %w", name, err)
		}

		rows, _ := strconv.ParseInt(stats["row_count"], 10, 64)
		out = append(out, Collection{Name: name, Dimension: dim, Rows: rows})
	}

	slices.SortFunc(out, func(a, b Collection) int { return strings.Compare(a.Name, b.Name) })

	return out, nil
}

// DropCollection deletes a collection and every chunk in it.
func DropCollection(ctx context.Context, cfg MilvusConfig, name string) error {
	client, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: cfg.Address, APIKey: cfg.Token})
	if err != nil {
		return fmt.Errorf("connecting to milvus at %s: %w", cfg.Address, err)
	}
	defer client.Close(ctx)

	return client.DropCollection(ctx, milvusclient.NewDropCollectionOption(name))
}

// Release unloads the collection from Milvus's memory; its chunks stay.
func (m *Milvus) Release(ctx context.Context) error {
	return m.client.ReleaseCollection(ctx, milvusclient.NewReleaseCollectionOption(m.collection))
}

const urlBatch = 1000

// URLs lists every page the collection holds chunks of, in order.
func (m *Milvus) URLs(ctx context.Context) ([]string, error) {
	it, err := m.client.QueryIterator(ctx, milvusclient.NewQueryIteratorOption(m.collection).
		WithBatchSize(urlBatch).
		WithOutputFields(FieldURL).
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		return nil, fmt.Errorf("iterating %s: %w", m.collection, err)
	}

	seen := map[string]bool{}

	for {
		rs, err := it.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("iterating %s: %w", m.collection, err)
		}

		column := rs.GetColumn(FieldURL)
		for i := 0; column != nil && i < column.Len(); i++ {
			u, err := column.GetAsString(i)
			if err != nil {
				return nil, err
			}

			seen[u] = true
		}
	}

	urls := make([]string, 0, len(seen))
	for u := range seen {
		urls = append(urls, u)
	}

	slices.Sort(urls)

	return urls, nil
}

// Chunks returns the chunks stored for a page, without their vectors, in
// order.
func (m *Milvus) Chunks(ctx context.Context, url string) ([]Row, error) {
	rs, err := m.client.Query(ctx, milvusclient.NewQueryOption(m.collection).
		WithFilter(FieldURL+" == {url}").
		WithTemplateParam("url", url).
		WithOutputFields(FieldID, FieldURL, FieldHost, FieldOrdinal, FieldTitle, FieldText, FieldContentHash, FieldRunID, FieldIndexedAt).
		WithLimit(existingIDsLimit).
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		return nil, fmt.Errorf("querying stored chunks for %s: %w", url, err)
	}

	n := rs.ResultCount
	if n >= existingIDsLimit {
		return nil, fmt.Errorf("%w: %s has at least %d", ErrTooManyChunks, url, existingIDsLimit)
	}

	rows := make([]Row, n)

	text := func(field string, set func(*Row, string)) error {
		column := rs.GetColumn(field)
		for i := 0; column != nil && i < n; i++ {
			v, err := column.GetAsString(i)
			if err != nil {
				return fmt.Errorf("reading %s: %w", field, err)
			}

			set(&rows[i], v)
		}

		return nil
	}

	number := func(field string, set func(*Row, int64)) error {
		column := rs.GetColumn(field)
		for i := 0; column != nil && i < n; i++ {
			v, err := column.GetAsInt64(i)
			if err != nil {
				return fmt.Errorf("reading %s: %w", field, err)
			}

			set(&rows[i], v)
		}

		return nil
	}

	err = errors.Join(
		text(FieldID, func(r *Row, v string) { r.ID = v }),
		text(FieldURL, func(r *Row, v string) { r.URL = v }),
		text(FieldHost, func(r *Row, v string) { r.Host = v }),
		text(FieldTitle, func(r *Row, v string) { r.Title = v }),
		text(FieldText, func(r *Row, v string) { r.Text = v }),
		text(FieldContentHash, func(r *Row, v string) { r.ContentHash = v }),
		text(FieldRunID, func(r *Row, v string) { r.RunID = v }),
		number(FieldOrdinal, func(r *Row, v int64) { r.Ordinal = int32(v) }),
		number(FieldIndexedAt, func(r *Row, v int64) { r.IndexedAt = v }),
	)
	if err != nil {
		return nil, err
	}

	slices.SortFunc(rows, func(a, b Row) int { return int(a.Ordinal - b.Ordinal) })

	return rows, nil
}

// storedDimension is the size of the collection's dense vectors.
func (m *Milvus) storedDimension(ctx context.Context) (int, error) {
	collection, err := m.client.DescribeCollection(ctx, milvusclient.NewDescribeCollectionOption(m.collection))
	if err != nil {
		return 0, fmt.Errorf("describing collection %s: %w", m.collection, err)
	}

	for _, field := range collection.Schema.Fields {
		if field.Name == FieldDense {
			dim, err := field.GetDim()
			return int(dim), err
		}
	}

	return 0, fmt.Errorf("%w: %s has no %s field", ErrSchemaMismatch, m.collection, FieldDense)
}
