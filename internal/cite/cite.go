// Package cite finds the source citations - [1], [2][3], [1, 3] - in text a
// model wrote, leaving code alone: in a code block, a[1] is an index.
package cite

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	group = regexp.MustCompile(`\[(\d{1,3}(?:\s*,\s*\d{1,3})*)\]`)
	// An inline code span, or a citation group outside one.
	spanOrGroup = regexp.MustCompile("`[^`\n]*`|" + group.String())
	fence       = regexp.MustCompile("^\\s{0,3}(```|~~~)")
	leading     = regexp.MustCompile("^" + group.String())
)

// Segment is a run of text: prose, or a fenced code block.
type Segment struct {
	Text string
	Code bool
}

// Segments cuts text into prose and fenced code blocks; joined, they are the
// text again. An unclosed fence runs to the end.
func Segments(text string) []Segment {
	var (
		out     []Segment
		current strings.Builder
		marker  string
	)

	flush := func(code bool) {
		if current.Len() > 0 {
			out = append(out, Segment{Text: current.String(), Code: code})
			current.Reset()
		}
	}

	for _, line := range strings.SplitAfter(text, "\n") {
		m := fence.FindStringSubmatch(line)

		switch {
		case marker == "" && m != nil:
			flush(false)
			marker = m[1]
			current.WriteString(line)
		case marker != "" && m != nil && m[1] == marker:
			current.WriteString(line)
			flush(true)
			marker = ""
		default:
			current.WriteString(line)
		}
	}

	flush(marker != "")

	return out
}

// Replace puts what fn returns in place of every citation group in the prose
// of text, inline code and code blocks aside.
func Replace(text string, fn func(numbers []int) string) string {
	var b strings.Builder

	for _, seg := range Segments(text) {
		if seg.Code {
			b.WriteString(seg.Text)
			continue
		}

		b.WriteString(spanOrGroup.ReplaceAllStringFunc(seg.Text, func(m string) string {
			if strings.HasPrefix(m, "`") {
				return m
			}

			return fn(numbers(m))
		}))
	}

	return b.String()
}

// In lists the numbers text cites, each once, in order.
func In(text string) []int {
	var out []int

	Replace(text, func(ns []int) string {
		for _, n := range ns {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}

		return ""
	})

	return out
}

// Strip takes the citation groups out of text's prose.
func Strip(text string) string {
	return Replace(text, func([]int) string { return "" })
}

// Leading is the length of the citation group text starts with, or 0.
func Leading(text string) int {
	if loc := leading.FindStringIndex(text); loc != nil {
		return loc[1]
	}

	return 0
}

func numbers(g string) []int {
	var out []int

	for raw := range strings.SplitSeq(strings.Trim(g, "[]"), ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			out = append(out, n)
		}
	}

	return out
}
