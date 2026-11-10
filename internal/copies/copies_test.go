package copies

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"web-crawler/internal/vectorstore"

	"go.uber.org/zap"
)

// prose is n words drawn from a vocabulary large enough that two texts
// seeded differently share almost no four-word run.
func prose(seed uint64, n int) []string {
	words := make([]string, n)
	x := seed

	for i := range words {
		x = x*6364136223846793005 + 1442695040888963407
		words[i] = fmt.Sprintf("w%d", (x>>33)%2000)
	}

	return words
}

func edited(words []string, every int) []string {
	out := append([]string(nil), words...)
	for i := every / 2; i < len(out); i += every {
		out[i] = "changed"
	}

	return out
}

func TestPagesThatAreOneTextAreSeenAsOne(t *testing.T) {
	page := prose(1, 600)
	sketch := func(words []string) Sketch { return SketchOf(SetOf(strings.Join(words, " "))) }

	header := append(prose(9, 40), page...)
	footer := append(append([]string(nil), page...), prose(10, 40)...)

	for name, other := range map[string][]string{
		"the same words, cased and punctuated otherwise": strings.Fields(strings.ToUpper(strings.Join(page, ", "))),
		"a mirror with its own header":                   header,
		"a footer added":                                 footer,
		"one word in fifty changed":                      edited(page, 50),
	} {
		if r := Resemblance(sketch(page), sketch(other)); r < pageResemblance {
			t.Errorf("%s: resemblance %.2f", name, r)
		}
	}

	for name, other := range map[string][]string{
		"another page":                  prose(2, 600),
		"one word in eight changed":     edited(page, 8),
		"the first quarter of the page": page[:150],
	} {
		if r := Resemblance(sketch(page), sketch(other)); r >= pageResemblance {
			t.Errorf("%s: resemblance %.2f; not one text", name, r)
		}
	}

	if SketchOf(SetOf("too short to say")) != nil || len(Sketch(nil).Bytes()) != 0 || Same(nil, nil) {
		t.Error("a short page has no sketch, and no sketch matches nothing")
	}

	s := sketch(page)
	if back := SketchFrom(s.Bytes()); Resemblance(s, back) != 1 || SketchFrom([]byte{1, 2, 3}) != nil {
		t.Error("a stored sketch reads back as it was")
	}
}

func TestExcerptsOfOneTextRepeatEachOther(t *testing.T) {
	page := prose(3, 400)
	text := func(w []string) string { return strings.Join(w, " ") }

	// The same page chunked at other places: the excerpts overlap mostly.
	if !Repeats(SetOf(text(page[0:200])), SetOf(text(page[20:200]), text(page[200:260]))) {
		t.Error("chunks cut at other places")
	}

	if Repeats(SetOf(text(page[0:200])), SetOf(text(page[200:400]))) {
		t.Error("two parts of one page are not copies of each other")
	}

	if Repeats(SetOf("a b c d e"), SetOf("a b c d e")) {
		t.Error("a phrase too short to tell")
	}
}

func TestCandidatesFindTheCopiesAmongMany(t *testing.T) {
	var sketches []Sketch
	for i := range 200 {
		sketches = append(sketches, SketchOf(SetOf(strings.Join(prose(uint64(100+i), 300), " "))))
	}

	copied := SketchOf(SetOf(strings.Join(edited(prose(150, 300), 60), " ")))
	sketches = append(sketches, nil, copied)

	found := false

	for _, pair := range Candidates(sketches) {
		if pair[0] >= pair[1] {
			t.Fatalf("pair %v out of order", pair)
		}

		if Same(sketches[pair[0]], sketches[pair[1]]) {
			if pair != [2]int{50, 201} {
				t.Errorf("%v taken for copies", pair)
			}

			found = true
		}
	}

	if !found {
		t.Error("the copy was not among the candidates")
	}
}

type memSketches struct {
	unsketched []string
	saved      map[string]Sketch
}

func (m *memSketches) Unsketched(_ context.Context, limit int) ([]string, error) {
	var out []string

	for _, u := range m.unsketched {
		if _, ok := m.saved[u]; !ok && len(out) < limit {
			out = append(out, u)
		}
	}

	return out, nil
}

func (m *memSketches) SaveSketch(_ context.Context, url string, s Sketch) error {
	m.saved[url] = s
	return nil
}

type memChunks map[string][]vectorstore.Row

func (m memChunks) Chunks(_ context.Context, url string) ([]vectorstore.Row, error) {
	switch url {
	case "huge":
		return nil, fmt.Errorf("%w: huge", vectorstore.ErrTooManyChunks)
	case "down":
		return nil, errors.New("milvus is down")
	}

	return m[url], nil
}

func TestBackfillSketchesFromStoredChunks(t *testing.T) {
	page := strings.Join(prose(4, 300), " ")
	title := strings.Join(prose(5, 30), " ")

	store := &memSketches{unsketched: []string{"a", "huge", "thin"}, saved: map[string]Sketch{}}
	chunks := memChunks{
		"a": {
			{Title: title, Text: title + "\n\n" + page[:len(page)/2]},
			{Title: title, Text: title + "\n\n" + page[len(page)/2:]},
		},
		"thin": {{Text: "a few words"}},
	}

	Backfill(context.Background(), zap.NewNop().Sugar(), store, chunks)

	if len(store.saved) != 3 {
		t.Fatalf("saved %v; every page is looked at once", store.saved)
	}

	if r := Resemblance(store.saved["a"], SketchOf(SetOf(page))); r < 0.9 {
		t.Errorf("a page sketched from its chunks resembles its whole text at %.2f; the title is not the text", r)
	}

	if store.saved["huge"] != nil || store.saved["thin"] != nil {
		t.Error("pages that cannot be sketched get an empty sketch")
	}

	down := &memSketches{unsketched: []string{"down", "a"}, saved: map[string]Sketch{}}
	Backfill(context.Background(), zap.NewNop().Sugar(), down, chunks)

	if len(down.saved) != 0 {
		t.Errorf("saved %v; with the index down, the next start goes on", down.saved)
	}
}
