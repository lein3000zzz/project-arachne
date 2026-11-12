package facts

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"web-crawler/internal/llm"
	"web-crawler/internal/vectorstore"

	"go.uber.org/zap"
)

func TestAPagesChunksArePlannedAcrossIt(t *testing.T) {
	for _, c := range []struct {
		chunks, most int
		want         []int
	}{
		{3, 12, []int{0, 1, 2}},
		{100, 4, []int{0, 1, 33, 99}},
		{13, 12, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 12}},
		{5, 1, []int{0, 4}},
	} {
		got := Plan(c.chunks, c.most)
		if c.most > 1 && !slices.Equal(got, c.want) {
			t.Errorf("%d of %d: %v, want %v", c.most, c.chunks, got, c.want)
		}

		for _, n := range got {
			if n < 0 || n >= c.chunks {
				t.Errorf("%d of %d: chunk %d does not exist", c.most, c.chunks, n)
			}
		}
	}

	for chunks := 1; chunks < 60; chunks++ {
		for most := 2; most < 15; most++ {
			if got := Plan(chunks, most); len(got) != min(chunks, most) {
				t.Fatalf("%d of %d: %v", most, chunks, got)
			}
		}
	}
}

type memStore struct {
	due      []Due
	versions []string
	saved    map[int]Digested
}

func (m *memStore) Due(context.Context, int) ([]Due, error) { return m.due, nil }

func (m *memStore) Version(_ context.Context, url, review, index string, planned []int, _ []ChunkText) error {
	m.versions = append(m.versions, fmt.Sprint(url, review, index, planned))
	return nil
}

func (m *memStore) Undigested(_ context.Context, _, _ string, ordinals []int) ([]int, error) {
	return slices.DeleteFunc(slices.Clone(ordinals), func(n int) bool { _, ok := m.saved[n]; return ok }), nil
}

func (m *memStore) SaveChunk(_ context.Context, _, _ string, d Digested) error {
	m.saved[d.Ordinal] = d
	return nil
}

func (m *memStore) Lead(context.Context) (func(), bool, error) { return func() {}, true, nil }

type pages map[string][]vectorstore.Row

func (p pages) Chunks(_ context.Context, url string) ([]vectorstore.Row, error) { return p[url], nil }

type replies struct {
	byText map[string]string
	err    error
}

func (r replies) Stream(_ context.Context, req llm.Request, _ func(string) error) (llm.Response, error) {
	for text, reply := range r.byText {
		if strings.Contains(req.Messages[0].Content, text) {
			return llm.Response{Text: reply}, nil
		}
	}

	return llm.Response{}, r.err
}

func (replies) Model() string { return "small" }

func TestDuePagesAreDigestedAsFarAsPlanned(t *testing.T) {
	now := time.Now()
	row := func(n int, text string) vectorstore.Row {
		return vectorstore.Row{URL: "u", Ordinal: int32(n), Text: text, ContentHash: "i1", IndexedAt: now.UnixMilli()}
	}

	chunks := pages{"u": {row(2, "Chiefs outside the constitution compete with the state."), row(0, "Henn measures state capacity by distance to the capital."),
		row(1, "Garbled text the model cannot read at all here.")}}
	model := replies{byText: map[string]string{
		"Henn measures": `{"facts":[{"claim":"Henn measures state capacity by distance to the capital.","about":"state capacity","sentences":[1]}]}`,
		"Garbled":       `I am sorry.`,
		"Chiefs":        `{"facts":[]}`,
	}}

	store := &memStore{due: []Due{{URL: "u", ReviewHash: "r1", ReviewedAt: now.Add(-time.Minute)}}, saved: map[int]Digested{}}
	d := NewDigester(zap.NewNop().Sugar(), store, chunks, NewExtractor(model), DigestConfig{MaxChunks: 12})

	if err := d.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(store.versions) != 1 || store.versions[0] != "ur1i1[0 1 2]" {
		t.Errorf("versions %v", store.versions)
	}

	if len(store.saved) != 3 || len(store.saved[0].Facts) != 1 || store.saved[1].Error == "" || store.saved[2].Error != "" {
		t.Errorf("saved %+v", store.saved)
	}

	// Reviewed after its chunks were indexed: the new version is not in the
	// index yet, so nothing is done with it.
	store.due[0].ReviewedAt, store.versions = now.Add(time.Minute), nil
	if err := d.Sweep(context.Background()); err != nil || store.versions != nil {
		t.Errorf("a version not indexed yet: %v, %v", err, store.versions)
	}
}

func TestAModelThatIsDownIsAskedAgainLater(t *testing.T) {
	store := &memStore{due: []Due{{URL: "u", ReviewHash: "r1"}}, saved: map[int]Digested{}}
	chunks := pages{"u": {{URL: "u", Ordinal: 0, Text: "Henn measures state capacity.", ContentHash: "i1", IndexedAt: time.Now().UnixMilli()}}}
	d := NewDigester(zap.NewNop().Sugar(), store, chunks, NewExtractor(replies{err: errors.New("connection refused")}), DigestConfig{MaxChunks: 4})

	if err := d.Sweep(context.Background()); err == nil || len(store.saved) != 0 {
		t.Errorf("%v, saved %+v: a model that was down is not recorded as an answer", err, store.saved)
	}
}
