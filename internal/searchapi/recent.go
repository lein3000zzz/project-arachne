package searchapi

import (
	"sync"
	"time"
	"web-crawler/internal/answer"
	"web-crawler/internal/utils"
)

// Enough for a day's questions of a few users; an hour to decide to keep one.
const (
	recentAnswersKept = 500
	recentAnswersFor  = time.Hour
)

// recentAnswers keeps the answers given lately, with their checks, so that
// "add to lab" can name one: a client never sends an answer's text back, or
// anyone could write into an artifact. Kept in memory - a restart forgets
// them, and asking again is the remedy.
type recentAnswers struct {
	mu    sync.Mutex
	items map[string]*recentAnswer
	order []string
	limit int
	ttl   time.Duration
}

type recentAnswer struct {
	owner  int64
	query  string
	res    answer.Result
	claims []answer.Claim
	// False while its check is still running.
	checked bool
	at      time.Time
}

func newRecentAnswers(limit int, ttl time.Duration) *recentAnswers {
	return &recentAnswers{items: map[string]*recentAnswer{}, limit: limit, ttl: ttl}
}

// add keeps an answer and returns its id; checked says no check will follow.
func (r *recentAnswers) add(owner int64, query string, res answer.Result, checked bool) string {
	id, err := utils.GenerateID()
	if err != nil {
		return ""
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.items[id] = &recentAnswer{owner: owner, query: query, res: res, checked: checked, at: time.Now()}
	r.order = append(r.order, id)

	for len(r.order) > 0 && (len(r.order) > r.limit || time.Since(r.items[r.order[0]].at) > r.ttl) {
		delete(r.items, r.order[0])
		r.order = r.order[1:]
	}

	return id
}

func (r *recentAnswers) check(id string, claims []answer.Claim) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if a, ok := r.items[id]; ok {
		a.claims, a.checked = claims, true
	}
}

// get returns the answer if it is the owner's and still kept.
func (r *recentAnswers) get(owner int64, id string) (recentAnswer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	a, ok := r.items[id]
	if !ok || a.owner != owner || time.Since(a.at) > r.ttl {
		return recentAnswer{}, false
	}

	return *a, true
}
