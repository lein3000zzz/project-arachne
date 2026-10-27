// Command rank computes page-level and site-level PageRank over the stored
// hyperlink graph and replaces the scores search reads.
package main

import (
	"context"
	"sort"
	"time"
	"web-crawler/internal/networker"
	"web-crawler/internal/pages"
	"web-crawler/internal/platform"
	"web-crawler/internal/rank"
	"web-crawler/internal/robots"
	"web-crawler/internal/twins"

	"go.uber.org/zap"
)

const (
	timeout = 30 * time.Minute
	// Twins mostly sit on a handful of hosts; more would hammer them.
	twinWorkers = 4
)

func main() {
	platform.LoadEnv()

	logger := platform.NewLogger()
	cfg := platform.LoadConfig(logger)
	sm := platform.NewSecretManager(logger)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	pool := platform.OpenDatabase(ctx, logger, sm, 2)
	defer pool.Close()

	store := pages.NewGraphStore(pool)

	started := time.Now()

	fetcher := networker.NewNetworker(logger, cfg.Crawler.UserAgent, cfg.Crawler.FetchTimeout.Std())
	defer fetcher.Stop()

	checker := robots.NewChecker(logger, fetcher, robots.NewMemoryCache(), cfg.Crawler.RobotsAgent(), 0)

	twinStats, err := twins.Resolve(ctx, logger, store, fetcher, checker, cfg.Rank.TwinChecks, twinWorkers)
	if err != nil {
		logger.Fatal("Resolving placeholder twins:", err)
	}

	logger.Infow("Placeholder twins", "checked", twinStats.Checked, "folded", twinStats.Folded, "retryLater", twinStats.Skipped)

	graph, err := store.LoadLinkGraph(ctx)
	if err != nil {
		logger.Fatal(err)
	}

	if len(graph.PageIDs) == 0 {
		logger.Info("No pages yet, nothing to rank")
		return
	}

	opts := rank.Options{Damping: cfg.Rank.Damping, Tolerance: cfg.Rank.Tolerance, MaxIterations: cfg.Rank.MaxIterations}

	pageResult := compute(logger, "page", len(graph.PageIDs), graph.Links, opts)

	hostLinks := rank.Collapse(graph.Links, graph.HostOf)
	hostResult := compute(logger, "site", len(graph.Hosts), hostLinks, opts)

	run := pages.RankRun{
		Pages:      len(graph.PageIDs),
		Links:      len(graph.Links),
		Hosts:      len(graph.Hosts),
		HostLinks:  len(hostLinks),
		Iterations: pageResult.Iterations,
		Residual:   pageResult.Residual,
	}

	if err := store.SaveRanks(ctx, run, graph.PageIDs, pageResult.Scores, graph.Hosts, hostResult.Scores); err != nil {
		logger.Fatal("Saving ranks:", err)
	}

	logger.Infow("Ranks saved",
		"pages", run.Pages, "links", run.Links, "hosts", run.Hosts, "siteLinks", run.HostLinks,
		"took", time.Since(started).String(),
		"topSites", top(graph.Hosts, hostResult.Scores, 10))
}

func compute(logger *zap.SugaredLogger, level string, nodes int, edges []rank.Edge, opts rank.Options) rank.Result {
	graph, err := rank.NewGraph(nodes, edges)
	if err != nil {
		logger.Fatalw("Building the graph", "level", level, "error", err)
	}

	result, err := rank.PageRank(graph, opts)
	if err != nil {
		logger.Fatalw("Ranking", "level", level, "error", err)
	}

	// Scores short of convergence are still usable, only less settled.
	if !result.Converged {
		logger.Warnw("PageRank did not converge", "level", level, "iterations", result.Iterations, "residual", result.Residual)
	}

	logger.Infow("Ranked", "level", level, "nodes", graph.Nodes(), "edges", graph.Edges(),
		"iterations", result.Iterations, "residual", result.Residual)

	return result
}

func top(names []string, scores []float64, n int) []string {
	order := make([]int, len(names))
	for i := range order {
		order[i] = i
	}

	sort.Slice(order, func(a, b int) bool { return scores[order[a]] > scores[order[b]] })

	out := make([]string, 0, n)
	for _, i := range order[:min(n, len(order))] {
		out = append(out, names[i])
	}

	return out
}
