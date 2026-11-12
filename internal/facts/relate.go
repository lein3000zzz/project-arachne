package facts

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"web-crawler/internal/copies"
	"web-crawler/internal/embedding"
	"web-crawler/internal/llm"

	"go.uber.org/zap"
)

// Relations between two facts of different pages.
const (
	Same       = "same"
	Contradict = "contradict"
	Different  = "different"
)

// Asked as a retrieval of statements asserting the same fact, the embedder
// set restatements apart from facts on the same topic; plain, it did not.
const embedInstruction = "Instruct: Given a statement, retrieve statements that assert the same fact\nQuery: "

const relatePrompt = `You compare two statements taken from different sources.

"same": they assert the same fact, worded differently or in different languages. One may add a small detail the other lacks.
"contradict": they cannot both be true - different numbers, dates or outcomes for the same thing, or one denies what the other asserts.
"different": anything else - different facts about one topic, or one statement reports a claim that the other judges.

Reply with JSON only: {"relation": "same" | "contradict" | "different"}`

const relateTokens = 60

// RelateStore is where embeddings and the pairs compared are kept.
type RelateStore interface {
	Unembedded(ctx context.Context, model string, limit int) ([]Claim, error)
	SaveEmbeddings(ctx context.Context, model string, claims []Claim) error
	// LabsToRelate is the labs whose live embedded facts changed since every
	// pair of them worth asking about was asked, each with how they stand now.
	LabsToRelate(ctx context.Context, model string) (map[string]string, error)
	LabClaims(ctx context.Context, labID, model string) ([]Claim, error)
	Compared(ctx context.Context, ids []string) (map[[2]string]bool, error)
	SaveLink(ctx context.Context, a, b, relation string, similarity float64, model string) error
	SetLabState(ctx context.Context, labID, signature string) error
}

// Sketches gives pages' MinHash sketches, to tell copies of one text apart.
type Sketches interface {
	Sketches(ctx context.Context, urls []string) (map[string]copies.Sketch, error)
}

// Claim is a fact as relating sees it.
type Claim struct {
	ID     string
	URL    string
	Text   string
	Vector []float32
}

type RelateConfig struct {
	// Pairs at least this similar are shown to the model; the rest are
	// different facts. On a labelled set every restatement scored 0.77 or
	// more, every different fact 0.75 or less, contradictions 0.55 to 0.96.
	Shortlist float64
	// Pairs asked about per sweep: digesting new pages goes on between.
	PairsPerSweep int
	EmbedBatch    int
}

// Relater finds which facts of a lab say the same thing and which cannot
// both be true: what agreement and contest in the ledger rest on.
type Relater struct {
	logger   *zap.SugaredLogger
	store    RelateStore
	embedder embedding.Embedder
	model    llm.Client
	sketches Sketches
	cfg      RelateConfig
}

func NewRelater(logger *zap.SugaredLogger, store RelateStore, embedder embedding.Embedder, model llm.Client, sketches Sketches, cfg RelateConfig) *Relater {
	cfg.Shortlist = cmp.Or(cfg.Shortlist, 0.70)
	cfg.PairsPerSweep = cmp.Or(cfg.PairsPerSweep, 40)
	cfg.EmbedBatch = cmp.Or(cfg.EmbedBatch, 32)

	return &Relater{logger: logger, store: store, embedder: embedder, model: model, sketches: sketches, cfg: cfg}
}

// Embed gives every live fact an embedding by the current embedder.
func (r *Relater) Embed(ctx context.Context) error {
	for {
		claims, err := r.store.Unembedded(ctx, r.embedder.Model(), r.cfg.EmbedBatch)
		if err != nil || len(claims) == 0 {
			return err
		}

		texts := make([]string, len(claims))
		for i, c := range claims {
			texts[i] = embedInstruction + c.Text
		}

		vectors, err := r.embedder.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("embedding facts: %w", err)
		}

		if len(vectors) != len(claims) {
			return fmt.Errorf("embedding facts: %d vectors for %d claims", len(vectors), len(claims))
		}

		for i := range claims {
			claims[i].Vector = unit(vectors[i])
		}

		if err := r.store.SaveEmbeddings(ctx, r.embedder.Model(), claims); err != nil {
			return err
		}
	}
}

// Relate asks about the pairs worth asking about, lab by lab, as far as the
// sweep's budget goes; a lab is done once none is left.
func (r *Relater) Relate(ctx context.Context) error {
	labs, err := r.store.LabsToRelate(ctx, r.embedder.Model())
	if err != nil {
		return err
	}

	budget := r.cfg.PairsPerSweep

	for lab, signature := range labs {
		done, asked, err := r.relateLab(ctx, lab, budget)
		if err != nil {
			return err
		}

		budget -= asked

		if done {
			if err := r.store.SetLabState(ctx, lab, signature); err != nil {
				return err
			}
		}

		if budget <= 0 {
			return nil
		}
	}

	return nil
}

type pair struct {
	a, b       int
	similarity float64
}

func (r *Relater) relateLab(ctx context.Context, lab string, budget int) (done bool, asked int, err error) {
	claims, err := r.store.LabClaims(ctx, lab, r.embedder.Model())
	if err != nil || len(claims) < 2 {
		return err == nil, 0, err
	}

	ids := make([]string, len(claims))
	var urls []string

	for i, c := range claims {
		ids[i] = c.ID
		if !slices.Contains(urls, c.URL) {
			urls = append(urls, c.URL)
		}
	}

	compared, err := r.store.Compared(ctx, ids)
	if err != nil {
		return false, 0, err
	}

	sketches, err := r.sketches.Sketches(ctx, urls)
	if err != nil {
		return false, 0, err
	}

	var pairs []pair

	for i := range claims {
		for j := i + 1; j < len(claims); j++ {
			a, b := claims[i], claims[j]
			if a.URL == b.URL || compared[key(a.ID, b.ID)] {
				continue
			}

			// A page and its copy agree with each other by being one text:
			// that is not two sources.
			if sa, sb := sketches[a.URL], sketches[b.URL]; len(sa) > 0 && len(sb) > 0 && copies.Same(sa, sb) {
				continue
			}

			if s := dot(a.Vector, b.Vector); s >= r.cfg.Shortlist {
				pairs = append(pairs, pair{i, j, s})
			}
		}
	}

	slices.SortFunc(pairs, func(x, y pair) int { return cmp.Compare(y.similarity, x.similarity) })

	for _, p := range pairs {
		if asked == budget {
			return false, asked, nil
		}

		relation, err := r.judge(ctx, claims[p.a].Text, claims[p.b].Text)
		if err != nil {
			return false, asked, err
		}

		asked++

		if err := r.store.SaveLink(ctx, claims[p.a].ID, claims[p.b].ID, relation, p.similarity, llm.ModelOf(ctx, r.model)); err != nil {
			return false, asked, err
		}
	}

	return true, asked, nil
}

// judge asks both ways round: the pair is the same only when both say so, and
// contradictory when either does - a small model judges a pair differently
// by its order, and a contradiction missed is worse than one asked about.
func (r *Relater) judge(ctx context.Context, a, b string) (string, error) {
	first, err := r.ask(ctx, a, b)
	if err != nil {
		return "", err
	}

	second, err := r.ask(ctx, b, a)
	if err != nil {
		return "", err
	}

	switch {
	case first == Contradict || second == Contradict:
		return Contradict, nil
	case first == Same && second == Same:
		return Same, nil
	default:
		return Different, nil
	}
}

func (r *Relater) ask(ctx context.Context, a, b string) (string, error) {
	resp, err := r.model.Stream(ctx, llm.Request{
		System:      relatePrompt,
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: "A: " + a + "\nB: " + b}},
		MaxTokens:   relateTokens,
		Temperature: new(float64),
		JSON:        true,
	}, nil)
	if err != nil {
		return "", err
	}

	// An unreadable reply counts as no relation: nothing is claimed of the
	// pair, and it is not asked about forever.
	var got struct {
		Relation string `json:"relation"`
	}

	text := resp.Text
	if start, end := strings.Index(text, "{"), strings.LastIndex(text, "}"); start >= 0 && end > start {
		_ = json.Unmarshal([]byte(text[start:end+1]), &got)
	}

	switch rel := strings.ToLower(strings.TrimSpace(got.Relation)); rel {
	case Same, Contradict:
		return rel, nil
	default:
		return Different, nil
	}
}

// key orders a pair as fact_links does: the lower id first.
func key(a, b string) [2]string {
	if b < a {
		a, b = b, a
	}

	return [2]string{a, b}
}

func unit(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}

	if sum == 0 {
		return v
	}

	n := float32(math.Sqrt(sum))
	out := make([]float32, len(v))

	for i, x := range v {
		out[i] = x / n
	}

	return out
}

func dot(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}

	var s float32
	for i := range a {
		s += a[i] * b[i]
	}

	return float64(s)
}
