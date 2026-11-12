package research

// Coverage is how much of what exists on the topic looks found.
type Coverage struct {
	Works    int `json:"works"`
	Pending  int `json:"pending"`
	Kept     int `json:"kept"`
	Maybe    int `json:"maybe"`
	Dropped  int `json:"dropped"`
	Read     int `json:"read"`
	In       int `json:"in"`
	SetAside int `json:"set_aside"`
	// Read, but kept out by their sites: blocked, empty or not fetched.
	KeptOut int `json:"kept_out"`
	// Read, and the same text as a page the lab held already.
	Copies int `json:"copies"`
	// Absent while search or citations have found too few kept works.
	Estimate  *Estimate          `json:"estimate,omitempty"`
	Subtopics []SubtopicCoverage `json:"subtopics"`
}

type SubtopicCoverage struct {
	ID       string    `json:"id"`
	Kept     int       `json:"kept"`
	In       int       `json:"in"`
	Estimate *Estimate `json:"estimate,omitempty"`
}

// Estimate compares the kept works search found with those citations found:
// the fewer found by both, the more neither has found yet. Works famous
// enough to be found both ways make it optimistic.
type Estimate struct {
	BySearch    int `json:"by_search"`
	ByCitations int `json:"by_citations"`
	ByBoth      int `json:"by_both"`
	// Found either way, and the estimated number that exist.
	Found int     `json:"found"`
	Total int     `json:"total"`
	Share float64 `json:"share"`
}

// Below this many kept works found each way, the overlap says nothing.
const minCaptures = 5

func coverageOf(subtopics []Subtopic, works []Work) Coverage {
	c := Coverage{Works: len(works), Subtopics: make([]SubtopicCoverage, len(subtopics))}

	var kept []Work

	for _, w := range works {
		switch w.Verdict {
		case VerdictPending:
			c.Pending++
		case VerdictKeep:
			c.Kept++
			kept = append(kept, w)
		case VerdictMaybe:
			c.Maybe++
		case VerdictDrop:
			c.Dropped++
		}

		if w.SourceID != "" {
			c.Read++
		}

		switch w.Read {
		case ReadIn:
			c.In++
		case ReadSetAside:
			c.SetAside++
		case ReadCopy:
			c.Copies++
		}

		if unread(w.Read) {
			c.KeptOut++
		}
	}

	c.Estimate = estimate(kept)

	for i, sub := range subtopics {
		var mine []Work

		sc := SubtopicCoverage{ID: sub.ID}

		for _, w := range kept {
			if w.SubtopicID != sub.ID {
				continue
			}

			mine = append(mine, w)
			sc.Kept++

			if w.Read == ReadIn {
				sc.In++
			}
		}

		sc.Estimate = estimate(mine)
		c.Subtopics[i] = sc
	}

	return c
}

// estimate is Chapman's form of the Lincoln-Petersen estimator, which stays
// finite when the two ways share nothing.
func estimate(kept []Work) *Estimate {
	e := Estimate{}

	for _, w := range kept {
		bySearch, byCitations := false, false

		for _, f := range w.Finds {
			switch f.Channel {
			case ChannelSearch:
				bySearch = true
			case ChannelReference, ChannelCiting:
				byCitations = true
			}
		}

		if bySearch {
			e.BySearch++
		}

		if byCitations {
			e.ByCitations++
		}

		if bySearch && byCitations {
			e.ByBoth++
		}
	}

	if e.BySearch < minCaptures || e.ByCitations < minCaptures {
		return nil
	}

	e.Found = e.BySearch + e.ByCitations - e.ByBoth
	e.Total = max(e.Found, int(float64((e.BySearch+1)*(e.ByCitations+1))/float64(e.ByBoth+1)+0.5)-1)
	e.Share = float64(e.Found) / float64(e.Total)

	return &e
}
