package labs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"web-crawler/internal/llm"
)

type scriptedChat struct {
	replies  []string
	err      error
	requests []llm.Request
}

func (c *scriptedChat) Model() string { return "small" }

func (c *scriptedChat) Stream(_ context.Context, req llm.Request, _ func(string) error) (llm.Response, error) {
	c.requests = append(c.requests, req)

	if c.err != nil {
		return llm.Response{}, c.err
	}

	reply := c.replies[0]
	if len(c.replies) > 1 {
		c.replies = c.replies[1:]
	}

	return llm.Response{Text: reply}, nil
}

func plan(t *testing.T, chat llm.Client, request string) Plan {
	t.Helper()

	p, err := NewService(nil, nil, chat, Limits{MaxSources: 20}).Plan(context.Background(), 1, 0, request)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

func sourcesOf(p Plan) string {
	parts := make([]string, len(p.Sources))
	for i, s := range p.Sources {
		parts[i] = s.URL + " " + s.Scope
	}

	return strings.Join(parts, ", ")
}

func TestExtractLinks(t *testing.T) {
	got := extractLinks(`Go concurrency: go.dev/doc/ and https://gobyexample.com, also (https://go.dev/doc/) again;
		write to someone@example.com, not ftp://files.example/x. Readme.md? https://GO.dev/blog/#top`)

	want := "[https://go.dev/doc/ https://gobyexample.com/ https://go.dev/blog/]"
	if fmt.Sprint(got) != want {
		t.Errorf("links = %v, want %v", got, want)
	}
}

func TestModelPlanKeepsOnlyTheRequestsLinks(t *testing.T) {
	chat := &scriptedChat{replies: []string{"```json\n" + `{"name": "Go concurrency", "topic": "Goroutines,\n channels.", "sources": [{"url": "https://go.dev/doc/", "scope": "section"}, {"url": "https://evil.example/", "scope": "site"},
		{"url": "gobyexample.com", "scope": "everything"}, {"url": "https://go.dev/ref/mem", "scope": "site"}], "reply": "I will collect the Go docs."}` + "\n```"}}

	p := plan(t, chat, "a lab on Go concurrency from go.dev/doc/, go.dev/ref/mem and gobyexample.com")

	if p.Name != "Go concurrency" || p.Topic != "Goroutines, channels." || p.Model != "small" || p.Reply != "I will collect the Go docs." {
		t.Errorf("plan = %+v", p)
	}

	if got := sourcesOf(p); got != "https://go.dev/doc/ section, https://gobyexample.com/ site, https://go.dev/ref/mem section" {
		t.Errorf("sources = %s: an invented link must go, an unknown scope be guessed, a deep link not mean the site", got)
	}

	req := chat.requests[0]
	if !req.JSON || req.Temperature == nil || *req.Temperature != 0 || !strings.Contains(req.Messages[0].Content, "- https://gobyexample.com/") {
		t.Errorf("request = %+v", req)
	}
}

func TestAModelDroppingEveryLinkIsOverruled(t *testing.T) {
	p := plan(t, &scriptedChat{replies: []string{`{"name": "Go", "topic": "", "sources": [], "reply": ""}`}},
		"go.dev/doc/ and gobyexample.com")

	if got := sourcesOf(p); got != "https://go.dev/doc/ section, https://gobyexample.com/ site" {
		t.Errorf("sources = %s", got)
	}
}

func TestUnusableRepliesAreRetriedThenReadLiterally(t *testing.T) {
	chat := &scriptedChat{replies: []string{"Sure! Here is your lab.", `{"name": "Go concurrency", "sources": []}`}}

	p := plan(t, chat, "Go concurrency go.dev/doc/")

	if len(chat.requests) != 2 || p.Model != "small" || p.Name != "Go concurrency" {
		t.Fatalf("%d requests, plan %+v", len(chat.requests), p)
	}

	retry := chat.requests[1].Messages
	if len(retry) != 3 || retry[1].Content != "Sure! Here is your lab." || !strings.Contains(retry[2].Content, "no JSON object") {
		t.Errorf("the retry must show the model its reply and what was wrong: %+v", retry)
	}

	chat = &scriptedChat{replies: []string{"no", `{"name": ""}`}}
	p = plan(t, chat, "Effective Go and the memory model: go.dev/doc/effective_go go.dev/ref/mem")

	if p.Model != "" || p.Name != "Effective Go and the memory" || sourcesOf(p) != "https://go.dev/doc/effective_go section, https://go.dev/ref/mem section" {
		t.Errorf("literal plan = %+v", p)
	}
}

func TestPlanWithoutAModel(t *testing.T) {
	p := plan(t, &scriptedChat{err: errors.New("ollama is down")}, "https://gobyexample.com/")

	if p.Model != "" || p.Name != "gobyexample.com" || sourcesOf(p) != "https://gobyexample.com/ site" {
		t.Errorf("plan = %+v", p)
	}

	if p := plan(t, nil, "  Rust async\n\n"); p.Name != "Rust async" || p.Topic != "Rust async" || len(p.Sources) != 0 {
		t.Errorf("plan = %+v", p)
	}
}

func TestPlanRejectsBadRequests(t *testing.T) {
	s := NewService(nil, nil, nil, Limits{MaxSources: 20})

	for _, request := range []string{"", "   ", strings.Repeat("x", maxRequestRunes+1), linksText(maxPlanLinks + 1)} {
		if _, err := s.Plan(context.Background(), 1, 0, request); !errors.Is(err, ErrInvalid) {
			t.Errorf("request of %d characters: %v", len(request), err)
		}
	}
}

func linksText(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, " https://site%d.example/", i)
	}

	return b.String()
}
