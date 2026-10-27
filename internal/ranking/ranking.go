// Package ranking recomputes the link-graph scores: resolve placeholder twins,
// load the hyperlink graph, run page- and site-level PageRank, save both.
package ranking

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
	"web-crawler/internal/pages"
	"web-crawler/internal/rank"
	"web-crawler/internal/twins"

	"go.uber.org/zap"
)

// Twins mostly sit on a handful of hosts; more would hammer them.
const twinWorkers = 4

type Config struct {
	Options    rank.Options
	TwinChecks int
}

type Ranker struct {
	logger  *zap.SugaredLogger
	store   *pages.GraphStore
	fetcher twins.Fetcher
	robots  twins.Robots
	cfg     Config
}

func New(logger *zap.SugaredLogger, store *pages.GraphStore, fetcher twins.Fetcher, robots twins.Robots, cfg Config) *Ranker {
	return &Ranker{logger: logger, store: store, fetcher: fetcher, robots: robots, cfg: cfg}
}

// Run ranks once. ErrBusy means another ranker - `make rank` beside the watcher,
// or a second watcher - is at it already.
func (r *Ranker) Run(ctx context.Context) error {
	var runErr error

	acquired, err := r.store.WithRankLock(ctx, func() error {
		runErr = r.run(ctx)
		return nil
	})
	if err != nil {
		return err
	}

	if !acquired {
		return ErrBusy
	}

	return runErr
}

// Watch ranks again whenever a crawl has finished since the last ranking,
// checking every interval until ctx ends.
func (r *Ranker) Watch(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		stale, err := r.store.RanksStale(ctx)

		switch {
		case err != nil && ctx.Err() == nil:
			r.logger.Warnw("Checking whether ranks are stale", "error", err)
		case stale:
			if err := r.Run(ctx); err != nil && !errors.Is(err, ErrBusy) && ctx.Err() == nil {
				r.logger.Errorw("Ranking failed; trying again on the next check", "error", err)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Ranker) run(ctx context.Context) error {
	started := time.Now()

	twinStats, err := twins.Resolve(ctx, r.logger, r.store, r.fetcher, r.robots, r.cfg.TwinChecks, twinWorkers)
	if err != nil {
		return fmt.Errorf("resolving placeholder twins: %w", err)
	}

	r.logger.Infow("Placeholder twins", "checked", twinStats.Checked, "folded", twinStats.Folded, "retryLater", twinStats.Skipped)

	graph, err := r.store.LoadLinkGraph(ctx)
	if err != nil {
		return err
	}

	if len(graph.PageIDs) == 0 {
		r.logger.Info("No pages yet, nothing to rank")
		return nil
	}

	pageResult, err := r.compute("page", len(graph.PageIDs), graph.Links)
	if err != nil {
		return err
	}

	hostLinks := rank.Collapse(graph.Links, graph.HostOf)

	hostResult, err := r.compute("site", len(graph.Hosts), hostLinks)
	if err != nil {
		return err
	}

	run := pages.RankRun{
		Pages:      len(graph.PageIDs),
		Links:      len(graph.Links),
		Hosts:      len(graph.Hosts),
		HostLinks:  len(hostLinks),
		Iterations: pageResult.Iterations,
		Residual:   pageResult.Residual,
	}

	if err := r.store.SaveRanks(ctx, run, graph.PageIDs, pageResult.Scores, graph.Hosts, hostResult.Scores); err != nil {
		return fmt.Errorf("saving ranks: %w", err)
	}

	r.logger.Infow("Ranks saved",
		"pages", run.Pages, "links", run.Links, "hosts", run.Hosts, "siteLinks", run.HostLinks,
		"took", time.Since(started).String(),
		"topSites", top(graph.Hosts, hostResult.Scores, 10))

	return nil
}

func (r *Ranker) compute(level string, nodes int, edges []rank.Edge) (rank.Result, error) {
	graph, err := rank.NewGraph(nodes, edges)
	if err != nil {
		return rank.Result{}, fmt.Errorf("building the %s graph: %w", level, err)
	}

	result, err := rank.PageRank(graph, r.cfg.Options)
	if err != nil {
		return rank.Result{}, fmt.Errorf("ranking %ss: %w", level, err)
	}

	// Scores short of convergence are still usable, only less settled.
	if !result.Converged {
		r.logger.Warnw("PageRank did not converge", "level", level, "iterations", result.Iterations, "residual", result.Residual)
	}

	r.logger.Infow("Ranked", "level", level, "nodes", graph.Nodes(), "edges", graph.Edges(),
		"iterations", result.Iterations, "residual", result.Residual)

	return result, nil
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
