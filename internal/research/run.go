package research

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"
	"web-crawler/internal/auth"
	"web-crawler/internal/labs"
	"web-crawler/internal/llm"
	"web-crawler/internal/openalex"
	"web-crawler/internal/urlnorm"
	"web-crawler/internal/websearch"

	"golang.org/x/net/publicsuffix"
)

const (
	// Queries asked for per later round, at most.
	maxNewQueries = 6
	// Works of one site read per run: a big site should not fill the lab.
	maxReadsPerSite = 3
	// Reads started per run, refunded ones included, as a multiple of the
	// read budget: a lab of refusing sites cannot read forever.
	maxAttemptsFactor = 2
	// A round with fewer new kept works than this share of all kept ones, or
	// than minNewKept, has found what its channels can find.
	saturationShare = 0.05
	minNewKept      = 2
	// Judging batches the model answered with nothing, in a row, before a
	// round stops judging.
	maxEmptyBatches = 2
)

type session struct {
	s       *Service
	id      int64
	ownerID int64
	lab     labs.Detail
	request string
	budget  Budget
	focus   Focus

	subs    []Subtopic
	planned bool
	rounds  []Round
	cur     *Round
	tried   []string

	judged, read int
	attempts     int
	// Works read per site this run.
	sites        map[string]int
	readNow      map[int64]bool
	followedRefs map[int64]bool

	// The lab's works by address, DOI and OpenAlex ID, so what is known is
	// neither looked up again nor found anew.
	byURL, byDOI, byOA map[string]int64
	urlOf              map[int64]string
}

func (s *Service) run(id, ownerID int64, lab labs.Detail, request string, budget Budget, focus Focus) {
	// The owner's own endpoint judges, when they have one.
	ctx, cancel := context.WithTimeout(auth.WithUser(s.ctx, auth.User{ID: ownerID}), s.cfg.Timeout)
	defer cancel()

	r := &session{
		s: s, id: id, ownerID: ownerID, lab: lab, request: request, budget: budget, focus: focus,
		sites: map[string]int{}, readNow: map[int64]bool{}, followedRefs: map[int64]bool{},
		byURL: map[string]int64{}, byDOI: map[string]int64{}, byOA: map[string]int64{}, urlOf: map[int64]string{},
	}

	started := time.Now()

	stopped, err := r.research(ctx)

	status, errText := StatusDone, ""

	switch {
	case err == nil:
	case s.ctx.Err() != nil:
		status, stopped, errText = StatusFailed, StopInterrupted, "interrupted by a shutdown"
	case errors.Is(err, context.DeadlineExceeded):
		// What was found by then stays: out of time is a way to end.
		stopped = StopTime
	case errors.Is(err, llm.ErrNoModel):
		status, errText = StatusFailed, llm.ErrNoModel.Error()
	case errors.Is(err, llm.ErrRequest):
		status, errText = StatusFailed, "the chat model failed the request - unreachable, a wrong model name or a bad key: the server log says which"
		s.logger.Warnw("Research failed", "run", id, "error", err)
	default:
		status, errText = StatusFailed, "the model could not be reached"
		s.logger.Warnw("Research failed", "run", id, "error", err)
	}

	r.save(ctx)

	s.logger.Infow("Research finished", "run", id, "lab", lab.ID, "status", status, "stopped", stopped, "rounds", len(r.rounds),
		"judged", r.judged, "read", r.read, "seconds", fmt.Sprintf("%.0f", time.Since(started).Seconds()))

	if err := s.store.FinishRun(context.WithoutCancel(ctx), id, status, stopped, errText); err != nil {
		s.logger.Warnw("Failed to record the end of research", "run", id, "error", err)
	}
}

// research runs rounds until the topic looks saturated or the budget, the
// rounds or the time run out, and says which.
func (r *session) research(ctx context.Context) (string, error) {
	if err := r.load(ctx); err != nil {
		return "", err
	}

	if len(r.subs) == 0 {
		planned, err := r.s.plan(ctx, r.lab, r.request)
		if err != nil {
			return "", err
		}

		if r.subs, err = r.s.store.AddSubtopics(ctx, r.lab.ID, planned); err != nil {
			return "", err
		}

		r.planned = true
	}

	rounds := r.s.cfg.Rounds

	switch {
	case r.focus.Work != 0:
		rounds = 1
	case r.focus.Subtopic == 0:
		r.seed(ctx)
	}

	for n := 1; n <= rounds; n++ {
		r.cur = &Round{N: n, Found: map[string]int{}}

		queries, err := r.queries(ctx, n)
		if err != nil {
			return "", err
		}

		r.cur.Queries = queries
		r.save(ctx)

		r.searchFor(ctx, queries)
		r.followReferences(ctx)
		r.followCiting(ctx)

		kept, err := r.judgeAll(ctx)
		if err != nil {
			return "", err
		}

		r.readKept(ctx)

		round := *r.cur
		r.rounds, r.cur = append(r.rounds, round), nil
		r.save(ctx)

		r.waitForReads(ctx)

		if err := ctx.Err(); err != nil {
			return "", err
		}

		found := 0
		for _, k := range round.Found {
			found += k
		}

		switch {
		case r.focus.Work != 0:
			return StopFollowed, nil
		case r.judged >= r.budget.Found:
			return StopBudget, nil
		case found == 0 && round.Judged == 0:
			return StopSaturated, nil
		case n >= 2 && round.Kept < max(minNewKept, int(saturationShare*float64(kept))):
			return StopSaturated, nil
		}
	}

	return StopRounds, nil
}

// load reads what the lab already has, so a run goes on from where the last
// one stopped.
func (r *session) load(ctx context.Context) error {
	var err error

	if r.subs, err = r.s.store.Subtopics(ctx, r.lab.ID); err != nil {
		return err
	}

	works, err := r.s.store.Works(ctx, r.lab.ID)
	if err != nil {
		return err
	}

	for _, w := range works {
		r.know(w.ID, w)

		for _, f := range w.Finds {
			if f.Channel == ChannelSearch && f.Query != "" && !containsFold(r.tried, f.Query) {
				r.tried = append(r.tried, f.Query)
			}
		}
	}

	return nil
}

func (r *session) know(id int64, w Work) {
	if w.URL != "" {
		r.byURL[w.URL], r.urlOf[id] = id, w.URL
	}

	if w.DOI != "" {
		r.byDOI[w.DOI] = id
	}

	if w.OpenAlexID != "" {
		r.byOA[w.OpenAlexID] = id
	}
}

func (r *session) known(w Work) (int64, bool) {
	if id, ok := r.byURL[w.URL]; ok && w.URL != "" {
		return id, true
	}

	if id, ok := r.byDOI[w.DOI]; ok && w.DOI != "" {
		return id, true
	}

	if id, ok := r.byOA[w.OpenAlexID]; ok && w.OpenAlexID != "" {
		return id, true
	}

	return 0, false
}

func (r *session) save(ctx context.Context) {
	rounds := slices.Clone(r.rounds)
	if r.cur != nil {
		rounds = append(rounds, *r.cur)
	}

	if err := r.s.store.SaveRounds(context.WithoutCancel(ctx), r.id, rounds); err != nil {
		r.s.logger.Warnw("Failed to record research progress", "run", r.id, "error", err)
	}
}

func (r *session) failed(what, why string) {
	if r.cur.Failed == nil {
		r.cur.Failed = map[string]string{}
	}

	r.cur.Failed[what] = why
}

// seed makes the lab's own sources works of the map, kept: they are what the
// user chose, and their references lead on.
func (r *session) seed(ctx context.Context) {
	for _, src := range r.lab.Sources {
		if src.Kind == labs.KindResearch {
			continue
		}

		w := Work{URL: src.URL, Kind: KindWeb, Site: hostOf(src.URL), DOI: doiOf(src.URL), Verdict: VerdictKeep, SourceID: src.ID}
		if w.DOI != "" {
			w.Kind = KindPaper
		}

		r.take(ctx, []Work{w}, Find{Channel: ChannelSeed}, nil)
	}
}

// queries are the round's searches: a new plan's own queries first, then
// queries the model writes for the subtopics with the fewest kept works. A
// run on one subtopic searches for it alone, starting with its own queries
// not yet tried; one following a work searches nothing.
func (r *session) queries(ctx context.Context, n int) ([]string, error) {
	subs := r.subs

	switch {
	case r.focus.Work != 0:
		return nil, nil
	case r.focus.Subtopic != 0:
		subs = slices.DeleteFunc(slices.Clone(r.subs), func(s Subtopic) bool { return s.ID != r.focus.Subtopic })
	}

	var untried []string

	if r.focus.Subtopic != 0 && n == 1 {
		for _, q := range subs[0].Queries {
			if !containsFold(r.tried, q) {
				untried = append(untried, q)
			}
		}
	}

	if n == 1 && r.planned && r.request == "" {
		var out []string

		for _, sub := range r.subs {
			for _, q := range sub.Queries {
				if !containsFold(out, q) {
					out = append(out, q)
				}
			}
		}

		return out, nil
	}

	works, err := r.s.store.Works(ctx, r.lab.ID)
	if err != nil {
		return nil, err
	}

	kept := map[int64][]string{}

	for _, w := range works {
		if w.Verdict == VerdictKeep {
			kept[w.SubtopicID] = append(kept[w.SubtopicID], titleOf(w))
		}
	}

	more, err := r.s.moreQueries(ctx, r.lab, r.request, subs, kept, append(slices.Clone(r.tried), untried...), min(maxNewQueries, 2*len(subs)+1))

	return append(untried, more...), err
}

// follows says whose citations a round follows: every kept work's, or the
// focus's alone - the work asked for, whatever its verdict, or the kept works
// of the subtopic asked for.
func (r *session) follows(w Work) bool {
	switch {
	case r.focus.Work != 0:
		return w.ID == r.focus.Work
	case r.focus.Subtopic != 0:
		return w.Verdict == VerdictKeep && w.SubtopicID == r.focus.Subtopic
	}

	return w.Verdict == VerdictKeep
}

// judges says what a run judges: everything pending, or only what is in its
// focus - the works found through the work it follows, whenever they were
// found, or what this run found for its subtopic. The rest waits for a run
// over the whole topic.
func (r *session) judges(w Work) bool {
	switch {
	case r.focus.Work != 0:
		return slices.ContainsFunc(w.Finds, func(f Find) bool { return f.Via == r.focus.Work })
	case r.focus.Subtopic != 0:
		return slices.ContainsFunc(w.Finds, func(f Find) bool { return f.Run == r.id })
	}

	return true
}

// forced: a work the user asked to follow is followed again, even if done.
func (r *session) forced(w Work) bool {
	return r.focus.Work == w.ID
}

// mayRead says what a focused run reads: the works found through the work
// it follows, or the subtopic's.
func (r *session) mayRead(w Work) bool {
	switch {
	case r.focus.Work != 0:
		return slices.ContainsFunc(w.Finds, func(f Find) bool { return f.Via == r.focus.Work })
	case r.focus.Subtopic != 0:
		return w.SubtopicID == r.focus.Subtopic
	}

	return true
}

// searchFor searches the web with the lab's engines and OpenAlex for each
// query.
func (r *session) searchFor(ctx context.Context, queries []string) {
	for _, q := range queries {
		if ctx.Err() != nil {
			return
		}

		r.tried = append(r.tried, q)

		var found []Work

		if r.s.search != nil && len(r.lab.Engines) > 0 {
			resp, err := r.s.search.Search(ctx, q, r.lab.Engines)

			switch {
			case err != nil:
				r.failed("web search", "the engines did not answer")
			default:
				for engine, why := range resp.Failed {
					r.failed(engine, why)
				}

				for _, res := range resp.Results[:min(len(resp.Results), r.s.cfg.SearchResults)] {
					if w, ok := webWork(res); ok {
						found = append(found, w)
					}
				}
			}
		}

		if r.s.scholar != nil {
			papers, err := r.s.scholar.Search(ctx, q, r.s.cfg.PaperResults)
			if err != nil {
				r.scholarFailed(err)
			}

			for _, p := range papers {
				if worthJudging(p) {
					found = append(found, r.s.paper(p))
				}
			}
		}

		r.take(ctx, found, Find{Channel: ChannelSearch, Query: q}, nil)
	}
}

func (r *session) scholarFailed(err error) {
	switch {
	case errors.Is(err, openalex.ErrRefused):
		r.failed("OpenAlex", "refused: its rate limit or the day's budget is spent")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
	default:
		r.failed("OpenAlex", "did not answer")
	}
}

// worthJudging leaves out what OpenAlex lists that no one cites as a work:
// front matter, errata.
func worthJudging(p openalex.Work) bool {
	return p.Title != "" && p.Type != "paratext" && p.Type != "erratum"
}

func webWork(res websearch.Result) (Work, bool) {
	u, ok := urlnorm.Canonical(res.URL, nil)
	if !ok || skippable(u) {
		return Work{}, false
	}

	w := Work{URL: u, Kind: KindWeb, Title: oneLine(res.Title), Snippet: oneLine(res.Snippet), Site: hostOf(u), DOI: doiOf(u)}
	if w.DOI != "" {
		w.Kind = KindPaper
	}

	return w, true
}

// entryNumber is a reference list's own numbering, "[3] " or "12. ", which is
// no part of the work's title.
var entryNumber = regexp.MustCompile(`^\s*(?:\[\d{1,4}\]|\d{1,4}\.)\s*`)

// followReferences takes, for each kept paper, the works OpenAlex says it
// cites, and for each work read, what its pages cite.
func (r *session) followReferences(ctx context.Context) {
	works, err := r.s.store.Works(ctx, r.lab.ID)
	if err != nil {
		r.failed("references", "could not be read")
		return
	}

	if r.s.scholar != nil {
		r.paperReferences(ctx, works)
	}

	var read []int64

	for _, w := range works {
		if w.Read == ReadIn && r.follows(w) && !r.followedRefs[w.ID] {
			read = append(read, w.ID)
			r.followedRefs[w.ID] = true
		}
	}

	if len(read) == 0 {
		return
	}

	refs, err := r.s.store.ReferencesOf(ctx, read)
	if err != nil {
		r.failed("references", "could not be read")
		return
	}

	taken := map[int64]int{}

	for _, ref := range refs {
		if taken[ref.Work] >= r.s.cfg.PerWork || ctx.Err() != nil {
			continue
		}

		u, ok := urlnorm.Canonical(ref.URL, nil)
		if !ok || skippable(u) {
			continue
		}

		taken[ref.Work]++

		w := Work{URL: u, Kind: KindWeb, Title: cut(entryNumber.ReplaceAllString(ref.Label, ""), 300), Site: hostOf(u), DOI: doiOf(u)}
		if w.DOI != "" {
			w.Kind = KindPaper
		}

		from := ref.Work
		r.take(ctx, []Work{w}, Find{Channel: ChannelReference, Via: from}, func(id int64) Link { return Link{From: from, To: id} })
	}
}

func (r *session) paperReferences(ctx context.Context, works []Work) {
	var ids []string

	byID := map[string]Work{}

	for _, w := range works {
		if r.follows(w) && w.OpenAlexID != "" && (!w.ReferencesDone || r.forced(w)) {
			ids = append(ids, w.OpenAlexID)
			byID[w.OpenAlexID] = w
		}
	}

	if len(ids) == 0 {
		return
	}

	papers, err := r.s.scholar.Works(ctx, ids)
	if err != nil {
		r.scholarFailed(err)
		return
	}

	for _, p := range papers {
		citing, ok := byID[p.ID]
		if !ok {
			continue
		}

		refs := p.References[:min(len(p.References), r.s.cfg.PerWork)]

		var unknown []string

		for _, ref := range refs {
			if id, ok := r.byOA[ref]; ok {
				r.take(ctx, []Work{{URL: r.urlOf[id]}}, Find{Channel: ChannelReference, Via: citing.ID}, func(to int64) Link { return Link{From: citing.ID, To: to} })
			} else {
				unknown = append(unknown, ref)
			}
		}

		cited, err := r.s.scholar.Works(ctx, unknown)
		if err != nil {
			r.scholarFailed(err)
			return
		}

		var found []Work

		for _, c := range cited {
			if worthJudging(c) {
				found = append(found, r.s.paper(c))
			}
		}

		r.take(ctx, found, Find{Channel: ChannelReference, Via: citing.ID}, func(to int64) Link { return Link{From: citing.ID, To: to} })

		if err := r.s.store.Expanded(context.WithoutCancel(ctx), citing.ID, true, false); err != nil {
			r.s.logger.Warnw("Failed to record followed references", "work", citing.ID, "error", err)
		}
	}
}

// followCiting takes, for each kept paper, the works citing it: newer work,
// often the niche follow-ups a search misses.
func (r *session) followCiting(ctx context.Context) {
	if r.s.scholar == nil {
		return
	}

	works, err := r.s.store.Works(ctx, r.lab.ID)
	if err != nil {
		r.failed("citing works", "could not be read")
		return
	}

	for _, w := range works {
		if !r.follows(w) || w.OpenAlexID == "" || (w.CitingDone && !r.forced(w)) || ctx.Err() != nil {
			continue
		}

		papers, err := r.s.scholar.Citing(ctx, w.OpenAlexID, r.s.cfg.PerWork)
		if err != nil {
			r.scholarFailed(err)
			return
		}

		var found []Work

		for _, p := range papers {
			if worthJudging(p) {
				found = append(found, r.s.paper(p))
			}
		}

		cited := w.ID
		r.take(ctx, found, Find{Channel: ChannelCiting, Via: cited}, func(from int64) Link { return Link{From: from, To: cited} })

		if err := r.s.store.Expanded(context.WithoutCancel(ctx), cited, false, true); err != nil {
			r.s.logger.Warnw("Failed to record followed citations", "work", cited, "error", err)
		}
	}
}

// take records a find of each work. A work with a DOI the lab does not know
// is looked up in OpenAlex first, so a preprint, its published version and a
// citation of either end as one work.
func (r *session) take(ctx context.Context, found []Work, f Find, link func(id int64) Link) {
	f.Round = r.roundN()

	for _, w := range found {
		if ctx.Err() != nil {
			return
		}

		if _, ok := r.known(w); !ok && w.OpenAlexID == "" && w.DOI != "" && r.s.scholar != nil {
			w = r.enrich(ctx, w)
		}

		if w.URL == "" {
			continue
		}

		id, added, err := r.s.store.Find(ctx, r.lab.ID, r.id, w, f)
		if err != nil {
			r.s.logger.Warnw("Failed to record a work", "url", w.URL, "error", err)
			continue
		}

		r.know(id, w)

		if added && r.cur != nil {
			r.cur.Found[f.Channel]++
		}

		if link != nil {
			if l := link(id); l.From != l.To {
				if err := r.s.store.Link(ctx, r.lab.ID, l); err != nil {
					r.s.logger.Warnw("Failed to record a citation", "from", l.From, "to", l.To, "error", err)
				}
			}
		}
	}
}

// enrich gives a work found by its DOI what OpenAlex knows of it. A seed
// keeps its address: it is the lab's source.
func (r *session) enrich(ctx context.Context, w Work) Work {
	p, err := r.s.scholar.ByDOI(ctx, w.DOI)
	if err != nil {
		r.scholarFailed(err)
		return w
	}

	if p == nil {
		return w
	}

	enriched := r.s.paper(*p)

	if w.SourceID != 0 {
		enriched.URL = w.URL
	}

	enriched.Verdict, enriched.SourceID = w.Verdict, w.SourceID
	if enriched.Title == "" {
		enriched.Title = w.Title
	}

	if enriched.DOI == "" {
		enriched.DOI = w.DOI
	}

	return enriched
}

func (r *session) roundN() int {
	if r.cur == nil {
		return 0
	}

	return r.cur.N
}

// judgeAll judges pending works, the ones found more ways first, until the
// run's budget is spent. It returns how many works the lab keeps.
func (r *session) judgeAll(ctx context.Context) (int, error) {
	works, err := r.s.store.Works(ctx, r.lab.ID)
	if err != nil {
		return 0, err
	}

	kept := 0

	var pending []Work

	for _, w := range works {
		switch {
		case w.Verdict == VerdictPending && r.judges(w):
			pending = append(pending, w)
		case w.Verdict == VerdictKeep:
			kept++
		}
	}

	// What this run found first: a focused run judges what it was asked for
	// before the backlog.
	foundNow := func(w Work) int {
		if slices.ContainsFunc(w.Finds, func(f Find) bool { return f.Run == r.id }) {
			return 1
		}

		return 0
	}

	slices.SortStableFunc(pending, func(a, b Work) int {
		return cmp.Or(cmp.Compare(foundNow(b), foundNow(a)), cmp.Compare(channels(b), channels(a)), cmp.Compare(len(b.Finds), len(a.Finds)),
			cmp.Compare(a.ID, b.ID))
	})

	empty := 0

	for len(pending) > 0 && r.judged < r.budget.Found {
		if err := ctx.Err(); err != nil {
			return kept, err
		}

		batch := pending[:min(len(pending), r.s.cfg.JudgeBatch, r.budget.Found-r.judged)]
		pending = pending[len(batch):]

		verdicts, err := r.s.judge(ctx, r.lab, r.subs, batch)
		if err != nil {
			return kept, err
		}

		if len(verdicts) == 0 {
			if empty++; empty == maxEmptyBatches {
				r.failed("judging", "the model gave no verdicts")
				break
			}

			continue
		}

		empty = 0

		for _, v := range verdicts {
			if err := r.s.store.Judge(ctx, v.work, v.verdict, v.reason, v.subtopicID); err != nil {
				r.s.logger.Warnw("Failed to record a verdict", "work", v.work, "error", err)
				continue
			}

			r.judged++
			r.cur.Judged++

			if v.verdict == VerdictKeep {
				r.cur.Kept++
				kept++
			}
		}

		r.save(ctx)
	}

	return kept, nil
}

func channels(w Work) int {
	seen := map[string]bool{}
	for _, f := range w.Finds {
		seen[f.Channel] = true
	}

	return len(seen)
}

// readKept reads kept works into the lab, a subtopic at a time so every one
// gets its share, and a few per site. A site is trusted only once a page of it
// came through: until then it gets one read a round, and once its reads have
// all come to nothing - it refuses crawlers, or builds its pages in the
// browser - it gets none, in this run or the next.
func (r *session) readKept(ctx context.Context) {
	if r.read >= r.budget.Read || r.attempts >= maxAttemptsFactor*r.budget.Read {
		return
	}

	works, err := r.s.store.Works(ctx, r.lab.ID)
	if err != nil {
		r.failed("reading", "could not be read")
		return
	}

	sites := sitesOf(works)
	triedNow := map[string]bool{}

	allowed := func(w Work) bool {
		site := siteOf(w.URL)
		s := sites[site]

		switch {
		case s.ok > 0:
			return r.sites[site] < maxReadsPerSite
		case s.unread > 0, s.reading > 0, triedNow[site]:
			return false
		}

		return true
	}

	queues := map[int64][]Work{}

	for _, w := range works {
		if w.Verdict == VerdictKeep && w.SourceID == 0 && readable(w) && r.mayRead(w) {
			queues[w.SubtopicID] = append(queues[w.SubtopicID], w)
		}
	}

	order := make([]int64, 0, len(r.subs)+1)
	for _, sub := range r.subs {
		order = append(order, sub.ID)
	}

	order = append(order, 0)

	for _, id := range order {
		slices.SortStableFunc(queues[id], func(a, b Work) int {
			return cmp.Or(cmp.Compare(len(b.Finds), len(a.Finds)), cmp.Compare(a.ID, b.ID))
		})
	}

	for progress := true; progress && r.read < r.budget.Read; {
		progress = false

		for _, id := range order {
			if r.read >= r.budget.Read || r.attempts >= maxAttemptsFactor*r.budget.Read || ctx.Err() != nil {
				return
			}

			// A work skipped now stays in the lab's list: a later round may
			// read it, once its site has proved readable.
			q := slices.DeleteFunc(queues[id], func(w Work) bool { return !allowed(w) })

			if len(q) == 0 {
				queues[id] = q
				continue
			}

			w := q[0]
			queues[id] = q[1:]
			progress = true

			if sites[siteOf(w.URL)].ok == 0 {
				triedNow[siteOf(w.URL)] = true
			}

			if stop := r.readWork(ctx, w); stop {
				return
			}
		}
	}
}

// readWork adds the work as a lab source; stop says no more can be.
func (r *session) readWork(ctx context.Context, w Work) (stop bool) {
	src, err := r.s.labs.AddResearched(ctx, r.ownerID, r.lab.ID, labs.Researched{URL: w.URL, Reason: w.Reason})

	switch {
	case err == nil:
	case errors.Is(err, labs.ErrSourceExists):
		// The user added it already: the work is that source.
		if lab, err := r.s.labs.Get(ctx, r.ownerID, r.lab.ID); err == nil {
			for _, s := range lab.Sources {
				if s.URL == w.URL {
					_ = r.s.store.SetSource(ctx, w.ID, s.ID)
				}
			}
		}

		return false
	case errors.Is(err, labs.ErrFull), errors.Is(err, labs.ErrCrawlingDisabled):
		r.failed("reading", err.Error())
		return true
	case errors.Is(err, labs.ErrInvalid):
		return false
	default:
		r.s.logger.Warnw("Failed to read a work", "url", w.URL, "error", err)
		r.failed("reading", "a work could not be added")

		return false
	}

	if err := r.s.store.SetSource(ctx, w.ID, src.ID); err != nil {
		r.s.logger.Warnw("Failed to record a work read", "work", w.ID, "error", err)
	}

	r.read++
	r.attempts++
	r.cur.Read++
	r.sites[siteOf(w.URL)]++
	r.readNow[w.ID] = true

	return false
}

// waitForReads gives the round's reads a while to be crawled and reviewed:
// what they cite feeds the next round. A read its site kept out gives its
// budget back, for a later round to spend on a work that can be read; so does
// one that brought only text the lab had.
func (r *session) waitForReads(ctx context.Context) {
	if len(r.readNow) == 0 {
		return
	}

	defer clear(r.readNow)

	deadline := time.Now().Add(r.s.cfg.RoundWait)

	var works []Work

	for {
		var err error
		if works, err = r.s.store.Works(ctx, r.lab.ID); err != nil {
			return
		}

		reading := slices.ContainsFunc(works, func(w Work) bool { return r.readNow[w.ID] && w.Read == ReadReading })

		if !reading || !time.Now().Before(deadline) {
			break
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(r.s.cfg.Poll):
		}
	}

	last := &r.rounds[len(r.rounds)-1]

	for _, w := range works {
		switch {
		case !r.readNow[w.ID]:
		case unread(w.Read):
			r.read--
			last.Unread++
		case w.Read == ReadCopy:
			r.read--
			last.Copies++
		}
	}

	if last.Unread > 0 || last.Copies > 0 {
		r.save(ctx)
	}
}

type siteRecord struct {
	ok, unread, reading int
}

// sitesOf is how reads of each site have gone in this lab: through (indexed,
// set aside on review, or a copy - the site gave its page either way), kept
// out, or still under way.
func sitesOf(works []Work) map[string]siteRecord {
	out := map[string]siteRecord{}

	for _, w := range works {
		if w.SourceID == 0 {
			continue
		}

		site := siteOf(w.URL)
		s := out[site]

		switch {
		case w.Read == ReadIn || w.Read == ReadSetAside || w.Read == ReadCopy:
			s.ok++
		case w.Read == ReadGone:
			// One missing page says nothing of the rest of its site.
		case unread(w.Read):
			s.unread++
		case w.Read == ReadReading:
			s.reading++
		}

		out[site] = s
	}

	return out
}

// siteOf is the site an address belongs to, as one owner runs it: blogs on
// medium.com share its refusals.
func siteOf(u string) string {
	host := hostOf(u)
	if site, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return site
	}

	return host
}
