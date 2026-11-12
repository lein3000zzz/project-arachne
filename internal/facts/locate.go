package facts

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// minQuoteWords keeps a quote from matching by chance: "2016" or "the
// state" are somewhere in most passages.
const minQuoteWords = 3

type word struct {
	text       string
	start, end int
}

func words(s string) []word {
	var out []word

	start := -1
	for i, r := range s {
		in := unicode.IsLetter(r) || unicode.IsDigit(r)

		switch {
		case in && start < 0:
			start = i
		case !in && start >= 0:
			out = append(out, word{strings.ToLower(s[start:i]), start, i})
			start = -1
		}
	}

	if start >= 0 {
		out = append(out, word{strings.ToLower(s[start:]), start, len(s)})
	}

	return out
}

// Locate finds the quote in the passage word for word - case, punctuation
// and white space set aside, as a small model copies those loosely - and
// returns the passage's own text for it. A quote that is not there, or is
// too short to tell, is not found: whatever is stored is the page's words.
func Locate(quote, passage string) (string, bool) {
	q := words(quote)
	if len(q) < minQuoteWords {
		return "", false
	}

	p := words(passage)

outer:
	for i := 0; i+len(q) <= len(p); i++ {
		for j := range q {
			if p[i+j].text != q[j].text {
				continue outer
			}
		}

		start, end := p[i].start, p[i+len(q)-1].end
		trimmed := strings.TrimSpace(quote)

		// Marks the quote opened or closed with - brackets, quotation marks,
		// a full stop - stay with it when the page has them there too.
		if r, size := utf8.DecodeLastRuneInString(passage[:start]); size > 0 && strings.ContainsRune("(«“\"'", r) && strings.HasPrefix(trimmed, string(r)) {
			start -= size
		}

		if r, size := utf8.DecodeRuneInString(passage[end:]); size > 0 && strings.ContainsRune(".!?)»”\"'%", r) && strings.HasSuffix(trimmed, string(r)) {
			end += size
		}

		return passage[start:end], true
	}

	return "", false
}
