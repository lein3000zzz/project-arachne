package facts

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// A sentence past this is split at its next pause: a claim should rest
	// on words that say it, not on a paragraph that also does.
	maxSentenceRunes = 320
	// Fragments shorter than this join the sentence before them: "Т.Н." of
	// "Т.Н. Юдиной" is not a sentence.
	minSentenceRunes = 25
)

// Sentences splits a passage into the units a fact may rest on. It errs
// toward splitting: a claim may cite several in a row, but cannot cite half
// of one.
func Sentences(passage string) []string {
	text := Normalize(passage)

	var out []string

	start := 0
	for i, r := range text {
		if !strings.ContainsRune(".!?…;", r) {
			continue
		}

		next := i + utf8.RuneLen(r)
		if next >= len(text) || text[next] != ' ' {
			continue
		}

		// An initial is not the end of a sentence: "Т.Н. Юдиной", "J. Smith".
		if r == '.' && initial(text[:i]) {
			continue
		}

		after, _ := utf8.DecodeRuneInString(text[next+1:])
		if !(unicode.IsUpper(after) || unicode.IsDigit(after) || strings.ContainsRune("«“\"(—–-", after)) && r != ';' {
			continue
		}

		out = append(out, text[start:next])
		start = next + 1
	}

	if start < len(text) {
		out = append(out, text[start:])
	}

	return tidy(out)
}

func tidy(parts []string) []string {
	var out []string

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}

		if len(out) > 0 && utf8.RuneCountInString(out[len(out)-1]) < minSentenceRunes {
			out[len(out)-1] += " " + p
			continue
		}

		for utf8.RuneCountInString(p) > maxSentenceRunes {
			cut := pause(p)
			out = append(out, strings.TrimSpace(p[:cut]))
			p = strings.TrimSpace(p[cut:])
		}

		out = append(out, p)
	}

	return out
}

// pause is where a long sentence breaks: the last comma or dash before the
// limit, or the last space when it has none.
func pause(s string) int {
	limit := len(string([]rune(s)[:maxSentenceRunes]))

	for _, sep := range []string{", ", " — ", " – ", " "} {
		if i := strings.LastIndex(s[:limit], sep); i > limit/3 {
			return i + len(sep)
		}
	}

	return limit
}

// initial says whether the text ends in a lone letter, as "Т.Н" or "J" do.
func initial(text string) bool {
	last, size := utf8.DecodeLastRuneInString(text)
	if size == 0 || !unicode.IsLetter(last) {
		return false
	}

	before, _ := utf8.DecodeLastRuneInString(text[:len(text)-size])

	return !unicode.IsLetter(before)
}

// A word a PDF broke at the end of a line: "географи- ческих".
var lineBreakHyphen = regexp.MustCompile(`(\p{L})- (\p{Ll})`)

// Normalize is a passage's text as facts quote it: white space collapsed and
// words broken across lines joined again. Quotes are looked for in a later
// version of a page in this form too.
func Normalize(text string) string {
	return lineBreakHyphen.ReplaceAllString(strings.Join(strings.Fields(text), " "), "$1$2")
}
