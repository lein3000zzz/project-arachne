package answer

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// excerpt cuts text to at most limit runes, at the last sentence or line end in
// its final third if there is one, else at a word boundary.
func excerpt(text string, limit int) string {
	text = strings.TrimSpace(text)

	runes := []rune(text)
	if limit <= 0 || len(runes) <= limit {
		return text
	}

	cut := string(runes[:limit])
	floor := len(string(runes[:limit*2/3]))

	if i := max(strings.LastIndex(cut, ". "), strings.LastIndex(cut, "\n")); i >= floor {
		return strings.TrimSpace(cut[:i+1]) + " …"
	}

	if i := strings.LastIndexAny(cut, " \t"); i > 0 {
		return strings.TrimSpace(cut[:i]) + " …"
	}

	return cut + "…"
}

// focus cuts text like excerpt, but from the sentence where the excerpt holds
// the most of the question's words. A chunk is about twice an excerpt, and what
// the search matched is as often in its second half as in its first.
func focus(text string, limit int, terms []string) string {
	text = strings.TrimSpace(text)
	if limit <= 0 || utf8.RuneCountInString(text) <= limit || len(terms) == 0 {
		return excerpt(text, limit)
	}

	best := excerpt(text, limit)
	most := hits(best, terms)

	for _, start := range sentences(text) {
		cut := excerpt(text[start:], limit)
		if n := hits(cut, terms); n > most {
			best, most = "… "+cut, n
		}
	}

	return best
}

// sentences lists where each sentence or line after the first begins.
func sentences(text string) []int {
	var out []int

	for i := 0; i < len(text)-1; i++ {
		if text[i] != '\n' && (!strings.ContainsRune(".!?", rune(text[i])) || text[i+1] != ' ') {
			continue
		}

		j := i + 1
		for j < len(text) && strings.ContainsRune(" \t\n", rune(text[j])) {
			j++
		}

		if j < len(text) {
			out = append(out, j)
		}

		i = j - 1
	}

	return out
}

// terms are the question's words that can tell one part of a text from
// another.
func terms(query string) []string {
	var out []string

	for _, w := range words(query) {
		if !stopWords[w] && !slices.Contains(out, w) {
			out = append(out, w)
		}
	}

	return out
}

func words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// hits counts the terms text holds, each once.
func hits(text string, terms []string) int {
	in := words(text)
	n := 0

	for _, t := range terms {
		if slices.ContainsFunc(in, func(w string) bool { return alike(t, w) }) {
			n++
		}
	}

	return n
}

// alike: words sharing the first two thirds of the shorter one - "enemy" and
// "enemies", "враг" and "врагу" - roughly what a stemmer finds, without one.
// Words of one or two letters are too short to share anything but themselves.
func alike(a, b string) bool {
	ra, rb := []rune(a), []rune(b)

	shorter := min(len(ra), len(rb))
	if shorter < 3 {
		return a == b
	}

	need := max(3, (2*shorter+2)/3)

	common := 0
	for common < shorter && ra[common] == rb[common] {
		common++
	}

	return common >= need
}

var stopWords = func() map[string]bool {
	m := map[string]bool{}

	for _, w := range strings.Fields(`
		a an the and or but nor of to in on at by for with from about into onto over under
		as is are was were be been being am do does did have has had
		what which who whom whose why how when where whether that this these those
		it its there their they them i me my we our us you your he him his she her
		not no if than then so such can could should would will shall may might must
		also just very more most some any all each other please tell explain describe
		и а но или да не ни что чем чём чего как так также такой какой какая какое какие каков
		кто кого где куда когда почему зачем ли же бы в во на с со по к ко о об обо от до из за
		для у при про через между над под это этот эта эти то тот та те он она оно они его её ее
		их мы вы я ты мне нас вас нам вам есть был была было были быть будет можно нужно ещё еще
		уже очень расскажи объясни опиши`) {
		m[w] = true
	}

	return m
}()
