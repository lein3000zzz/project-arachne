// Command eval runs a judged query set through the search API in each mode and
// reports nDCG, MRR and recall per mode.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"web-crawler/internal/evaluation"
	"web-crawler/internal/retrieval"
	"web-crawler/internal/searchapi"
)

type queryReport struct {
	ID     string                `json:"id"`
	Query  string                `json:"query"`
	Mode   retrieval.Mode        `json:"mode"`
	Scores evaluation.Scores     `json:"scores"`
	Deep   evaluation.Scores     `json:"deep"`
	Ranked []string              `json:"ranked"`
	Judged evaluation.Judgements `json:"judged"`
	Missed map[string]int        `json:"missed,omitempty"`
}

func main() {
	datasetPath := flag.String("dataset", "eval/queries.yml", "judged query set")
	endpoint := flag.String("endpoint", "http://localhost:8090", "search service")
	modes := flag.String("modes", "hybrid,dense,bm25", "comma-separated modes to compare")
	k := flag.Int("k", 10, "cutoff for nDCG, MRR and recall")
	deep := flag.Int("deep", 50, "second recall cutoff: what a reranker could still reach")
	minRelevant := flag.Int("min-relevant", 1, "lowest grade counted as relevant for MRR and recall")
	out := flag.String("out", "", "write per-query results as JSON here")
	flag.Parse()

	ds, err := evaluation.LoadDataset(*datasetPath)
	if err != nil {
		log.Fatal(err)
	}

	client := &http.Client{Timeout: time.Minute}
	limit := max(*k, *deep)

	var reports []queryReport

	table := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(table, "mode\tnDCG@%d\tMRR@%d\tR@%d\tR@%d\tjudged@%d\t\n", *k, *k, *k, *deep, *k)

	for _, mode := range strings.Split(*modes, ",") {
		mode := retrieval.Mode(strings.TrimSpace(mode))

		var atK, atDeep []evaluation.Scores

		for _, q := range ds.Queries {
			ranked, err := search(client, *endpoint, q, mode, limit)
			if err != nil {
				log.Fatalf("query %s (%s): %v", q.ID, mode, err)
			}

			report := queryReport{
				ID: q.ID, Query: q.Text, Mode: mode, Ranked: ranked, Judged: q.Relevant,
				Scores: evaluation.Score(ranked, q.Relevant, *k, *minRelevant),
				Deep:   evaluation.Score(ranked, q.Relevant, *deep, *minRelevant),
				Missed: missed(ranked, q.Relevant, *deep, *minRelevant),
			}

			atK = append(atK, report.Scores)
			atDeep = append(atDeep, report.Deep)
			reports = append(reports, report)
		}

		mean, meanDeep := evaluation.Mean(atK), evaluation.Mean(atDeep)
		fmt.Fprintf(table, "%s\t%.3f\t%.3f\t%.3f\t%.3f\t%.2f\t\n", mode, mean.NDCG, mean.MRR, mean.Recall, meanDeep.Recall, mean.Judged)
	}

	_ = table.Flush()
	fmt.Printf("\n%d queries from %s\n", len(ds.Queries), *datasetPath)

	if *out != "" {
		raw, err := json.MarshalIndent(reports, "", "  ")
		if err != nil {
			log.Fatal(err)
		}

		if err := os.WriteFile(*out, raw, 0o644); err != nil {
			log.Fatal(err)
		}
	}
}

func search(client *http.Client, endpoint string, q evaluation.Query, mode retrieval.Mode, limit int) ([]string, error) {
	params := url.Values{"q": {q.Text}, "mode": {string(mode)}, "limit": {strconv.Itoa(limit)}}
	if q.Host != "" {
		params.Set("host", q.Host)
	}

	resp, err := client.Get(strings.TrimRight(endpoint, "/") + "/search?" + params.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search returned %s", resp.Status)
	}

	var body searchapi.Response
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}

	ranked := make([]string, len(body.Results))
	for i, res := range body.Results {
		ranked[i] = res.URL
	}

	return ranked, nil
}

// missed lists relevant pages outside the deep cutoff - either ranked too low or
// not in the index at all, which per-query output makes easy to tell apart.
func missed(ranked []string, judged evaluation.Judgements, deep, minRelevant int) map[string]int {
	found := make(map[string]struct{}, deep)
	for _, url := range ranked[:min(deep, len(ranked))] {
		found[url] = struct{}{}
	}

	out := make(map[string]int)

	for url, grade := range judged {
		if _, ok := found[url]; !ok && grade >= minRelevant {
			out[url] = grade
		}
	}

	return out
}
