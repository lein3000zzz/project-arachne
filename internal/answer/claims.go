package answer

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
	"web-crawler/internal/cite"
)

// Claim is a sentence of the answer that cites sources.
type Claim struct {
	// Without its citation marks.
	Text  string `json:"text"`
	Cites []int  `json:"cites"`
	// Empty until checked.
	Verdict Verdict `json:"verdict,omitempty"`
	// Words of a cited excerpt that bear the claim out, found there verbatim.
	Quote string `json:"quote,omitempty"`
	// Why there is no verdict from the model, when there is not.
	Note string `json:"note,omitempty"`
}

// Part is a piece of the answer, in order: together the parts are the answer.
type Part struct {
	Text string `json:"text"`
	// The claim this part is, or -1.
	Claim int `json:"claim"`
}

// What a citation taken out leaves: "battles ." for "battles [1].".
var spaceBefore = regexp.MustCompile(`\s+([.,;:!?…])`)

// Words a full stop follows without ending the sentence.
var abbreviations = map[string]bool{
	"e.g": true, "i.e": true, "etc": true, "vs": true, "cf": true, "al": true, "fig": true, "eq": true, "no": true,
	"т.е": true, "т.д": true, "т.п": true, "др": true, "см": true, "рис": true, "г": true,
}

const minClaimWords = 3

// Split cuts the answer into parts, and the sentences among them that cite
// any of the sources into claims. A citation after the full stop - "battles.
// [1]" - belongs to the sentence before it; code is never a claim.
func Split(answer string, sources int) ([]Part, []Claim) {
	var (
		parts []Part
		out   []Claim
	)

	for _, seg := range cite.Segments(answer) {
		if seg.Code {
			parts = append(parts, Part{Text: seg.Text, Claim: -1})
			continue
		}

		for _, sentence := range splitSentences(seg.Text) {
			part := Part{Text: sentence, Claim: -1}

			if cites := cite.In(sentence); len(cites) > 0 {
				text := spaceBefore.ReplaceAllString(strings.Join(strings.Fields(cite.Strip(sentence)), " "), "$1")
				text = strings.TrimSpace(strings.TrimLeft(text, "-*• "))

				if len(strings.FieldsFunc(text, notWord)) >= minClaimWords {
					claim := Claim{Text: text}

					for _, n := range cites {
						if n >= 1 && n <= sources {
							claim.Cites = append(claim.Cites, n)
						}
					}

					if len(claim.Cites) == 0 {
						claim.Verdict, claim.Note = Unsupported, "cites a source it was not given"
					}

					part.Claim = len(out)
					out = append(out, claim)
				}
			}

			parts = append(parts, part)
		}
	}

	return parts, out
}

func notWord(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// splitSentences cuts text at sentence ends and line breaks, keeping every byte:
// joined, they are the text again.
func splitSentences(text string) []string {
	var out []string

	start := 0

	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		end := -1

		switch {
		case r == '\n':
			end = i + size
			for end < len(text) && text[end] == '\n' {
				end++
			}
		case strings.ContainsRune(".!?…", r) && !abbreviated(text[start:i]):
			j := skipCitations(text, skipClosers(text, i+size))
			if j == len(text) || text[j] == ' ' || text[j] == '\t' || text[j] == '\n' {
				for j < len(text) && (text[j] == ' ' || text[j] == '\t') {
					j++
				}

				end = j
			}
		}

		if end > 0 {
			out = append(out, text[start:end])
			start, i = end, end

			continue
		}

		i += size
	}

	if start < len(text) {
		out = append(out, text[start:])
	}

	return out
}

// abbreviated: the word before a full stop is an abbreviation, an initial
// ("K. He") or a list number ("1.").
func abbreviated(before string) bool {
	word := before[strings.LastIndexAny(before, " \t\n(")+1:]
	if utf8.RuneCountInString(word) == 1 {
		return true
	}

	return abbreviations[strings.ToLower(word)]
}

func skipClosers(text string, i int) int {
	for i < len(text) {
		r, size := utf8.DecodeRuneInString(text[i:])
		if !strings.ContainsRune(`)"'”’»`, r) {
			break
		}

		i += size
	}

	return i
}

func skipCitations(text string, i int) int {
	for {
		j := i
		for j < len(text) && text[j] == ' ' {
			j++
		}

		n := cite.Leading(text[j:])
		if n == 0 {
			return i
		}

		i = j + n
	}
}
