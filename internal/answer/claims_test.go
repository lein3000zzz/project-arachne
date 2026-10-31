package answer

import (
	"slices"
	"strings"
	"testing"
)

func TestAnAnswerSplitsIntoClaims(t *testing.T) {
	answer := "Sun Tzu says that knowing the enemy and yourself wins a hundred battles [1]. " +
		"Residual nets, e.g. ResNet-50, train well. [2][3] K. He wrote that paper [2].\n\n" +
		"1. Deeper plain networks train worse [2, 3]\n" +
		"2. Residual blocks fix it [3]\n" +
		"The sources say nothing about speed.\n" +
		"See [1]. A made-up source backs this claim [9]."

	parts, list := Split(answer, 3)

	var joined strings.Builder
	for _, p := range parts {
		joined.WriteString(p.Text)
	}

	if joined.String() != answer {
		t.Fatalf("the parts are not the answer:\n%q", joined.String())
	}

	want := []struct {
		text  string
		cites []int
	}{
		{"Sun Tzu says that knowing the enemy and yourself wins a hundred battles.", []int{1}},
		{"Residual nets, e.g. ResNet-50, train well.", []int{2, 3}},
		{"K. He wrote that paper.", []int{2}},
		{"1. Deeper plain networks train worse", []int{2, 3}},
		{"2. Residual blocks fix it", []int{3}},
		{"A made-up source backs this claim.", nil},
	}

	if len(list) != len(want) {
		t.Fatalf("got %d claims, want %d: %+v", len(list), len(want), list)
	}

	for i, w := range want {
		if list[i].Text != w.text || !slices.Equal(list[i].Cites, w.cites) {
			t.Errorf("claim %d = %q %v, want %q %v", i, list[i].Text, list[i].Cites, w.text, w.cites)
		}
	}

	if last := list[5]; last.Verdict != Unsupported || last.Note == "" {
		t.Errorf("a claim citing only a source the model was not given: %+v", last)
	}

	claimed := 0
	for _, p := range parts {
		if p.Claim >= 0 {
			if !strings.Contains(p.Text, "[") {
				t.Errorf("part %q is claim %d but cites nothing", p.Text, p.Claim)
			}
			claimed++
		}
	}

	if claimed != len(list) {
		t.Errorf("%d parts point to claims, %d claims", claimed, len(list))
	}
}

func TestRussianSentencesSplitToo(t *testing.T) {
	_, list := Split("Остаточные связи решают проблему деградации [1]. Это показано на ImageNet и CIFAR, т.е. на двух наборах [2].", 2)

	if len(list) != 2 || list[1].Text != "Это показано на ImageNet и CIFAR, т.е. на двух наборах." {
		t.Errorf("claims = %+v", list)
	}
}

func TestCodeIsNeverAClaim(t *testing.T) {
	answer := "Index a slice with brackets [1].\n```go\nx := a[1] // the second element [2]\n```\nOr use `s[2]` to take another one [2]."

	parts, list := Split(answer, 2)
	if len(list) != 2 || list[1].Text != "Or use `s[2]` to take another one." || !slices.Equal(list[1].Cites, []int{2}) {
		t.Fatalf("claims = %+v", list)
	}

	for _, p := range parts {
		if strings.Contains(p.Text, "x := a[1]") && p.Claim >= 0 {
			t.Errorf("a code block became claim %d", p.Claim)
		}
	}
}
