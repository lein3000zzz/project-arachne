package chunker

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

type piece struct {
	text string
	lead string
}

func splitParagraphs(content string) []string {
	paragraphs := make([]string, 0)
	lines := make([]string, 0)

	flush := func() {
		if len(lines) > 0 {
			paragraphs = append(paragraphs, strings.Join(lines, "\n"))
			lines = lines[:0]
		}
	}

	for _, line := range strings.Split(content, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			flush()
			continue
		}

		lines = append(lines, line)
	}

	flush()

	return paragraphs
}

// splitOversized breaks a paragraph that cannot fit in one chunk into pieces no
// longer than limit: by line, then by sentence, then by word, and finally by rune
// for single words longer than limit.
func splitOversized(paragraph string, limit int) []piece {
	out := make([]piece, 0)

	for li, line := range strings.Split(paragraph, "\n") {
		lineLead := "\n"
		if li == 0 {
			lineLead = ""
		}

		for si, sentence := range splitSentences(line) {
			sentenceLead := " "
			if si == 0 {
				sentenceLead = lineLead
			}

			if utf8.RuneCountInString(sentence) <= limit {
				out = append(out, piece{text: sentence, lead: sentenceLead})
				continue
			}

			for wi, word := range splitWords(sentence, limit) {
				if wi == 0 {
					word.lead = sentenceLead
				}

				out = append(out, word)
			}
		}
	}

	return out
}

func splitSentences(line string) []string {
	runes := []rune(line)
	out := make([]string, 0)
	start := 0

	for i := 0; i < len(runes); i++ {
		if !isTerminator(runes[i]) {
			continue
		}

		end := i + 1
		for end < len(runes) && (isTerminator(runes[end]) || isCloser(runes[end])) {
			end++
		}

		if end < len(runes) && unicode.IsSpace(runes[end]) {
			if sentence := strings.TrimSpace(string(runes[start:end])); sentence != "" {
				out = append(out, sentence)
			}

			start = end + 1
			i = end
		}
	}

	if start < len(runes) {
		if sentence := strings.TrimSpace(string(runes[start:])); sentence != "" {
			out = append(out, sentence)
		}
	}

	return out
}

func splitWords(sentence string, limit int) []piece {
	out := make([]piece, 0)

	var current strings.Builder

	currentRunes := 0

	flush := func() {
		if currentRunes > 0 {
			out = append(out, piece{text: current.String(), lead: " "})
			current.Reset()
			currentRunes = 0
		}
	}

	for _, word := range strings.Fields(sentence) {
		wordRunes := utf8.RuneCountInString(word)

		if wordRunes > limit {
			flush()

			runes := []rune(word)
			for start := 0; start < len(runes); start += limit {
				lead := " "
				if start > 0 {
					lead = ""
				}

				out = append(out, piece{text: string(runes[start:min(start+limit, len(runes))]), lead: lead})
			}

			continue
		}

		separator := 0
		if currentRunes > 0 {
			separator = 1
		}

		if currentRunes+separator+wordRunes > limit {
			flush()

			separator = 0
		}

		if separator == 1 {
			current.WriteByte(' ')
		}

		current.WriteString(word)
		currentRunes += separator + wordRunes
	}

	flush()

	return out
}

func isTerminator(r rune) bool {
	return r == '.' || r == '!' || r == '?' || r == '…'
}

func isCloser(r rune) bool {
	return strings.ContainsRune(`"')]}»”’`, r)
}
