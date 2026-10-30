//go:build integration

package vectorstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/milvusclient"
)

const testDim = 4

func milvusAddr() string {
	if addr := os.Getenv("MILVUS_ADDR"); addr != "" {
		return addr
	}
	return "localhost:19530"
}

func openTestStore(t *testing.T) *Milvus {
	t.Helper()

	ctx := context.Background()
	name := fmt.Sprintf("it_%d", time.Now().UnixNano())

	m, err := NewMilvus(ctx, MilvusConfig{Address: milvusAddr(), Collection: name, Dimension: testDim})
	if err != nil {
		t.Fatalf("NewMilvus: %v", err)
	}

	t.Cleanup(func() {
		_ = m.client.DropCollection(context.Background(), milvusclient.NewDropCollectionOption(name))
		_ = m.Close(context.Background())
	})

	return m
}

func row(url, id, text string, vector ...float32) Row {
	return Row{ID: id, URL: url, Host: "example.com", Title: "T", Text: text, ContentHash: "h", RunID: "r", Vector: vector}
}

func storedIDs(t *testing.T, m *Milvus, url string) []string {
	t.Helper()

	ids, err := m.existingIDs(context.Background(), url)
	if err != nil {
		t.Fatalf("existingIDs: %v", err)
	}

	slices.Sort(ids)

	return ids
}

func TestReplaceMakesStoredRowsMatch(t *testing.T) {
	m := openTestStore(t)
	ctx := context.Background()
	const a, b = "https://example.com/a", "https://example.com/b"

	if err := m.Replace(ctx, a, []Row{
		row(a, "aa01", "first", 1, 0, 0, 0),
		row(a, "aa02", "second", 0, 1, 0, 0),
		row(a, "aa03", "third", 0, 0, 1, 0),
	}); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	if got := storedIDs(t, m, a); !slices.Equal(got, []string{"aa01", "aa02", "aa03"}) {
		t.Fatalf("after insert: %v", got)
	}

	if err := m.Replace(ctx, a, []Row{
		row(a, "aa01", "first", 1, 0, 0, 0),
		row(a, "aa04", "new fourth", 0, 0, 0, 1),
	}); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	if got := storedIDs(t, m, a); !slices.Equal(got, []string{"aa01", "aa04"}) {
		t.Fatalf("stale chunks must be deleted and new ones added, got %v", got)
	}

	if err := m.Replace(ctx, b, []Row{row(b, "bb01", "other page", 1, 1, 0, 0)}); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	if err := m.Replace(ctx, a, nil); err != nil {
		t.Fatalf("Replace with no rows: %v", err)
	}

	if got := storedIDs(t, m, a); len(got) != 0 {
		t.Errorf("an empty batch must remove the URL, got %v", got)
	}

	if got := storedIDs(t, m, b); !slices.Equal(got, []string{"bb01"}) {
		t.Errorf("replacing one URL must not touch another, got %v", got)
	}
}

func TestReplaceIsSafeForHostileURLs(t *testing.T) {
	m := openTestStore(t)
	ctx := context.Background()
	const hostile = `https://example.com/x" or page_url != "\y`

	if err := m.Replace(ctx, hostile, []Row{row(hostile, "cc01", "text", 1, 0, 0, 0)}); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	const bystander = "https://example.com/bystander"
	if err := m.Replace(ctx, bystander, []Row{row(bystander, "dd01", "text", 0, 1, 0, 0)}); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	if err := m.Replace(ctx, hostile, nil); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	if got := storedIDs(t, m, bystander); !slices.Equal(got, []string{"dd01"}) {
		t.Errorf("a quote in one URL must not widen the delete to other URLs, got %v", got)
	}
}

func TestBM25StemsRussianAndEnglish(t *testing.T) {
	m := openTestStore(t)
	ctx := context.Background()
	const url = "https://example.com/vitamin"

	if err := m.Replace(ctx, url, []Row{
		row(url, "ee01", "Разбираемся с витамином D: пить или нет", 1, 0, 0, 0),
		row(url, "ee02", "The configuration runs every night", 0, 1, 0, 0),
		row(url, "ee03", "Совершенно посторонний текст про погоду", 0, 0, 1, 0),
	}); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	for query, want := range map[string]string{
		"витамины":        "ee01",
		"configured runs": "ee02",
	} {
		results, err := m.client.Search(ctx, milvusclient.NewSearchOption(m.collection, 3, []entity.Vector{entity.Text(query)}).
			WithANNSField(FieldSparse).
			WithOutputFields(FieldID).
			WithConsistencyLevel(entity.ClStrong))
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}

		if results[0].ResultCount == 0 {
			t.Errorf("query %q matched nothing", query)
			continue
		}

		top, _ := results[0].GetColumn(FieldID).GetAsString(0)
		if top != want {
			t.Errorf("query %q: top hit %s, want %s", query, top, want)
		}
	}
}

func TestDenseSearchFindsNearest(t *testing.T) {
	m := openTestStore(t)
	ctx := context.Background()
	const url = "https://example.com/dense"

	if err := m.Replace(ctx, url, []Row{
		row(url, "ff01", "x", 1, 0, 0, 0),
		row(url, "ff02", "y", 0, 1, 0, 0),
	}); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	results, err := m.client.Search(ctx, milvusclient.NewSearchOption(m.collection, 1, []entity.Vector{entity.FloatVector{0.1, 0.9, 0, 0}}).
		WithANNSField(FieldDense).
		WithOutputFields(FieldID).
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if top, _ := results[0].GetColumn(FieldID).GetAsString(0); top != "ff02" {
		t.Errorf("nearest to (0.1, 0.9) should be ff02, got %s", top)
	}
}

func TestReopenChecksDimension(t *testing.T) {
	m := openTestStore(t)
	ctx := context.Background()

	same, err := NewMilvus(ctx, MilvusConfig{Address: milvusAddr(), Collection: m.collection, Dimension: testDim})
	if err != nil {
		t.Fatalf("reopening with the same dimension: %v", err)
	}
	_ = same.Close(ctx)

	if _, err := NewMilvus(ctx, MilvusConfig{Address: milvusAddr(), Collection: m.collection, Dimension: testDim * 2}); !errors.Is(err, ErrSchemaMismatch) {
		t.Errorf("err = %v, want ErrSchemaMismatch", err)
	}
}

func TestReplaceRejectsInvalidRows(t *testing.T) {
	m := openTestStore(t)
	ctx := context.Background()
	const url = "https://example.com/v"

	for name, r := range map[string]Row{
		"wrong url":       row("https://example.com/other", "aa", "t", 1, 0, 0, 0),
		"non-hex id":      row(url, "not-hex", "t", 1, 0, 0, 0),
		"wrong dimension": row(url, "aa", "t", 1, 0),
	} {
		if err := m.Replace(ctx, url, []Row{r}); !errors.Is(err, ErrInvalidRow) {
			t.Errorf("%s: err = %v, want ErrInvalidRow", name, err)
		}
	}
}

func TestSearchReturnsHitsWithTheirFields(t *testing.T) {
	m := openTestStore(t)
	ctx := context.Background()
	const a, b = "https://example.com/a", "https://other.example/b"

	if err := m.Replace(ctx, a, []Row{row(a, "ab01", "goroutines and channels", 1, 0, 0, 0)}); err != nil {
		t.Fatal(err)
	}

	other := row(b, "ab02", "channels in another site", 0.9, 0.1, 0, 0)
	other.Host = "other.example"
	if err := m.Replace(ctx, b, []Row{other}); err != nil {
		t.Fatal(err)
	}

	// Searches are eventually consistent; Replace's strong query does not make
	// the rows visible to them.
	waitFor := func(search func() ([]Hit, error), want int) []Hit {
		t.Helper()

		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(200 * time.Millisecond) {
			hits, err := search()
			if err != nil {
				t.Fatal(err)
			}

			if len(hits) >= want || time.Now().After(deadline) {
				return hits
			}
		}
	}

	dense := waitFor(func() ([]Hit, error) { return m.SearchDense(ctx, []float32{1, 0, 0, 0}, 5, Filter{}) }, 2)
	if len(dense) != 2 || dense[0].ID != "ab01" || dense[0].URL != a || dense[0].Text != "goroutines and channels" || dense[0].Score <= dense[1].Score {
		t.Errorf("dense hits = %+v", dense)
	}

	text := waitFor(func() ([]Hit, error) { return m.SearchText(ctx, "goroutine", 5, Filter{}) }, 1)
	if len(text) != 1 || text[0].ID != "ab01" || text[0].Score <= 0 {
		t.Errorf("text hits = %+v", text)
	}

	onlyOther, err := m.SearchDense(ctx, []float32{1, 0, 0, 0}, 5, Filter{Host: "other.example"})
	if err != nil || len(onlyOther) != 1 || onlyOther[0].Host != "other.example" {
		t.Errorf("host filter: %+v, %v", onlyOther, err)
	}

	hostile, err := m.SearchText(ctx, "channels", 5, Filter{Host: `x" or host != "y`})
	if err != nil || len(hostile) != 0 {
		t.Errorf("a quote in the host must not widen the filter: %+v, %v", hostile, err)
	}

	onlyA, err := m.SearchDense(ctx, []float32{0, 1, 0, 0}, 5, Filter{URLs: []string{a, "https://never.example/"}})
	if err != nil || len(onlyA) != 1 || onlyA[0].URL != a {
		t.Errorf("URL filter: %+v, %v", onlyA, err)
	}

	both, err := m.SearchText(ctx, "channels", 5, Filter{URLs: []string{a, b}, Host: "other.example"})
	if err != nil || len(both) != 1 || both[0].URL != b {
		t.Errorf("URL and host filters together: %+v, %v", both, err)
	}

	for _, urls := range [][]string{{}, {`x" or page_url != "y`}, {`"] or page_url != ["`}} {
		hits, err := m.SearchDense(ctx, []float32{1, 0, 0, 0}, 5, Filter{URLs: urls})
		if err != nil || len(hits) != 0 {
			t.Errorf("URLs %q must match nothing: %+v, %v", urls, hits, err)
		}
	}
}

func TestSearchRejectsBadQueries(t *testing.T) {
	m := openTestStore(t)
	ctx := context.Background()

	if _, err := m.SearchDense(ctx, []float32{1, 0}, 5, Filter{}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("wrong dimension: %v", err)
	}

	if _, err := m.SearchText(ctx, "", 5, Filter{}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("empty text: %v", err)
	}
}
