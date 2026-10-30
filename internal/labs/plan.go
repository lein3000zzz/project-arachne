package labs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
	"web-crawler/internal/llm"
	"web-crawler/internal/urlnorm"

	"mvdan.cc/xurls/v2"
)

// ScopePage collects the one page and nothing it links to.
const ScopePage = "page"

// Plan is what the model proposes for a request; nothing happens until the
// user confirms it, and then it is checked again like any other request.
type Plan struct {
	// Zero for a new lab.
	LabID   int64           `json:"lab_id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Topic   string          `json:"topic,omitempty"`
	Sources []SourceRequest `json:"sources"`
	Reply   string          `json:"reply"`
	// The model that drafted it; empty when the model was unavailable or its
	// replies unusable, and the request was read literally instead.
	Model string `json:"model"`
}

const (
	maxRequestRunes = 4000
	maxPlanLinks    = 20
	planMaxTokens   = 400
	maxReplyRunes   = 300
)

const planPrompt = `You set up research labs. A lab collects web pages about one topic, so that questions can later be answered from them.

Reply with one JSON object and nothing else:
{"name": "...", "topic": "...", "sources": [{"url": "...", "scope": "section" or "page" or "site"}], "reply": "..."}

- name: 2 to 5 words naming the subject, in the language of the request.
- topic: one sentence on what the lab should cover.
- sources: links from LINKS only, copied exactly. Leave out a link the request does not want collected. scope is "section" for the part of the site under that address (documentation, a blog, a book), "page" for that one page only (an article), "site" for a whole website given by its home page.
- reply: one short sentence telling the user what the lab will collect.`

var (
	links = xurls.Relaxed()
	zero  = 0.0
)

// Plan drafts a new lab from a request, or with labID non-zero, more sources
// for that lab. A link is only ever one the request contains: the model picks
// scopes and may leave links out, but cannot add one.
func (s *Service) Plan(ctx context.Context, ownerID, labID int64, request string) (Plan, error) {
	request = strings.TrimSpace(request)
	if request == "" || utf8.RuneCountInString(request) > maxRequestRunes {
		return Plan{}, fmt.Errorf("%w: a request is 1 to %d characters", ErrInvalid, maxRequestRunes)
	}

	var lab *Detail

	if labID != 0 {
		detail, err := s.Get(ctx, ownerID, labID)
		if err != nil {
			return Plan{}, err
		}

		lab = &detail
	}

	found := extractLinks(request)
	if len(found) > maxPlanLinks {
		return Plan{}, fmt.Errorf("%w: at most %d links at a time", ErrInvalid, maxPlanLinks)
	}

	if lab != nil {
		found = slices.DeleteFunc(found, func(link string) bool {
			return slices.ContainsFunc(lab.Sources, func(src SourceStatus) bool { return src.URL == link })
		})
	}

	plan := s.draft(ctx, request, found, lab)
	plan.LabID = labID

	if lab != nil {
		plan.Name, plan.Topic = "", ""
	}

	return plan, nil
}

// draft asks the model, once more with its mistake pointed out if the first
// reply is unusable, and reads the request literally if that fails too.
func (s *Service) draft(ctx context.Context, request string, found []string, lab *Detail) Plan {
	if s.chat == nil {
		return literalPlan(request, found, lab)
	}

	messages := []llm.Message{{Role: llm.RoleUser, Content: planInput(request, found, lab)}}

	for attempt := 0; attempt < 2; attempt++ {
		resp, err := s.chat.Stream(ctx, llm.Request{
			System: planPrompt, Messages: messages, MaxTokens: planMaxTokens, Temperature: &zero, JSON: true,
		}, nil)
		if err != nil {
			break
		}

		plan, err := parsePlan(resp.Text, found, lab == nil)
		if err == nil {
			plan.Model = s.chat.Model()
			return plan
		}

		messages = append(messages,
			llm.Message{Role: llm.RoleAssistant, Content: resp.Text},
			llm.Message{Role: llm.RoleUser, Content: "That reply was not usable: " + err.Error() + ". Reply with the JSON object only."})
	}

	return literalPlan(request, found, lab)
}

func planInput(request string, found []string, lab *Detail) string {
	var b strings.Builder

	if lab != nil {
		fmt.Fprintf(&b, "LAB: %s\nTOPIC: %s\nIT ALREADY COLLECTS:\n", lab.Name, lab.Topic)

		for _, src := range lab.Sources {
			fmt.Fprintf(&b, "- %s\n", src.URL)
		}

		b.WriteString("\nThe request asks for more material for this lab; name and topic are ignored. " +
			"LINKS holds only what the lab does not have yet, and the reply mentions only those.\n\n")
	}

	b.WriteString("REQUEST:\n")
	b.WriteString(request)
	b.WriteString("\n\nLINKS:\n")

	if len(found) == 0 {
		b.WriteString("(none)\n")
	}

	for _, link := range found {
		fmt.Fprintf(&b, "- %s\n", link)
	}

	return b.String()
}

type modelPlan struct {
	Name    string `json:"name"`
	Topic   string `json:"topic"`
	Sources []struct {
		URL   string `json:"url"`
		Scope string `json:"scope"`
	} `json:"sources"`
	Reply string `json:"reply"`
}

var errNoObject = errors.New("no JSON object in it")

// parsePlan takes the first JSON object in reply, so a fence or a sentence
// around it does no harm. Links not in found are dropped; if that leaves none
// while the request had some, they are all kept - a model that drops every
// link has misread the request, not narrowed it.
func parsePlan(reply string, found []string, named bool) (Plan, error) {
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end < start {
		return Plan{}, errNoObject
	}

	var raw modelPlan
	if err := json.Unmarshal([]byte(reply[start:end+1]), &raw); err != nil {
		return Plan{}, fmt.Errorf("invalid JSON: %w", err)
	}

	plan := Plan{Sources: []SourceRequest{}, Reply: truncate(oneLine(raw.Reply), maxReplyRunes)}

	if named {
		name, err := cleanName(oneLine(raw.Name))
		if err != nil {
			return Plan{}, errors.New("the name is missing or too long")
		}

		plan.Name = name
		plan.Topic = truncate(oneLine(raw.Topic), maxTopicRunes)
	}

	allowed := make(map[string]bool, len(found))
	for _, link := range found {
		allowed[link] = true
	}

	for _, src := range raw.Sources {
		link, ok := urlnorm.FromUserInput(src.URL)
		if !ok || !allowed[link] {
			continue
		}

		allowed[link] = false
		plan.Sources = append(plan.Sources, SourceRequest{URL: link, Scope: knownScope(src.Scope, link)})
	}

	if len(plan.Sources) == 0 {
		plan.Sources = guessSources(found)
	}

	return plan, nil
}

func literalPlan(request string, found []string, lab *Detail) Plan {
	plan := Plan{Sources: guessSources(found)}

	if lab == nil {
		plan.Name = literalName(request, found)
		plan.Topic = truncate(oneLine(links.ReplaceAllString(request, "")), maxTopicRunes)
	}

	return plan
}

// literalName is the request's first few words, or the first link's host when
// the request is nothing but links.
func literalName(request string, found []string) string {
	words := strings.FieldsFunc(links.ReplaceAllString(request, " "), func(r rune) bool {
		return unicode.IsSpace(r) || r == ',' || r == ';'
	})

	if len(words) > 5 {
		words = words[:5]
	}

	name := strings.Trim(strings.Join(words, " "), ".:!?-–— ")

	if name == "" && len(found) > 0 {
		if u, err := url.Parse(found[0]); err == nil {
			name = u.Hostname()
		}
	}

	if name == "" {
		name = "New lab"
	}

	return truncate(name, maxNameRunes)
}

func guessSources(found []string) []SourceRequest {
	sources := make([]SourceRequest, len(found))
	for i, link := range found {
		sources[i] = SourceRequest{URL: link, Scope: knownScope("", link)}
	}

	return sources
}

// knownScope keeps a scope the model named, except "site" for anything but a
// home page: a deep link rarely means the whole host, and on a shared one
// (github.io) it would mean thousands of strangers' pages. Unknown scopes are
// guessed the same way.
func knownScope(scope, link string) string {
	home := false
	if u, err := url.Parse(link); err == nil {
		home = u.Path == "" || u.Path == "/"
	}

	switch {
	case scope == "section", scope == ScopePage:
		return scope
	case home:
		return "site"
	}

	return "section"
}

// Top-level domains that, bare, are far more often a file name: README.md is
// not a site in Moldova.
var fileLike = map[string]bool{"md": true, "rs": true, "py": true, "sh": true, "zip": true, "mov": true}

// extractLinks finds web addresses in text, with or without a scheme,
// canonical and each once. Anything with an @ is taken for an email address.
func extractLinks(text string) []string {
	var out []string

	seen := map[string]bool{}

	for _, match := range links.FindAllString(text, -1) {
		if strings.Contains(match, "@") {
			continue
		}

		if !strings.Contains(match, "/") && fileLike[strings.ToLower(match[strings.LastIndex(match, ".")+1:])] {
			continue
		}

		link, ok := urlnorm.FromUserInput(match)
		if !ok || seen[link] || !strings.HasPrefix(link, "http") {
			continue
		}

		seen[link] = true
		out = append(out, link)
	}

	return out
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}

	return string([]rune(s)[:limit-1]) + "…"
}
