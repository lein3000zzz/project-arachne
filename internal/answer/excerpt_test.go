package answer

import (
	"slices"
	"strings"
	"testing"
)

func TestExcerpt(t *testing.T) {
	cases := []struct {
		text  string
		limit int
		want  string
	}{
		{"short text", 100, "short text"},
		// A sentence end in the last third of the limit is where the cut goes...
		{"The first sentence is long enough. Then a second one runs past the limit.", 45, "The first sentence is long enough. …"},
		{"Строка первая и длинная.\nСтрока вторая и ещё немного текста", 30, "Строка первая и длинная. …"},
		// ...an earlier one would throw most of the excerpt away, so a word boundary wins.
		{"Short. Then a much longer sentence that runs well past the limit.", 40, "Short. Then a much longer sentence that …"},
		{"no sentence end in this long run of words at all", 20, "no sentence end in …"},
		{"anything", 0, "anything"},
	}

	for _, tc := range cases {
		got := excerpt(tc.text, tc.limit)
		if got != tc.want {
			t.Errorf("excerpt(%q, %d) = %q, want %q", tc.text, tc.limit, got, tc.want)
		}

		if tc.limit > 0 && len([]rune(got)) > tc.limit+2 {
			t.Errorf("excerpt(%q, %d) is %d runes long", tc.text, tc.limit, len([]rune(got)))
		}
	}
}

func TestFocusKeepsThePartTheQuestionIsAbout(t *testing.T) {
	chunk := "(5) He will win who has military capacity and is not interfered with by the sovereign.\n\n" +
		"[Tu Yu quotes Wang Tzŭ as saying that it is the sovereign's function to give broad instructions.]\n\n" +
		"Victory lies in the knowledge of these five points.\n\n" +
		"18. Hence the saying: If you know the enemy and know yourself, you need not fear the result of a hundred battles."
	asked := terms("What does Sun Tzu say about knowing the enemy?")

	got := focus(chunk, 150, asked)
	if !strings.HasPrefix(got, "… ") || !strings.Contains(got, "If you know the enemy") || len([]rune(got)) > 150+4 {
		t.Errorf("focus = %q", got)
	}

	// Nothing of the question in the text, or a tie: the beginning, as before.
	if got := focus(chunk, 150, terms("borrow checker")); got != excerpt(chunk, 150) {
		t.Errorf("unrelated question moved the excerpt: %q", got)
	}

	if got := focus(chunk, 150, terms("sovereign")); got != excerpt(chunk, 150) {
		t.Errorf("the first match should win a tie: %q", got)
	}

	ru := "Глава первая о погоде и о дорогах, которые размыты дождями всю осень. " +
		"Глава вторая о том, что нужно знать врага: кто знает врагов и себя, тот не проиграет."
	if got := focus(ru, 80, terms("Что говорится о враге?")); !strings.Contains(got, "знать врага") {
		t.Errorf("focus = %q", got)
	}
}

func TestTerms(t *testing.T) {
	if got := terms("What is the borrow checker, and what does the borrow checker do?"); !slices.Equal(got, []string{"borrow", "checker"}) {
		t.Errorf("terms = %q", got)
	}

	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"enemy", "enemies", true}, {"враг", "врагу", true}, {"lifetimes", "lifetime", true},
		{"construct", "consider", false}, {"war", "was", false}, {"go", "go", true}, {"go", "good", false}, {"war", "wa", false},
	} {
		if alike(tc.a, tc.b) != tc.want {
			t.Errorf("alike(%q, %q) = %v", tc.a, tc.b, !tc.want)
		}
	}
}
