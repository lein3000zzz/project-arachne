package answer

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"web-crawler/internal/llm"
	"web-crawler/internal/retrieval"
)

type fakeSearch struct {
	results []retrieval.Result
	got     retrieval.Request
}

func (f *fakeSearch) Search(_ context.Context, req retrieval.Request) ([]retrieval.Result, error) {
	f.got = req
	return f.results, nil
}

type fakeChat struct {
	reply string
	got   llm.Request
	err   error
}

func (f *fakeChat) Model() string { return "fake" }

func (f *fakeChat) Stream(_ context.Context, req llm.Request, onDelta func(string) error) (llm.Response, error) {
	f.got = req

	for _, word := range strings.SplitAfter(f.reply, " ") {
		if onDelta != nil {
			if err := onDelta(word); err != nil {
				return llm.Response{}, err
			}
		}
	}

	return llm.Response{Text: f.reply, Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}}, f.err
}

func service(t *testing.T, search Searcher, chat llm.Client) *Service {
	t.Helper()

	s, err := New(search, chat, Config{Sources: 3, MaxTokens: 256})
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func TestAnswerGroundsTheModelInNumberedSources(t *testing.T) {
	search := &fakeSearch{results: []retrieval.Result{
		{URL: "https://go.dev/doc/effective_go", Title: "Effective Go", Snippet: "Do not communicate by sharing memory."},
		{URL: "https://go.dev/empty", Title: "Empty", Snippet: "  "},
		{URL: "https://go.dev/ref/spec", Title: "Spec", Snippet: "Channels are typed conduits."},
	}}
	chat := &fakeChat{reply: "Goroutines communicate over channels [2], sharing memory by communicating [1][2]. See also [9]."}

	var (
		streamed []string
		sources  []Source
	)

	res, err := service(t, search, chat).Answer(context.Background(), Request{Query: " how do goroutines talk? ", Host: "go.dev", Mode: retrieval.ModeHybrid}, Events{
		Sources: func(s []Source) error { sources = s; return nil },
		Delta:   func(d string) error { streamed = append(streamed, d); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	if search.got.Limit != 3 || search.got.Host != "go.dev" {
		t.Errorf("search request = %+v", search.got)
	}

	if len(sources) != 2 || sources[0].N != 1 || sources[1].N != 2 || sources[1].URL != "https://go.dev/ref/spec" {
		t.Fatalf("sources = %+v: a result without text is no source, and numbering stays dense", sources)
	}

	prompt := chat.got.Messages[0].Content
	for _, want := range []string{"[1] Effective Go\nhttps://go.dev/doc/effective_go\nDo not communicate", "[2] Spec", "Question: how do goroutines talk?"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}

	if !strings.Contains(chat.got.System, "untrusted") || chat.got.MaxTokens != 256 {
		t.Errorf("system prompt / max tokens = %q / %d", chat.got.System, chat.got.MaxTokens)
	}

	if strings.Join(streamed, "") != chat.reply || res.Answer != chat.reply {
		t.Errorf("streamed %q, answer %q", strings.Join(streamed, ""), res.Answer)
	}

	if !slices.Equal(res.Cited, []int{2, 1}) {
		t.Errorf("cited = %v: in order of use, without the invented [9]", res.Cited)
	}
}

func TestAnswerDoesNotAskTheModelWithoutSources(t *testing.T) {
	chat := &fakeChat{reply: "made up"}

	res, err := service(t, &fakeSearch{}, chat).Answer(context.Background(), Request{Query: "q", Mode: retrieval.ModeHybrid}, Events{})
	if !errors.Is(err, ErrNoSources) || res.Answer != "" || chat.got.System != "" {
		t.Errorf("err %v answer %q: with nothing found the model must not be asked", err, res.Answer)
	}
}

func TestAnswerKeepsWhatWasGeneratedOnFailure(t *testing.T) {
	search := &fakeSearch{results: []retrieval.Result{{URL: "u", Title: "T", Snippet: "text"}}}
	chat := &fakeChat{reply: "partial answer [1]", err: errors.New("stream cut")}

	res, err := service(t, search, chat).Answer(context.Background(), Request{Query: "q", Mode: retrieval.ModeHybrid}, Events{})
	if err == nil || res.Answer != "partial answer [1]" || !slices.Equal(res.Cited, []int{1}) {
		t.Errorf("err %v result %+v", err, res)
	}
}

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
