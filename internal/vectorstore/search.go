package vectorstore

import (
	"context"
	"fmt"
	"strings"
	"web-crawler/internal/uploads"

	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/index"
	"github.com/milvus-io/milvus/client/v2/milvusclient"
)

// HNSW returns at most ef results, so ef is raised to the limit when below it.
const minHNSWEf = 64

func (m *Milvus) SearchDense(ctx context.Context, vector []float32, limit int, filter Filter) ([]Hit, error) {
	if len(vector) != m.dimension {
		return nil, fmt.Errorf("%w: query vector has %d dimensions, collection has %d", ErrInvalidQuery, len(vector), m.dimension)
	}

	return m.search(ctx, entity.FloatVector(vector), FieldDense, index.NewHNSWAnnParam(max(limit, minHNSWEf)), limit, filter)
}

func (m *Milvus) SearchText(ctx context.Context, query string, limit int, filter Filter) ([]Hit, error) {
	if query == "" {
		return nil, fmt.Errorf("%w: empty text query", ErrInvalidQuery)
	}

	return m.search(ctx, entity.Text(query), FieldSparse, nil, limit, filter)
}

func (m *Milvus) search(ctx context.Context, vector entity.Vector, field string, annParam index.AnnParam, limit int, filter Filter) ([]Hit, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: limit must be > 0", ErrInvalidQuery)
	}

	if filter.URLs != nil && len(filter.URLs) == 0 {
		return []Hit{}, nil
	}

	opt := milvusclient.NewSearchOption(m.collection, limit, []entity.Vector{vector}).
		WithANNSField(field).
		WithOutputFields(FieldID, FieldURL, FieldHost, FieldTitle, FieldText)

	if annParam != nil {
		opt = opt.WithAnnParam(annParam)
	}

	// Template parameters, never string building: the host comes from the request,
	// the URLs from whatever pages were crawled.
	var conditions []string

	if filter.Host != "" {
		conditions = append(conditions, FieldHost+" == {host}")
		opt = opt.WithTemplateParam("host", filter.Host)
	}

	if filter.URLs != nil {
		conditions = append(conditions, FieldURL+" in {urls}")
		opt = opt.WithTemplateParam("urls", filter.URLs)
	} else {
		// An uploaded file is its owner's: only a search restricted to the pages
		// of labs that hold it may find it.
		conditions = append(conditions, FieldHost+" != {uploads}")
		opt = opt.WithTemplateParam("uploads", uploads.Host)
	}

	if len(conditions) > 0 {
		opt = opt.WithFilter(strings.Join(conditions, " && "))
	}

	results, err := m.client.Search(ctx, opt)
	if err != nil {
		return nil, fmt.Errorf("searching %s: %w", field, err)
	}

	if len(results) == 0 {
		return []Hit{}, nil
	}

	rs := results[0]
	if rs.Err != nil {
		return nil, fmt.Errorf("searching %s: %w", field, rs.Err)
	}

	hits := make([]Hit, 0, rs.ResultCount)

	for i := 0; i < rs.ResultCount; i++ {
		hit := Hit{Score: rs.Scores[i]}

		for name, dst := range map[string]*string{
			FieldID: &hit.ID, FieldURL: &hit.URL, FieldHost: &hit.Host, FieldTitle: &hit.Title, FieldText: &hit.Text,
		} {
			column := rs.GetColumn(name)
			if column == nil {
				return nil, fmt.Errorf("searching %s: result has no %s", field, name)
			}

			if *dst, err = column.GetAsString(i); err != nil {
				return nil, fmt.Errorf("searching %s: reading %s: %w", field, name, err)
			}
		}

		hits = append(hits, hit)
	}

	return hits, nil
}
