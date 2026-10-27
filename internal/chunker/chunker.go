package chunker

import "web-crawler/internal/documents"

type Chunk struct {
	ID      string
	URL     string
	Title   string
	Ordinal int
	Text    string
}

func (c Chunk) EmbeddingText() string {
	if c.Title == "" {
		return c.Text
	}

	return c.Title + "\n\n" + c.Text
}

type Chunker interface {
	Chunk(doc *documents.Document) []Chunk
	Fingerprint() string
}
