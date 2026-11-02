package markdown

import (
	"strings"
	"testing"
	"web-crawler/internal/answer"
)

const text = "## Memory\n\n" +
	"- **Rust** checks borrows at compile time [1].\n" +
	"- Zig leaves it to the programmer [2][9].\n\n" +
	"```zig\nconst x = a[1];\n```\n\n" +
	"<img src=x onerror=alert(1)> [click](javascript:alert(1))"

func TestAnAnswerRendersWithPendingMarks(t *testing.T) {
	claims := []answer.Claim{{Text: "a"}, {Text: "b", Verdict: answer.Unsupported}}

	out := Render(text, Options{CiteHref: "#src-%d", Sources: 2, Claims: claims})

	for _, want := range []string{
		"<h2>Memory</h2>", "<strong>Rust</strong>", `<a href="#src-1"`, `<a href="#src-2"`, `[?]`,
		`<button type="button" class="mark pending" data-claim="0-0" aria-label="pending">·</button>`,
		`<button type="button" class="mark unsupported" data-claim="0-1" aria-label="unsupported">✗</button>`,
		`<pre class="chroma">`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}

	for _, unwanted := range []string{"<img", "onerror", "javascript:", "#src-9", markOpen} {
		if strings.Contains(out, unwanted) {
			t.Errorf("rendered %q:\n%s", unwanted, out)
		}
	}
}

func TestMarksNeedTheirClaims(t *testing.T) {
	if out := Render(text, Options{CiteHref: "#src-%d", Sources: 2}); strings.Contains(out, `class="mark`) {
		t.Errorf("no claims, no marks:\n%s", out)
	}

	if out := Render(text, Options{CiteHref: "#src-%d", Sources: 2, Claims: []answer.Claim{{}}}); strings.Contains(out, `class="mark`) {
		t.Errorf("claims that do not match the text, no marks:\n%s", out)
	}

	// Mid-stream: half a sentence, an unclosed fence - rendered, not broken.
	if out := Render("Rust **checks bor", Options{CiteHref: "#src-%d", Sources: 1}); !strings.Contains(out, "Rust") {
		t.Errorf("partial text: %s", out)
	}
}

func TestEveryClaimGetsItsMarkTablesIncluded(t *testing.T) {
	text := "Zig and Rust differ [1].\n\n| | Zig | Rust |\n|---|---|---|\n| 1.0 | not yet [2] | 2015 [2] |\n| Packages | few [1] | many [1] |\n"

	_, claims := answer.Split(text, 2)
	for i := range claims {
		claims[i].Verdict = answer.Supported
	}

	out := Render(text, Options{CiteHref: "#src-%d", Sources: 2, Claims: claims})

	if got := strings.Count(out, `class="mark`); got != len(claims) || len(claims) != 3 {
		t.Errorf("%d marks for %d claims:\n%s", got, len(claims), out)
	}

	if !strings.Contains(out, "<table>") || strings.Count(out, "<td>") != 6 {
		t.Errorf("the table must keep its cells:\n%s", out)
	}
}
