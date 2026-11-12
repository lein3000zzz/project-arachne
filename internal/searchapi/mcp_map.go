package searchapi

import (
	"fmt"
	"strings"
	"web-crawler/internal/research"
)

const (
	keptShown    = 60
	maybeShown   = 40
	droppedShown = 100
)

var readNames = map[string]string{
	research.ReadIn: "in the index", research.ReadReading: "being read", research.ReadSetAside: "set aside on review",
	research.ReadFailed: "could not be fetched", research.ReadBlocked: "blocked by the site", research.ReadGone: "no longer there",
	research.ReadEmpty: "no text without a browser", research.ReadCopy: "a copy of a page the lab holds", "": "not read",
}

// mapText is a lab's map as an agent reads it: per subtopic the kept works,
// how each was found and how far it is read; then the maybes, and, when asked,
// what was dropped and why.
func mapText(labName string, m research.Map, dropped bool) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Map of %s\n\n", labName)

	if m.Run == nil {
		b.WriteString("Not researched yet: the research tool starts it.\n")
		return b.String()
	}

	b.WriteString(runLine(*m.Run))

	c := m.Coverage
	fmt.Fprintf(&b, "%d works found: %d kept, %d maybe, %d dropped, %d not yet judged. %d read: %d in the index, %d set aside on review, %d kept out by their sites, %d copies of pages the lab held.\n",
		c.Works, c.Kept, c.Maybe, c.Dropped, c.Pending, c.Read, c.In, c.SetAside, c.KeptOut, c.Copies)

	if e := c.Estimate; e != nil {
		fmt.Fprintf(&b, "Coverage: search and citations together found %d of an estimated %d kept works (%.0f%%); a rough estimate, likely optimistic.\n",
			e.Found, e.Total, 100*e.Share)
	} else {
		b.WriteString("Too few works found both by search and by citations to estimate what is still unfound.\n")
	}

	titles := map[string]string{}
	for _, w := range m.Works {
		titles[w.ID] = workTitle(w)
	}

	inSubtopic := map[string]bool{}

	for i, sub := range m.Subtopics {
		inSubtopic[sub.ID] = true

		var kept []research.Work

		for _, w := range m.Works {
			if w.Verdict == research.VerdictKeep && w.SubtopicID == sub.ID {
				kept = append(kept, w)
			}
		}

		estimate := ""
		if i < len(c.Subtopics) && c.Subtopics[i].Estimate != nil {
			estimate = fmt.Sprintf(", about %.0f%% found", 100*c.Subtopics[i].Estimate.Share)
		}

		fmt.Fprintf(&b, "\n## %d. %s (subtopic %s): %d kept%s\n", i+1, sub.Name, sub.ID, len(kept), estimate)

		if sub.Focus != "" {
			fmt.Fprintf(&b, "%s\n", sub.Focus)
		}

		writeWorks(&b, kept, titles, keptShown)
	}

	var loose, maybe, gone []research.Work

	for _, w := range m.Works {
		switch {
		case w.Verdict == research.VerdictKeep && !inSubtopic[w.SubtopicID]:
			loose = append(loose, w)
		case w.Verdict == research.VerdictMaybe:
			maybe = append(maybe, w)
		case w.Verdict == research.VerdictDrop:
			gone = append(gone, w)
		}
	}

	if len(loose) > 0 {
		fmt.Fprintf(&b, "\n## Kept, in no subtopic: %d\n", len(loose))
		writeWorks(&b, loose, titles, keptShown)
	}

	if len(maybe) > 0 {
		fmt.Fprintf(&b, "\n## Maybe: %d\n", len(maybe))
		writeWorks(&b, maybe, titles, maybeShown)
	}

	if dropped && len(gone) > 0 {
		fmt.Fprintf(&b, "\n## Dropped: %d\n", len(gone))

		for i, w := range gone {
			if i == droppedShown {
				fmt.Fprintf(&b, "- and %d more\n", len(gone)-i)
				break
			}

			fmt.Fprintf(&b, "- %s (work %s): %s\n", mdLink(titles[w.ID], w.URL), w.ID, w.Reason)
		}
	}

	return b.String()
}

func runLine(r research.Run) string {
	switch r.Status {
	case research.StatusRunning:
		return fmt.Sprintf("Research is running: round %d.\n", len(r.Rounds))
	case research.StatusFailed:
		return fmt.Sprintf("The last research failed after %d rounds: %s.\n", len(r.Rounds), r.Error)
	}

	return fmt.Sprintf("Researched in %d rounds, stopped: %s.\n", len(r.Rounds), stopNames[r.Stopped])
}

var stopNames = map[string]string{
	research.StopSaturated: "new rounds found little new", research.StopBudget: "the judging budget was spent; research again to go on",
	research.StopRounds: "the rounds ran out", research.StopTime: "out of time", research.StopInterrupted: "interrupted",
	research.StopFollowed: "the work's citations were followed",
}

func writeWorks(b *strings.Builder, works []research.Work, titles map[string]string, shown int) {
	for i, w := range works {
		if i == shown {
			fmt.Fprintf(b, "- and %d more\n", len(works)-i)
			return
		}

		about := []string{"work " + w.ID, w.Kind}
		if w.Year > 0 {
			about = append(about, fmt.Sprint(w.Year))
		}

		if w.Site != "" {
			about = append(about, w.Site)
		}

		read := readNames[w.Read]
		if w.CopyOf != "" {
			read = "the same text as " + w.CopyOf + ", in the lab already"
		}

		fmt.Fprintf(b, "- %s (%s): %s; %s", mdLink(titles[w.ID], w.URL), strings.Join(about, ", "), read, foundBy(w, titles))

		if w.Reason != "" {
			fmt.Fprintf(b, ". %s", w.Reason)
		}

		if w.ByUser {
			b.WriteString(" (the user's verdict)")
		}

		b.WriteString("\n")
	}
}

// foundBy says how a work was found: its queries, and the works it was found
// through, a few of each.
func foundBy(w research.Work, titles map[string]string) string {
	var queries, citedBy, cites []string

	seed := false

	for _, f := range w.Finds {
		switch f.Channel {
		case research.ChannelSeed:
			seed = true
		case research.ChannelSearch:
			queries = appendNew(queries, "“"+f.Query+"”")
		case research.ChannelReference:
			citedBy = appendNew(citedBy, "“"+titles[f.Via]+"”")
		case research.ChannelCiting:
			cites = appendNew(cites, "“"+titles[f.Via]+"”")
		}
	}

	var parts []string

	if seed {
		parts = append(parts, "a source of the lab")
	}

	if len(queries) > 0 {
		parts = append(parts, "searched "+few(queries))
	}

	if len(citedBy) > 0 {
		parts = append(parts, "cited by "+few(citedBy))
	}

	if len(cites) > 0 {
		parts = append(parts, "cites "+few(cites))
	}

	return "found: " + strings.Join(parts, "; ")
}

func appendNew(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}

	return append(list, s)
}

func few(items []string) string {
	if len(items) <= 2 {
		return strings.Join(items, ", ")
	}

	return fmt.Sprintf("%s and %d more", strings.Join(items[:2], ", "), len(items)-2)
}

func workTitle(w research.Work) string {
	if w.Title != "" {
		return oneLine(w.Title)
	}

	return w.URL
}

func mdLink(title, url string) string {
	title = strings.NewReplacer("[", "(", "]", ")").Replace(title)
	return "[" + title + "](<" + url + ">)"
}
