package evaluation

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeDataset(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "ds.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestLoadDatasetCanonicalizesJudgedURLs(t *testing.T) {
	ds, err := LoadDataset(writeDataset(t, `
queries:
  - query: "  goroutines  "
    relevant:
      go.dev/doc/effective_go: 3
      HTTPS://Go.dev/ref/spec#Go_statements: 2
`))
	if err != nil {
		t.Fatal(err)
	}

	q := ds.Queries[0]
	if q.ID != "q1" || q.Text != "goroutines" {
		t.Errorf("query = %+v", q)
	}

	if q.Relevant["https://go.dev/doc/effective_go"] != 3 || q.Relevant["https://go.dev/ref/spec"] != 2 || len(q.Relevant) != 2 {
		t.Errorf("judgements = %v", q.Relevant)
	}
}

func TestLoadDatasetRejectsBadFiles(t *testing.T) {
	for name, body := range map[string]string{
		"no queries":     "queries: []\n",
		"unknown key":    "queries:\n  - query: x\n    relevant: {go.dev: 1}\n    grade: 2\n",
		"empty query":    "queries:\n  - query: ' '\n    relevant: {go.dev: 1}\n",
		"grade too high": "queries:\n  - query: x\n    relevant: {go.dev: 4}\n",
		"nothing found":  "queries:\n  - query: x\n    relevant: {go.dev: 0}\n",
		"not a url":      "queries:\n  - query: x\n    relevant: {'mailto:a@b': 1}\n",
		"duplicate id":   "queries:\n  - {id: a, query: x, relevant: {go.dev: 1}}\n  - {id: a, query: y, relevant: {go.dev: 1}}\n",
		"conflict":       "queries:\n  - query: x\n    relevant: {go.dev: 1, 'https://go.dev/': 3}\n",
	} {
		_, err := LoadDataset(writeDataset(t, body))

		// A misspelled key is a YAML error; everything else is ours.
		if err == nil || name != "unknown key" && !errors.Is(err, ErrInvalidDataset) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
