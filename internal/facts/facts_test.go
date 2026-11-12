package facts

import (
	"context"
	"strings"
	"testing"
	"web-crawler/internal/llm"
)

func TestAQuoteIsFoundAsThePageHasIt(t *testing.T) {
	passage := "Where chiefs are not in the constitution,\n they compete with the state. Миграция  населения — «процесс перемещения людей». В 2016 году."

	for _, c := range []struct{ quote, want string }{
		{"they compete with the state.", "they compete with the state."},
		{"Where chiefs are NOT in the constitution, they compete", "Where chiefs are not in the constitution,\n they compete"},
		{"миграция населения процесс перемещения людей", "Миграция  населения — «процесс перемещения людей"},
		{"«процесс перемещения людей»", "«процесс перемещения людей»"},
	} {
		if got, ok := Locate(c.quote, passage); !ok || got != c.want {
			t.Errorf("%q: %q, %v; want %q", c.quote, got, ok, c.want)
		}
	}

	for _, quote := range []string{
		"chiefs compete with the state",       // a paraphrase
		"Where chiefs are ... with the state", // shortened in the middle
		"В 2016",                              // too short to tell
		"the people move from place to place",
		"",
	} {
		if got, ok := Locate(quote, passage); ok {
			t.Errorf("%q found as %q", quote, got)
		}
	}
}

type model struct {
	reply string
	asked *string
}

func (m model) Stream(_ context.Context, req llm.Request, _ func(string) error) (llm.Response, error) {
	if m.asked != nil {
		*m.asked = req.Messages[0].Content
	}

	return llm.Response{Text: m.reply}, nil
}

func (model) Model() string { return "small" }

func TestAPassageSplitsIntoSentencesAFactCanRestOn(t *testing.T) {
	got := Sentences("По данным, приводимым Т.Н. Юдиной, в 1846–1915 гг. прибыло 42 млн. человек. В 1882 г. доля выходцев из Китая составила половину!  Henn uses distance; chiefs compete. " +
		"Short. Then a much longer closing sentence follows here.")
	want := []string{
		"По данным, приводимым Т.Н. Юдиной, в 1846–1915 гг. прибыло 42 млн. человек.",
		"В 1882 г. доля выходцев из Китая составила половину!",
		"Henn uses distance; chiefs compete.",
		"Short. Then a much longer closing sentence follows here.",
	}

	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("\n got %q\nwant %q", got, want)
	}

	if got := Sentences("Среди мореплавателей, сыгравших ключевую роль в географи- ческих открытиях, место принадлежит Колумбу. Москва - столица."); got[0] != "Среди мореплавателей, сыгравших ключевую роль в географических открытиях, место принадлежит Колумбу." || !strings.Contains(strings.Join(got, " "), "Москва - столица") {
		t.Errorf("line-break hyphens: %q", got)
	}

	long := strings.Repeat("word ", 60) + "and, " + strings.Repeat("more ", 60) + "end."
	for _, s := range Sentences(long) {
		if len([]rune(s)) > maxSentenceRunes {
			t.Errorf("a sentence of %d runes", len([]rune(s)))
		}
	}
}

func TestOnlyFactsTheirSentencesBearOutAreKept(t *testing.T) {
	passage := "Henn uses distance to the capital as a measure of state capacity. Where chiefs are not in the constitution, they compete with the state. " +
		"In 1882 half of the immigrants came from China."
	reply := "Here you go:\n```json\n" + `{"facts":[
		{"claim":"Henn measures state capacity by the distance to the capital.","about":"state capacity","sentences":[1]},
		{"claim":"  Chiefs outside the constitution\n compete with the state. ","about":"chiefs","sentences":[2, 2, 9]},
		{"claim":"In 1885 half of the immigrants came from China.","about":"immigration","sentences":[3]},
		{"claim":"In 1882 half of the immigrants came from China.","about":"immigration","sentences":[3]},
		{"claim":"Bananas are yellow and grow in tropical countries.","about":"bananas","sentences":[1]},
		{"claim":"Something.","about":"x","sentences":[7]},
		{"claim":"Henn measures state capacity by the distance to the capital.","about":"again","sentences":[1]}]}` + "\n```"

	res, err := NewExtractor(model{reply: reply}).Extract(context.Background(), "Henn", passage)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Facts) != 3 || len(res.Rejected) != 3 || res.Model != "small" {
		t.Fatalf("%+v", res)
	}

	f := res.Facts[1]
	if f.Claim != "Chiefs outside the constitution compete with the state." || f.Quote != "Where chiefs are not in the constitution, they compete with the state." ||
		len(f.Sentences) != 1 {
		t.Errorf("%+v", f)
	}

	if !strings.Contains(res.Rejected[0].Why, "1885") {
		t.Errorf("a number its sentences lack: %+v", res.Rejected[0])
	}
}

func TestARussianPassageIsAskedForInRussian(t *testing.T) {
	var asked string
	passage := "В конце XIX века в Северную Америку устремились выходцы из Германии, Скандинавии и Польши."
	reply := `{"facts":[{"claim":"In the late 19th century people from Germany went to North America.","about":"immigration","sentences":[1]},
		{"claim":"В конце XIX века в Северную Америку устремились выходцы из Германии и Польши.","about":"иммиграция","sentences":[1]}]}`

	res, err := NewExtractor(model{reply: reply, asked: &asked}).Extract(context.Background(), "Миграции", "Миграции\n\n"+passage)
	if err != nil || !strings.Contains(asked, "in Russian") || !strings.Contains(asked, "[1] В конце") || strings.Contains(asked, "[1] Миграции") {
		t.Fatalf("%v; asked %q", err, asked)
	}

	if len(res.Facts) != 1 || !strings.HasPrefix(res.Facts[0].Claim, "В конце") || len(res.Rejected) != 1 {
		t.Errorf("an English claim for a Russian passage is not borne out by it: %+v", res)
	}
}

func TestAReplyCutShortKeepsTheFactsItFinished(t *testing.T) {
	passage := "One two three four five six seven. Eight nine ten eleven twelve thirteen."
	reply := `{"facts":[{"claim":"One two three four five.","about":"a","sentences":[1]},{"claim":"Eleven twelve thirteen.","about":"b","sentences":[2]},{"claim":"C.","sen`

	res, err := NewExtractor(model{reply: reply}).Extract(context.Background(), "", passage)
	if err != nil || len(res.Facts) != 2 {
		t.Fatalf("%+v, %v", res, err)
	}

	if _, err := NewExtractor(model{reply: "I cannot help with that."}).Extract(context.Background(), "", passage); err == nil {
		t.Error("prose taken for facts")
	}
}
