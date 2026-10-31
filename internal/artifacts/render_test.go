package artifacts

import (
	"strings"
	"testing"
	"web-crawler/internal/answer"
)

func TestRenderLinksCitationsAndMarksClaims(t *testing.T) {
	v := Version{
		Sections: []Section{
			{
				Heading: "Indexing <slices>",
				State:   StateDone,
				Text: "Index a slice with brackets [1].\n\n```go\nx := a[1] // [2]\n```\n\n" +
					"It is **always** safe [2].\n\n<script>alert(1)</script> [Click](javascript:alert(2)) and [?](#x)",
				Claims: []answer.Claim{
					{Text: "Index a slice with brackets.", Cites: []int{1}, Verdict: answer.Supported},
					{Text: "It is always safe.", Cites: []int{2}, Verdict: answer.Unsupported},
				},
			},
			{Heading: "Hardware", Focus: `GPUs "and" <TPUs>`, State: StateThin, Note: "The lab holds too little on this."},
			{Heading: "Later", State: StateWriting},
		},
		Sources: []Source{
			{N: 1, URL: "https://go.dev/ref/spec", Title: "The Go <Spec>"},
			{N: 2, URL: "https://uploads.arachne.invalid/0123456789abcdef0123456789abcdef01234567", Title: "notes.md"},
		},
	}

	body, toc := Render(v)

	for _, want := range []string{
		`<h2>Indexing &lt;slices&gt;</h2>`,
		`<a href="#ref-1"`,
		`<pre class="chroma">`,
		`<button type="button" class="mark supported" data-claim="0-0" aria-label="supported">✓</button>`,
		`<button type="button" class="mark unsupported" data-claim="0-1" aria-label="unsupported">✗</button>`,
		`data-focus="GPUs &#34;and&#34; &lt;TPUs&gt;"`,
		`<p class="note pending">writing…</p>`,
		`<li id="ref-1"><a href="https://go.dev/ref/spec" target="_blank" rel="noopener noreferrer">The Go &lt;Spec&gt;</a> <span class="host">go.dev</span></li>`,
		`<a href="/uploads/0123456789abcdef0123456789abcdef01234567"`,
		`<span class="host">your file</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}

	for _, unwanted := range []string{"<script", "javascript:", `href="#ref-2">2</a></span>`, ""} {
		if strings.Contains(body, unwanted) {
			t.Errorf("rendered %q", unwanted)
		}
	}

	if code := body[strings.Index(body, `<pre class="chroma">`):]; strings.Contains(code[:strings.Index(code, "</pre>")], "#ref-") ||
		!strings.Contains(code, `<span class="c1">// [2]</span>`) {
		t.Errorf("a[1] and // [2] in code must stay code, never citations:\n%s", code)
	}

	if len(toc) != 4 || toc[0].ID != "s1" || toc[3].ID != "sources" {
		t.Errorf("toc = %+v", toc)
	}
}

func TestClaimsNoLongerMatchingTheTextAreNotMarked(t *testing.T) {
	body, _ := Render(Version{
		Sections: []Section{{State: StateDone, Text: "One claim here [1]. Another claim there [1].",
			Claims: []answer.Claim{{Verdict: answer.Supported}}}},
		Sources: []Source{{N: 1, URL: "https://example.com/"}},
	})

	if strings.Contains(body, `class="mark`) {
		t.Errorf("two claims, one verdict: no marks rather than wrong ones:\n%s", body)
	}
}

func TestMarkdownSaysWhatWasNotWritten(t *testing.T) {
	out := Markdown(Artifact{Kind: KindReport, Title: "Residual networks"}, Version{
		Sections: []Section{
			{Heading: "Blocks", State: StateDone, Text: "A block adds its input [1].",
				Claims: []answer.Claim{{Text: "A block adds its input.", Verdict: answer.Partly}}},
			{Heading: "Hardware", Focus: "GPUs", State: StateThin, Note: "The lab holds too little on this."},
		},
		Sources: []Source{{N: 1, URL: "https://en.wikipedia.org/wiki/Residual_(network)", Title: "Residual [network]"}},
	})

	for _, want := range []string{
		"# Residual networks\n", "## Blocks\n\nA block adds its input [1].",
		"> Not written: The lab holds too little on this. It was to cover: GPUs",
		`1. [Residual \[network\]](<https://en.wikipedia.org/wiki/Residual_(network)>)`,
		"- A block adds its input. (partly)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
