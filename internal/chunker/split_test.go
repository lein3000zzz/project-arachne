package chunker

import (
	"reflect"
	"testing"
)

func TestSplitSentences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"two sentences", "First one. Second one.", []string{"First one.", "Second one."}},
		{"question and exclamation", "Really? Yes! Done.", []string{"Really?", "Yes!", "Done."}},
		{"decimal is not a boundary", "Pi is 3.14 roughly.", []string{"Pi is 3.14 roughly."}},
		{"closing quote stays attached", `He said "stop." Then left.`, []string{`He said "stop."`, "Then left."}},
		{"ellipsis run", "Wait... Now go.", []string{"Wait...", "Now go."}},
		{"unicode ellipsis", "Подождите… Теперь идите.", []string{"Подождите…", "Теперь идите."}},
		{"cyrillic with guillemets", "Он сказал «стоп.» Потом ушёл.", []string{"Он сказал «стоп.»", "Потом ушёл."}},
		{"no terminator", "a fragment without an ending", []string{"a fragment without an ending"}},
		{"empty", "", []string{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := splitSentences(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitSentences(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSplitParagraphs(t *testing.T) {
	got := splitParagraphs("  one  \n\n\n two a\n two b \n\n\t\nthree")
	want := []string{"one", "two a\ntwo b", "three"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("splitParagraphs = %q, want %q", got, want)
	}
}
