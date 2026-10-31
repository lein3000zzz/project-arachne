package cite

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

const text = "Channels pass values [1]. Use `ch[0]` sparingly [2, 3].\n\n" +
	"```go\nx := a[1]\n```\n\n" +
	"~~~\nunclosed [4] code"

func TestSegmentsKeepEveryByte(t *testing.T) {
	segs := Segments(text)

	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.Text)
	}

	if b.String() != text {
		t.Fatalf("joined segments differ:\n%q", b.String())
	}

	var kinds []bool
	for _, s := range segs {
		kinds = append(kinds, s.Code)
	}

	if fmt.Sprint(kinds) != "[false true false true]" {
		t.Errorf("segments %v: %+v", kinds, segs)
	}
}

func TestCitationsAreFoundInProseOnly(t *testing.T) {
	if got := In(text); !slices.Equal(got, []int{1, 2, 3}) {
		t.Errorf("In = %v; code and inline code are not citations", got)
	}

	renumbered := Replace(text, func(ns []int) string { return fmt.Sprint(ns) })
	if !strings.Contains(renumbered, "values [1].") || !strings.Contains(renumbered, "`ch[0]` sparingly [2 3].") ||
		!strings.Contains(renumbered, "x := a[1]") || !strings.Contains(renumbered, "unclosed [4] code") {
		t.Errorf("replaced:\n%s", renumbered)
	}

	if got := Strip("Fast [1][2], safe [3]."); got != "Fast , safe ." {
		t.Errorf("Strip = %q", got)
	}

	if Leading("[12] rest") != 4 || Leading(" [1]") != 0 || Leading("x[1]") != 0 {
		t.Error("Leading")
	}
}
