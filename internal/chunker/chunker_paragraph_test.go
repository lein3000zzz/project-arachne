package chunker

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
	"web-crawler/internal/documents"
)

func newChunker(t *testing.T, maxRunes, overlap, minRunes int) *ParagraphChunker {
	t.Helper()

	c, err := NewParagraphChunker(Settings{MaxRunes: maxRunes, OverlapRunes: overlap, MinDocumentRunes: minRunes})
	if err != nil {
		t.Fatalf("NewParagraphChunker: %v", err)
	}

	return c
}

func doc(content string) *documents.Document {
	return &documents.Document{URL: "https://example.com/page", Title: "Page", Content: content}
}

func sentences(prefix string, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("%s sentence number %d ends here.", prefix, i)
	}

	return strings.Join(parts, " ")
}

func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

func TestNewParagraphChunkerRejectsBadSettings(t *testing.T) {
	cases := []struct {
		name     string
		settings Settings
	}{
		{"zero max", Settings{MaxRunes: 0}},
		{"negative max", Settings{MaxRunes: -5}},
		{"negative overlap", Settings{MaxRunes: 100, OverlapRunes: -1}},
		{"overlap equals max", Settings{MaxRunes: 100, OverlapRunes: 100}},
		{"overlap over max", Settings{MaxRunes: 100, OverlapRunes: 150}},
		{"negative minimum", Settings{MaxRunes: 100, MinDocumentRunes: -1}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewParagraphChunker(tc.settings); !errors.Is(err, ErrInvalidSettings) {
				t.Errorf("err = %v, want ErrInvalidSettings", err)
			}
		})
	}
}

func TestChunkProducesNothingForEmptyOrTinyDocuments(t *testing.T) {
	c := newChunker(t, 200, 20, 80)

	cases := []struct {
		name string
		doc  *documents.Document
	}{
		{"nil document", nil},
		{"empty content", doc("")},
		{"whitespace only", doc(" \n\n\t \n")},
		{"below minimum", doc("Please enable JavaScript to run this app.")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Chunk(tc.doc); len(got) != 0 {
				t.Errorf("want no chunks, got %d: %+v", len(got), got)
			}
		})
	}
}

func TestShortDocumentIsOneVerbatimChunk(t *testing.T) {
	const content = "Example Domain\n\nThis domain is for use in documentation examples without needing permission."

	chunks := newChunker(t, 1200, 150, 80).Chunk(doc(content))
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}

	if chunks[0].Text != content {
		t.Errorf("text = %q, want %q", chunks[0].Text, content)
	}

	if chunks[0].Ordinal != 0 || chunks[0].URL != "https://example.com/page" || chunks[0].Title != "Page" {
		t.Errorf("metadata not carried: %+v", chunks[0])
	}
}

func TestSizesAreMeasuredInRunesNotBytes(t *testing.T) {
	// 60 Cyrillic runes are 120 bytes: a byte-based limit of 100 would split this.
	paragraph := strings.Repeat("щ", 60)
	if len(paragraph) <= 100 || utf8.RuneCountInString(paragraph) > 100 {
		t.Fatal("fixture no longer exercises the byte/rune difference")
	}

	chunks := newChunker(t, 100, 0, 0).Chunk(doc(paragraph))
	if len(chunks) != 1 || chunks[0].Text != paragraph {
		t.Fatalf("a %d-rune paragraph must fit a 100-rune chunk whole, got %d chunk(s)", 60, len(chunks))
	}
}

func TestCyrillicDocumentSplitsWithinLimit(t *testing.T) {
	var b strings.Builder
	for i := range 40 {
		fmt.Fprintf(&b, "Предложение номер %d заканчивается здесь. ", i)
	}

	chunks := newChunker(t, 300, 60, 0).Chunk(doc(b.String()))
	if len(chunks) < 2 {
		t.Fatalf("want the document split, got %d chunk(s)", len(chunks))
	}

	for _, chunk := range chunks {
		if n := utf8.RuneCountInString(chunk.Text); n > 300 {
			t.Errorf("chunk %d is %d runes, over the 300 limit", chunk.Ordinal, n)
		}

		if !strings.HasSuffix(chunk.Text, ".") {
			t.Errorf("chunk %d should end on a sentence boundary: %q", chunk.Ordinal, chunk.Text)
		}
	}
}

func TestParagraphsThatFitAreNeverSplit(t *testing.T) {
	paragraphs := []string{
		sentences("alpha", 3),
		sentences("beta", 4),
		sentences("gamma", 2),
		sentences("delta", 5),
		sentences("epsilon", 3),
	}

	chunks := newChunker(t, 400, 0, 0).Chunk(doc(strings.Join(paragraphs, "\n\n")))

	for _, paragraph := range paragraphs {
		whole := false
		for _, chunk := range chunks {
			if strings.Contains(chunk.Text, paragraph) {
				whole = true
				break
			}
		}

		if !whole {
			t.Errorf("paragraph was split across chunks: %q", paragraph[:40])
		}
	}
}

func TestLongParagraphSplitsOnSentencesWithOverlap(t *testing.T) {
	chunks := newChunker(t, 200, 60, 0).Chunk(doc(sentences("long", 20)))
	if len(chunks) < 3 {
		t.Fatalf("want several chunks, got %d", len(chunks))
	}

	for i := 1; i < len(chunks); i++ {
		prev := chunks[i-1].Text
		lastSentence := prev[strings.LastIndex(prev, "long sentence"):]

		if !strings.HasPrefix(chunks[i].Text, lastSentence) {
			t.Errorf("chunk %d should open with the last sentence of chunk %d (%q), got %q",
				i, i-1, lastSentence, chunks[i].Text)
		}
	}
}

func TestNoOverlapWhenDisabled(t *testing.T) {
	chunks := newChunker(t, 200, 0, 0).Chunk(doc(sentences("long", 20)))

	seen := make(map[string]int)
	for _, chunk := range chunks {
		for _, s := range splitSentences(chunk.Text) {
			seen[s]++
		}
	}

	for s, n := range seen {
		if n > 1 {
			t.Errorf("sentence repeated %d times with overlap disabled: %q", n, s)
		}
	}
}

func TestListLinesSurviveVerbatim(t *testing.T) {
	const content = "Supported formats:\nfirst item\nsecond item\nthird item"

	chunks := newChunker(t, 1200, 0, 0).Chunk(doc(content))
	if len(chunks) != 1 || chunks[0].Text != content {
		t.Fatalf("list lines must be kept as lines, got %+v", chunks)
	}
}

func TestGiantWordIsSplitAndReassembles(t *testing.T) {
	// Non-periodic on purpose: identical slices would share an ID and be deduped.
	alphabet := []rune("abcdefghijklmnopqrstuvwxyzабвгдежзийклмнопрстуфхцчшщ")
	r := rand.New(rand.NewSource(3))

	runes := make([]rune, 3200)
	for i := range runes {
		runes[i] = alphabet[r.Intn(len(alphabet))]
	}

	word := string(runes)

	chunks := newChunker(t, 1000, 0, 0).Chunk(doc(word))

	var rebuilt strings.Builder
	for _, chunk := range chunks {
		if n := utf8.RuneCountInString(chunk.Text); n > 1000 {
			t.Errorf("chunk %d is %d runes", chunk.Ordinal, n)
		}

		rebuilt.WriteString(chunk.Text)
	}

	if rebuilt.String() != word {
		t.Error("a word longer than the limit must be recoverable by concatenating its pieces")
	}
}

func TestIDsAreDeterministic(t *testing.T) {
	c := newChunker(t, 200, 40, 0)
	content := sentences("stable", 15)

	first, second := c.Chunk(doc(content)), c.Chunk(doc(content))
	if len(first) != len(second) {
		t.Fatalf("chunk count changed between runs: %d vs %d", len(first), len(second))
	}

	for i := range first {
		if first[i].ID != second[i].ID {
			t.Errorf("chunk %d ID changed between identical runs", i)
		}

		if len(first[i].ID) != idHexLength {
			t.Errorf("chunk %d ID has length %d, want %d", i, len(first[i].ID), idHexLength)
		}
	}
}

func TestEditingTheLastParagraphKeepsEarlierIDs(t *testing.T) {
	c := newChunker(t, 300, 0, 0)
	head := []string{sentences("one", 4), sentences("two", 4), sentences("three", 4)}

	before := c.Chunk(doc(strings.Join(append(head, "The original ending."), "\n\n")))
	after := c.Chunk(doc(strings.Join(append(head, "A rewritten ending."), "\n\n")))

	if len(before) != len(after) || len(before) < 2 {
		t.Fatalf("fixture should give the same multi-chunk layout, got %d and %d", len(before), len(after))
	}

	last := len(before) - 1
	for i := 0; i < last; i++ {
		if before[i].ID != after[i].ID {
			t.Errorf("chunk %d did not change but its ID did", i)
		}
	}

	if before[last].ID == after[last].ID {
		t.Error("the edited chunk kept its ID")
	}
}

func TestIDsDependOnURLAndTitle(t *testing.T) {
	c := newChunker(t, 1200, 0, 0)
	const content = "Identical content published on two different pages of the same site."

	base := c.Chunk(&documents.Document{URL: "https://a.example/", Title: "T", Content: content})[0].ID
	otherURL := c.Chunk(&documents.Document{URL: "https://b.example/", Title: "T", Content: content})[0].ID
	otherTitle := c.Chunk(&documents.Document{URL: "https://a.example/", Title: "U", Content: content})[0].ID

	if base == otherURL {
		t.Error("the same text on two URLs must be two chunks")
	}

	if base == otherTitle {
		t.Error("the title is embedded with the chunk, so changing it must change the ID")
	}
}

func TestDuplicateChunksWithinADocumentCollapse(t *testing.T) {
	repeated := sentences("repeat", 5)

	chunks := newChunker(t, utf8.RuneCountInString(repeated), 0, 0).Chunk(doc(repeated + "\n\n" + repeated))
	if len(chunks) != 1 {
		t.Fatalf("identical chunks share an ID and must collapse to one, got %d", len(chunks))
	}

	if chunks[0].Ordinal != 0 {
		t.Errorf("ordinal = %d, want 0", chunks[0].Ordinal)
	}
}

func TestEmbeddingTextCarriesTitle(t *testing.T) {
	chunk := Chunk{Title: "Widget docs", Text: "Pass the timeout in milliseconds."}
	if got := chunk.EmbeddingText(); got != "Widget docs\n\nPass the timeout in milliseconds." {
		t.Errorf("EmbeddingText = %q", got)
	}

	untitled := Chunk{Text: "Body only."}
	if got := untitled.EmbeddingText(); got != "Body only." {
		t.Errorf("untitled EmbeddingText = %q", got)
	}
}

func TestFingerprintTracksSettings(t *testing.T) {
	base := newChunker(t, 1200, 150, 80).Fingerprint()

	for name, other := range map[string]*ParagraphChunker{
		"max":     newChunker(t, 1000, 150, 80),
		"overlap": newChunker(t, 1200, 100, 80),
		"minimum": newChunker(t, 1200, 150, 40),
	} {
		if other.Fingerprint() == base {
			t.Errorf("changing %s must change the fingerprint", name)
		}
	}

	if newChunker(t, 1200, 150, 80).Fingerprint() != base {
		t.Error("identical settings must give an identical fingerprint")
	}
}

func generateDocument(r *rand.Rand, unique bool) string {
	vocabulary := []string{"alpha", "beta", "configuration", "widget", "тест", "слово", "проверка", "значение", "x", "ёж"}
	terminators := []string{".", "!", "?", "…", ".\""}
	token := 0

	word := func() string {
		if unique {
			token++
			if token%2 == 0 {
				return fmt.Sprintf("w%d", token)
			}
			return fmt.Sprintf("с%d", token)
		}
		return vocabulary[r.Intn(len(vocabulary))]
	}

	paragraphs := make([]string, r.Intn(10)+1)
	for p := range paragraphs {
		lines := make([]string, r.Intn(3)+1)
		for l := range lines {
			sents := make([]string, r.Intn(6)+1)
			for s := range sents {
				words := make([]string, r.Intn(25)+1)
				for w := range words {
					words[w] = word()
				}
				sents[s] = strings.Join(words, " ") + terminators[r.Intn(len(terminators))]
			}
			lines[l] = strings.Join(sents, " ")
		}
		paragraphs[p] = strings.Join(lines, "\n")
	}

	return strings.Join(paragraphs, "\n\n")
}

func TestPropertyChunksNeverExceedLimit(t *testing.T) {
	r := rand.New(rand.NewSource(1))

	for _, s := range []Settings{
		{MaxRunes: 30, OverlapRunes: 0},
		{MaxRunes: 50, OverlapRunes: 10},
		{MaxRunes: 200, OverlapRunes: 60},
		{MaxRunes: 1200, OverlapRunes: 150},
	} {
		c := newChunker(t, s.MaxRunes, s.OverlapRunes, 0)

		for i := range 300 {
			content := generateDocument(r, false)
			if i%10 == 0 {
				content += "\n\n" + strings.Repeat("ы", r.Intn(3000))
			}

			for _, chunk := range c.Chunk(doc(content)) {
				if n := utf8.RuneCountInString(chunk.Text); n > s.MaxRunes {
					t.Fatalf("settings %+v, doc %d: chunk %d is %d runes", s, i, chunk.Ordinal, n)
				}

				if chunk.Text == "" {
					t.Fatalf("settings %+v, doc %d: empty chunk", s, i)
				}
			}
		}
	}
}

func TestPropertyNoOverlapReconstructsInput(t *testing.T) {
	r := rand.New(rand.NewSource(2))

	for _, limit := range []int{30, 80, 300, 1200} {
		c := newChunker(t, limit, 0, 0)

		for i := range 200 {
			content := generateDocument(r, true)

			var rebuilt strings.Builder
			for j, chunk := range c.Chunk(doc(content)) {
				if chunk.Ordinal != j {
					t.Fatalf("limit %d, doc %d: ordinal %d at position %d", limit, i, chunk.Ordinal, j)
				}
				rebuilt.WriteString(chunk.Text)
			}

			if stripSpace(rebuilt.String()) != stripSpace(content) {
				t.Fatalf("limit %d, doc %d: chunks lost, duplicated or reordered content", limit, i)
			}
		}
	}
}
