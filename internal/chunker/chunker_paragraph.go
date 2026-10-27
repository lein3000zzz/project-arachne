package chunker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
	"web-crawler/internal/documents"
)

const paragraphAlgorithmVersion = "paragraph/v1"

const paragraphBreakFill = 0.5

const (
	paragraphSeparator      = "\n\n"
	paragraphSeparatorRunes = 2
	idHexLength             = 32
)

type Settings struct {
	MaxRunes         int
	OverlapRunes     int
	MinDocumentRunes int
}

type ParagraphChunker struct {
	settings Settings
}

type unit struct {
	text           string
	runes          int
	paragraph      int
	lead           string
	first          bool
	paragraphRunes int
}

func NewParagraphChunker(settings Settings) (*ParagraphChunker, error) {
	switch {
	case settings.MaxRunes <= 0:
		return nil, fmt.Errorf("%w: max runes must be > 0, got %d", ErrInvalidSettings, settings.MaxRunes)
	case settings.OverlapRunes < 0 || settings.OverlapRunes >= settings.MaxRunes:
		return nil, fmt.Errorf("%w: overlap runes must be in [0, %d), got %d",
			ErrInvalidSettings, settings.MaxRunes, settings.OverlapRunes)
	case settings.MinDocumentRunes < 0:
		return nil, fmt.Errorf("%w: min document runes must be >= 0, got %d", ErrInvalidSettings, settings.MinDocumentRunes)
	}

	return &ParagraphChunker{settings: settings}, nil
}

func (c *ParagraphChunker) Fingerprint() string {
	return fmt.Sprintf("%s:max=%d,overlap=%d,min=%d",
		paragraphAlgorithmVersion, c.settings.MaxRunes, c.settings.OverlapRunes, c.settings.MinDocumentRunes)
}

func (c *ParagraphChunker) Chunk(doc *documents.Document) []Chunk {
	if doc == nil {
		return nil
	}

	paragraphs := splitParagraphs(doc.Content)
	if len(paragraphs) == 0 || contentRunes(paragraphs) < c.settings.MinDocumentRunes {
		return nil
	}

	texts := c.pack(c.units(paragraphs))

	chunks := make([]Chunk, 0, len(texts))
	seen := make(map[string]struct{}, len(texts))

	for _, text := range texts {
		chunk := Chunk{
			URL:   doc.URL,
			Title: doc.Title,
			Text:  text,
		}
		chunk.ID = chunkID(doc.URL, chunk.EmbeddingText())

		if _, duplicate := seen[chunk.ID]; duplicate {
			continue
		}

		seen[chunk.ID] = struct{}{}
		chunk.Ordinal = len(chunks)
		chunks = append(chunks, chunk)
	}

	return chunks
}

func (c *ParagraphChunker) units(paragraphs []string) []unit {
	out := make([]unit, 0, len(paragraphs))

	for i, paragraph := range paragraphs {
		paragraphRunes := utf8.RuneCountInString(paragraph)

		if paragraphRunes <= c.settings.MaxRunes {
			out = append(out, unit{
				text:           paragraph,
				runes:          paragraphRunes,
				paragraph:      i,
				first:          true,
				paragraphRunes: paragraphRunes,
			})

			continue
		}

		for j, p := range splitOversized(paragraph, c.settings.MaxRunes) {
			u := unit{text: p.text, runes: utf8.RuneCountInString(p.text), paragraph: i, lead: p.lead}
			if j == 0 {
				u.first = true
				u.lead = ""
				u.paragraphRunes = paragraphRunes
			}

			out = append(out, u)
		}
	}

	return out
}

func (c *ParagraphChunker) pack(units []unit) []string {
	limit := c.settings.MaxRunes
	fill := int(paragraphBreakFill * float64(limit))

	texts := make([]string, 0)
	current := make([]unit, 0)
	currentRunes := 0

	for _, u := range units {
		if len(current) > 0 {
			last := current[len(current)-1]
			fits := currentRunes+separatorRunes(last, u)+u.runes <= limit
			paragraphWouldSpill := u.first && currentRunes+paragraphSeparatorRunes+u.paragraphRunes > limit

			if !fits || (paragraphWouldSpill && currentRunes >= fill) {
				texts = append(texts, render(current))

				current = c.overlap(current)
				currentRunes = measure(current)

				if len(current) > 0 && currentRunes+separatorRunes(current[len(current)-1], u)+u.runes > limit {
					current = current[:0]
					currentRunes = 0
				}
			}
		}

		if len(current) > 0 {
			currentRunes += separatorRunes(current[len(current)-1], u)
		}

		current = append(current, u)
		currentRunes += u.runes
	}

	if len(current) > 0 {
		texts = append(texts, render(current))
	}

	return texts
}

func (c *ParagraphChunker) overlap(closed []unit) []unit {
	if c.settings.OverlapRunes == 0 || len(closed) < 2 {
		return make([]unit, 0)
	}

	start := len(closed)
	total := 0

	for i := len(closed) - 1; i >= 1; i-- {
		cost := closed[i].runes
		if i < len(closed)-1 {
			cost += separatorRunes(closed[i], closed[i+1])
		}

		if total+cost > c.settings.OverlapRunes {
			break
		}

		total += cost
		start = i
	}

	tail := make([]unit, len(closed)-start)
	copy(tail, closed[start:])

	return tail
}

func render(units []unit) string {
	var b strings.Builder

	for i, u := range units {
		if i > 0 {
			b.WriteString(separator(units[i-1], u))
		}

		b.WriteString(u.text)
	}

	return b.String()
}

func measure(units []unit) int {
	total := 0

	for i, u := range units {
		if i > 0 {
			total += separatorRunes(units[i-1], u)
		}

		total += u.runes
	}

	return total
}

func separator(prev, next unit) string {
	if prev.paragraph == next.paragraph {
		return next.lead
	}

	return paragraphSeparator
}

func separatorRunes(prev, next unit) int {
	if prev.paragraph == next.paragraph {
		return utf8.RuneCountInString(next.lead)
	}

	return paragraphSeparatorRunes
}

func contentRunes(paragraphs []string) int {
	total := 0
	for _, p := range paragraphs {
		total += utf8.RuneCountInString(p)
	}

	return total
}

func chunkID(url, embeddingText string) string {
	sum := sha256.Sum256([]byte(url + "\x00" + embeddingText))
	return hex.EncodeToString(sum[:])[:idHexLength]
}
