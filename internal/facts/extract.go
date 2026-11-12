// Package facts digests indexed pages into short claims, each resting on
// sentences of the page: the ledger answers are built from.
package facts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
	"web-crawler/internal/llm"
)

// Fact is one statement a page asserts and the page's own words for it.
type Fact struct {
	Claim string `json:"claim"`
	About string `json:"about"`
	// The sentences of the passage it rests on, numbered from 1, as the
	// model named them; Quote is their text, taken by code.
	Sentences []int  `json:"sentences"`
	Quote     string `json:"quote"`
}

// Rejected is a fact the model offered that its sentences do not bear out.
type Rejected struct {
	Claim string `json:"claim"`
	Why   string `json:"why"`
}

const (
	maxFacts          = 6
	maxClaimRunes     = 400
	maxAboutRunes     = 60
	maxCited          = 3
	maxSentencesShown = 80
	// Six facts with their sentence numbers fit well within; a reply that
	// runs on is cut here rather than left to loop until a timeout.
	extractTokens = 700
	// Share of a claim's content words its sentences must hold. Claims
	// rephrase and name their subject, so not all; below this a claim says
	// what its sentences do not, or says it in another language.
	minSupport = 0.5
)

// The model points at sentences; it never copies them. Asked to copy a
// quote, a 4B model copied whole passages and looped until cut off.
const extractPrompt = `You pull facts out of a passage for a research index. The passage comes as numbered sentences.

A fact is one statement the passage itself asserts: a finding, a definition, a number, a date, a cause, who did or argued what.

For each fact:
"claim": the fact as one short sentence, at most 20 words, that makes sense alone. Name who or what it is about - never "I", "we", "it", "this paper", "the author": use the name from the page title or the passage. Use the passage's own words and its numbers exactly, but leave out asides, references in brackets and linking words such as "so" or "thus".
"about": what the fact is about, in 1 to 4 words.
"sentences": the numbers of the sentences that state it - one, or a few in a row.

Not facts, so leave them out: titles, author names, publishers, ISBNs, grant or page numbers, tables of contents, menus, links, references lists, questions, and what the passage only mentions.

Example. Page title: "Henn - Complements or Substitutes? State Capacity and Chiefs".
[1] I use distance to the capital as a measure of state capacity.
[2] Where chiefs are not in the constitution, they compete with the state.
{"facts":[{"claim":"Henn measures state capacity by the distance to the capital.","about":"state capacity","sentences":[1]},{"claim":"Henn finds that chiefs who are not in the constitution compete with the state.","about":"chiefs and the state","sentences":[2]}]}

At most 6 facts; an empty list when the passage asserts nothing. Reply with the JSON object only.`

// ErrReply is a reply that could not be read as facts. Asking again gets
// the same reply, unlike a model that was down.
var ErrReply = errors.New("the reply holds no facts to read")

type Extractor struct {
	model llm.Client
}

func NewExtractor(model llm.Client) *Extractor {
	return &Extractor{model: model}
}

// Result is what one passage gave: the facts kept, and those offered that
// their sentences do not bear out.
type Result struct {
	Facts    []Fact
	Rejected []Rejected
	Model    string
}

// Extract asks for the facts of one passage of a page.
func (e *Extractor) Extract(ctx context.Context, title, passage string) (Result, error) {
	// The index keeps a chunk with its page's title in front, for embedding;
	// the title is not something the page asserts.
	if t := strings.TrimSpace(title); t != "" {
		passage = strings.TrimPrefix(strings.TrimSpace(passage), t)
	}

	sentences := Sentences(passage)
	if len(sentences) > maxSentencesShown {
		sentences = sentences[:maxSentencesShown]
	}

	res := Result{Model: llm.ModelOf(ctx, e.model), Facts: []Fact{}}
	if len(sentences) == 0 {
		return res, nil
	}

	var b strings.Builder

	fmt.Fprintf(&b, "Page title: %q\n", oneLine(title, 200))

	if language(passage) == "Russian" {
		b.WriteString("Write each claim and \"about\" in Russian, as the passage is.\n")
	}

	b.WriteString("\n")

	for i, s := range sentences {
		fmt.Fprintf(&b, "[%d] %s\n", i+1, s)
	}

	resp, err := e.model.Stream(ctx, llm.Request{
		System:      extractPrompt,
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: b.String()}},
		MaxTokens:   extractTokens,
		Temperature: new(float64),
		JSON:        true,
	}, nil)
	if err != nil {
		return Result{}, err
	}

	offered, err := parse(resp.Text)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrReply, err)
	}

	seen := map[string]bool{}

	for _, f := range offered {
		f.Claim, f.About = oneLine(f.Claim, maxClaimRunes), oneLine(f.About, maxAboutRunes)
		reject := func(why string) { res.Rejected = append(res.Rejected, Rejected{Claim: f.Claim, Why: why}) }

		cited := cleanCited(f.Sentences, len(sentences))

		switch {
		case f.Claim == "":
			continue
		case len(cited) == 0:
			reject("no sentence of the passage named")
			continue
		}

		var quote []string
		for _, n := range cited {
			quote = append(quote, sentences[n-1])
		}

		f.Sentences, f.Quote = cited, strings.Join(quote, " ")

		if why := unsupported(f.Claim, f.Quote); why != "" {
			reject(why)
			continue
		}

		if seen[f.Claim] || len(res.Facts) == maxFacts {
			continue
		}

		seen[f.Claim] = true
		res.Facts = append(res.Facts, f)
	}

	return res, nil
}

// cleanCited keeps the first run of sentence numbers in a row that exist,
// at most a few: a quote is then one stretch of the page, found again in a
// later version of it as long as those sentences stand.
func cleanCited(nums []int, n int) []int {
	var valid []int

	for _, x := range nums {
		if x >= 1 && x <= n && !slices.Contains(valid, x) {
			valid = append(valid, x)
		}
	}

	slices.Sort(valid)

	var out []int

	for _, x := range valid {
		if len(out) > 0 && x != out[len(out)-1]+1 || len(out) == maxCited {
			break
		}

		out = append(out, x)
	}

	return out
}

// unsupported says why the quote does not bear the claim out, or "". Numbers
// are where a small model slips, so each of the claim's must be in the quote.
func unsupported(claim, quote string) string {
	have := map[string]bool{}
	for _, w := range words(quote) {
		have[w.text], have[stem(w.text)] = true, true
	}

	var content, held int

	for _, w := range words(claim) {
		number := unicode.IsDigit([]rune(w.text)[0])

		switch {
		case number && !have[w.text]:
			return "the claim has a number its sentences do not: " + w.text
		case number || utf8.RuneCountInString(w.text) >= 4:
			content++
			if have[stem(w.text)] {
				held++
			}
		}
	}

	if content == 0 || float64(held)/float64(content) < minSupport {
		return fmt.Sprintf("its sentences hold %d of the claim's %d content words", held, content)
	}

	return ""
}

// stem is a word's first five letters: crude, but it lets "миграции" meet
// "миграция" and "measures" meet "measure" without a stemmer per language.
func stem(w string) string {
	if r := []rune(w); len(r) > 5 {
		return string(r[:5])
	}

	return w
}

// language names a passage's language where the prompt must say it: a
// small model asked in English answers in English.
func language(s string) string {
	var cyrillic, latin int

	for _, r := range s {
		switch {
		case unicode.Is(unicode.Cyrillic, r):
			cyrillic++
		case unicode.Is(unicode.Latin, r):
			latin++
		}
	}

	if cyrillic > latin {
		return "Russian"
	}

	return ""
}

// parse reads the reply leniently: a small model may wrap the object in
// prose or a code fence, and a reply cut at the token limit keeps the facts
// it finished.
func parse(text string) ([]Fact, error) {
	start := strings.Index(text, "{")
	if start < 0 {
		return nil, fmt.Errorf("the reply holds no JSON object: %.200q", text)
	}

	var got struct {
		Facts []Fact `json:"facts"`
	}
	if end := strings.LastIndex(text, "}"); end > start && json.Unmarshal([]byte(text[start:end+1]), &got) == nil {
		return got.Facts, nil
	}

	list := strings.Index(text[start:], "[")
	if list < 0 {
		return nil, fmt.Errorf("the reply is not the JSON asked for: %.200q", text)
	}

	dec := json.NewDecoder(strings.NewReader(text[start+list:]))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}

	var facts []Fact

	for dec.More() {
		var f Fact
		if err := dec.Decode(&f); err != nil {
			break
		}

		facts = append(facts, f)
	}

	if len(facts) == 0 {
		return nil, fmt.Errorf("the reply is not the JSON asked for: %.200q", text)
	}

	return facts, nil
}

func oneLine(s string, most int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > most {
		s = string([]rune(s)[:most])
	}

	return s
}
