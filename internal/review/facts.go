package review

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Facts are what code can tell about a page without a model. They go to the
// model as hints and are stored with the verdict.
type Facts struct {
	Runes int `json:"runes"`
	Parts int `json:"parts"`
	// Share of non-empty lines of at most shortLineRunes: menus and link lists
	// are mostly such lines.
	ShortLines float64 `json:"short_lines"`
	// Share of non-empty lines that occur more than once.
	RepeatedLines float64 `json:"repeated_lines"`
	// Phrases addressed to AI models. They inform, they do not decide: a page
	// about prompt injection quotes them too.
	Addressed []string `json:"addressed,omitempty"`
}

const shortLineRunes = 40

var addressedToModels = regexp.MustCompile(`(?i)` + strings.Join([]string{
	`ignore\s+(all\s+|any\s+)?(the\s+)?(previous|prior|above|earlier)\s+(instructions|prompts?|messages)`,
	`disregard\s+(all\s+|any\s+)?(the\s+)?(previous|prior|above)`,
	`as\s+an\s+ai(\s+language)?\s+model`,
	`(reveal|print|show)\s+(your|the)\s+system\s+prompt`,
	`you\s+are\s+now\s+(an?\s+)?(ai|assistant|chatbot|dan)\b`,
	`(ai|llm|assistant|language model)s?\s*:\s*(always|never|you must)`,
	`do\s+not\s+(tell|inform)\s+the\s+user`,
	`<\|im_start\|>|\[/?INST\]`,
	`игнорируй(те)?\s+(все\s+)?(предыдущие|прошлые)\s+инструкции`,
}, "|"))

func factsOf(text string, parts int) Facts {
	f := Facts{Runes: utf8.RuneCountInString(text), Parts: parts}

	counts := map[string]int{}

	var lines, short int

	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		lines++
		counts[line]++

		if utf8.RuneCountInString(line) <= shortLineRunes {
			short++
		}
	}

	if lines > 0 {
		repeated := 0
		for _, n := range counts {
			if n > 1 {
				repeated += n
			}
		}

		f.ShortLines = float64(short) / float64(lines)
		f.RepeatedLines = float64(repeated) / float64(lines)
	}

	seen := map[string]bool{}
	for _, m := range addressedToModels.FindAllString(text, 5) {
		m = strings.Join(strings.Fields(strings.ToLower(m)), " ")
		if !seen[m] {
			seen[m] = true
			f.Addressed = append(f.Addressed, m)
		}
	}

	return f
}

func (f Facts) describe() string {
	addressed := "none"
	if len(f.Addressed) > 0 {
		addressed = `"` + strings.Join(f.Addressed, `", "`) + `"`
	}

	return fmt.Sprintf("Length: %d characters in %d parts.\nShort lines (%d characters or fewer): %.0f%% of lines.\n"+
		"Repeated lines: %.0f%% of lines.\nPhrases addressed to AI models: %s.",
		f.Runes, f.Parts, shortLineRunes, f.ShortLines*100, f.RepeatedLines*100, addressed)
}

// split cuts text into parts of at most limit runes at line ends, cutting a
// longer line where it must.
func split(text string, limit int) []string {
	var (
		parts []string
		cur   strings.Builder
		n     int
	)

	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			parts = append(parts, s)
		}

		cur.Reset()
		n = 0
	}

	for _, line := range strings.SplitAfter(text, "\n") {
		runes := []rune(line)

		for len(runes) > 0 {
			if n > 0 && n+len(runes) > limit {
				flush()
			}

			take := min(len(runes), limit)
			cur.WriteString(string(runes[:take]))
			n += take
			runes = runes[take:]

			if len(runes) > 0 {
				flush()
			}
		}
	}

	flush()

	return parts
}
