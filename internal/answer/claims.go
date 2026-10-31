package answer

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
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

var (
	// "[1]", "[2][3]", "[1, 3]".
	citeGroup = regexp.MustCompile(`\[(\d{1,3}(?:\s*,\s*\d{1,3})*)\]`)
	// What a citation taken out leaves: "battles ." for "battles [1].".
	spaceBefore = regexp.MustCompile(`\s+([.,;:!?…])`)
)

// Words a full stop follows without ending the sentence.
var abbreviations = map[string]bool{
	"e.g": true, "i.e": true, "etc": true, "vs": true, "cf": true, "al": true, "fig": true, "eq": true, "no": true,
	"т.е": true, "т.д": true, "т.п": true, "др": true, "см": true, "рис": true, "г": true,
}

const minClaimWords = 3

// claims splits the answer into parts, and the sentences among them that cite
// any of the sources into claims. A citation after the full stop - "battles.
// [1]" - belongs to the sentence before it.
func claims(answer string, sources int) ([]Part, []Claim) {
	var (
		parts []Part
		out   []Claim
	)

	for _, sentence := range splitSentences(answer) {
		part := Part{Text: sentence, Claim: -1}

		if cites := citesIn(sentence); len(cites) > 0 {
			text := spaceBefore.ReplaceAllString(strings.Join(strings.Fields(citeGroup.ReplaceAllString(sentence, "")), " "), "$1")
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

	return parts, out
}

func citesIn(sentence string) []int {
	var out []int

	for _, m := range citeGroup.FindAllStringSubmatch(sentence, -1) {
		for _, raw := range strings.Split(m[1], ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}

	return out
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

		loc := citeGroup.FindStringIndex(text[j:])
		if loc == nil || loc[0] != 0 {
			return i
		}

		i = j + loc[1]
	}
}
