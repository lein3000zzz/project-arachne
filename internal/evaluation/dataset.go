package evaluation

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"web-crawler/internal/urlnorm"

	"gopkg.in/yaml.v3"
)

const maxGrade = 3

type Query struct {
	ID   string `yaml:"id"`
	Text string `yaml:"query"`
	// Optional: evaluates the query within one site.
	Host     string     `yaml:"host"`
	Relevant Judgements `yaml:"relevant"`
}

type Dataset struct {
	Queries []Query `yaml:"queries"`
}

// LoadDataset reads and checks a judgement file. Judged URLs are canonicalized
// like everything the crawler stores, so they may be written as typed:
// "go.dev/doc" matches the stored "https://go.dev/doc".
func LoadDataset(path string) (Dataset, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Dataset{}, err
	}

	var ds Dataset

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	if err := dec.Decode(&ds); err != nil && !errors.Is(err, io.EOF) {
		return Dataset{}, fmt.Errorf("parsing %s: %w", path, err)
	}

	if len(ds.Queries) == 0 {
		return Dataset{}, fmt.Errorf("%w: %s has no queries", ErrInvalidDataset, path)
	}

	ids := make(map[string]struct{}, len(ds.Queries))

	for i := range ds.Queries {
		q := &ds.Queries[i]
		q.Text = strings.TrimSpace(q.Text)

		if q.ID == "" {
			q.ID = fmt.Sprintf("q%d", i+1)
		}

		if _, dup := ids[q.ID]; dup {
			return Dataset{}, fmt.Errorf("%w: duplicate query id %q", ErrInvalidDataset, q.ID)
		}

		ids[q.ID] = struct{}{}

		if q.Text == "" {
			return Dataset{}, fmt.Errorf("%w: query %s is empty", ErrInvalidDataset, q.ID)
		}

		canonical := make(Judgements, len(q.Relevant))
		hasRelevant := false

		for raw, grade := range q.Relevant {
			url, ok := urlnorm.FromUserInput(raw)
			if !ok {
				return Dataset{}, fmt.Errorf("%w: query %s: %q is not a URL", ErrInvalidDataset, q.ID, raw)
			}

			if grade < 0 || grade > maxGrade {
				return Dataset{}, fmt.Errorf("%w: query %s: grade %d for %s is outside 0-%d", ErrInvalidDataset, q.ID, grade, raw, maxGrade)
			}

			if prev, dup := canonical[url]; dup && prev != grade {
				return Dataset{}, fmt.Errorf("%w: query %s: %s is judged twice, %d and %d", ErrInvalidDataset, q.ID, url, prev, grade)
			}

			canonical[url] = grade
			hasRelevant = hasRelevant || grade > 0
		}

		// Every metric of such a query is 0 whatever the ranking, which only
		// drags the mean down.
		if !hasRelevant {
			return Dataset{}, fmt.Errorf("%w: query %s has no relevant page", ErrInvalidDataset, q.ID)
		}

		q.Relevant = canonical
	}

	return ds, nil
}
