// Command rank computes page-level and site-level PageRank over the stored
// hyperlink graph and replaces the scores search reads. With -watch it keeps
// running and ranks again whenever a crawl finishes.
package main

import (
	"context"
	"errors"
	"flag"
	"os/signal"
	"syscall"
	"time"
	"web-crawler/internal/networker"
	"web-crawler/internal/pages"
	"web-crawler/internal/platform"
	"web-crawler/internal/rank"
	"web-crawler/internal/ranking"
	"web-crawler/internal/robots"
)

const onceTimeout = 30 * time.Minute

func main() {
	watch := flag.Bool("watch", false, "keep running and rank again after every finished crawl")
	flag.Parse()

	platform.LoadEnv()

	logger := platform.NewLogger()
	cfg := platform.LoadConfig(logger)
	sm := platform.NewSecretManager(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool := platform.OpenDatabase(ctx, logger, sm, 3)
	defer pool.Close()

	fetcher := networker.NewNetworker(logger, cfg.Crawler.UserAgent, cfg.Crawler.FetchTimeout.Std())
	defer fetcher.Stop()

	ranker := ranking.New(logger, pages.NewGraphStore(pool), fetcher,
		robots.NewChecker(logger, fetcher, robots.NewMemoryCache(), cfg.Crawler.RobotsAgent(), cfg.Cache.RobotsTTL.Std()),
		ranking.Config{
			Options:    rank.Options{Damping: cfg.Rank.Damping, Tolerance: cfg.Rank.Tolerance, MaxIterations: cfg.Rank.MaxIterations},
			TwinChecks: cfg.Rank.TwinChecks,
		})

	if *watch {
		logger.Infow("Watching for finished crawls", "interval", cfg.Rank.WatchInterval.Std().String())
		ranker.Watch(ctx, cfg.Rank.WatchInterval.Std())

		return
	}

	ctx, cancel := context.WithTimeout(ctx, onceTimeout)
	defer cancel()

	if err := ranker.Run(ctx); err != nil {
		if errors.Is(err, ranking.ErrBusy) {
			logger.Info("Another ranker is running; its scores will be current when it finishes")
			return
		}

		logger.Fatal(err)
	}
}
