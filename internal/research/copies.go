package research

import "web-crawler/internal/copies"

// LabPage is an indexed page of a lab, as copies are looked for.
type LabPage struct {
	SourceID string
	URL      string
	Hash     string
	Sketch   copies.Sketch
}

// markCopies turns a work read in into a copy when each of its pages holds
// the text of a page another source brought in earlier - or is that page,
// crawled already under a site the lab holds: the work added nothing the lab
// did not have. The earlier source keeps the text.
func markCopies(works []Work, pages []LabPage) {
	if len(pages) < 2 {
		return
	}

	// For each page, the page of the earliest other source with its text.
	original := make([]int, len(pages))
	for i := range original {
		original[i] = -1
	}

	earlier := func(i, j int) {
		a, b := pages[i], pages[j]
		if a.SourceID == b.SourceID {
			return
		}

		if a.SourceID > b.SourceID {
			i, j = j, i
		}

		if o := original[j]; o < 0 || pages[i].SourceID < pages[o].SourceID {
			original[j] = i
		}
	}

	byHash := map[string][]int{}
	sketches := make([]copies.Sketch, len(pages))

	for i, p := range pages {
		sketches[i] = p.Sketch

		if p.Hash != "" {
			for _, j := range byHash[p.Hash] {
				earlier(i, j)
			}

			byHash[p.Hash] = append(byHash[p.Hash], i)
		}
	}

	for _, pair := range copies.Candidates(sketches) {
		if copies.Same(sketches[pair[0]], sketches[pair[1]]) {
			earlier(pair[0], pair[1])
		}
	}

	bySource := map[string][]int{}
	for i, p := range pages {
		bySource[p.SourceID] = append(bySource[p.SourceID], i)
	}

	for i := range works {
		w := &works[i]
		if w.Read != ReadIn || len(bySource[w.SourceID]) == 0 {
			continue
		}

		of := ""

		for _, p := range bySource[w.SourceID] {
			if original[p] < 0 {
				of = ""
				break
			}

			if of == "" {
				of = pages[original[p]].URL
			}
		}

		if of != "" {
			w.Read, w.CopyOf = ReadCopy, of
		}
	}
}
