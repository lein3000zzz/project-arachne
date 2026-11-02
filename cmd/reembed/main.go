// Command reembed rebuilds the index for the current embedding model from the
// chunks stored for an earlier one, embedding them anew: after embedding.model
// changes, nothing has to be crawled, reviewed or chunked again. Pages the
// index already has are left alone, so it can be stopped and run again, and
// runs beside the crawler.
//
//	reembed [-list] [-from <collection>] [-dry-run] [-drop <collection>]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"
	"web-crawler/internal/platform"
	"web-crawler/internal/reembed"
	"web-crawler/internal/vectorstore"
)

const progressEvery = 5 * time.Second

func main() {
	list := flag.Bool("list", false, "list the collections, one per embedding model used, and stop")
	from := flag.String("from", "", "the collection to copy from; by default the only other one holding chunks")
	dryRun := flag.Bool("dry-run", false, "count what would be copied; embed and copy nothing (the new model's collection is still created, as the services would)")
	drop := flag.String("drop", "", "delete this collection and every chunk in it, then stop; never the current one")
	flag.Parse()

	platform.LoadEnv()

	logger := platform.NewLogger()
	cfg := platform.LoadConfig(logger)
	sm := platform.NewSecretManager(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	embedder := platform.NewEmbedder(logger, cfg, sm)

	dimension, err := embedder.Probe(ctx)
	if err != nil {
		logger.Fatalw("The embedding endpoint is not usable", "model", cfg.Embedding.Model, "error", err)
	}

	milvus := platform.MilvusConfig(logger, sm)
	current := vectorstore.CollectionName(cfg.VectorStore.CollectionPrefix, cfg.Embedding.Model, dimension)

	collections, err := vectorstore.Collections(ctx, milvus, cfg.VectorStore.CollectionPrefix)
	if err != nil {
		logger.Fatalw("Listing collections failed", "error", err)
	}

	switch {
	case *list:
		show(collections, current)
		return
	case *drop != "":
		if *drop == current || !known(collections, *drop) {
			fail("%s is the current collection or not one of this project's: nothing dropped", *drop)
		}

		if err := vectorstore.DropCollection(ctx, milvus, *drop); err != nil {
			logger.Fatalw("Dropping failed", "collection", *drop, "error", err)
		}

		fmt.Printf("dropped %s\n", *drop)

		return
	}

	source, err := choose(collections, current, *from)
	if err != nil {
		show(collections, current)
		fail("%v", err)
	}

	target := platform.NewVectorStore(ctx, logger, cfg, sm, dimension)
	defer target.Close(context.Background())

	milvus.Collection, milvus.Dimension = source.Name, source.Dimension

	old, err := vectorstore.NewMilvus(ctx, milvus)
	if err != nil {
		logger.Fatalw("Opening the old collection failed", "collection", source.Name, "error", err)
	}
	defer old.Close(context.Background())

	fmt.Printf("copying %s (%d chunks) into %s with %s%s\n", source.Name, source.Rows, current, embedder.Model(),
		map[bool]string{true: ", dry run", false: ""}[*dryRun])

	started, last := time.Now(), time.Now()

	stats, err := reembed.Run(ctx, logger, old, target, embedder, reembed.Config{
		Batch:  cfg.Embedding.BatchSize,
		DryRun: *dryRun,
		Progress: func(s reembed.Stats) {
			if time.Since(last) >= progressEvery {
				last = time.Now()
				fmt.Printf("  %d of %d pages: %d copied, %d already there, %d newer, %d failed\n",
					s.Copied+s.Current+s.Newer+s.Failed, s.Pages, s.Copied, s.Current, s.Newer, s.Failed)
			}
		},
	})

	verb := "copied"
	if *dryRun {
		verb = "would copy"
	}

	fmt.Printf("%s %d pages (%d chunks); %d already there, %d newer in the new index, %d failed; %.0f s\n",
		verb, stats.Copied, stats.Chunks, stats.Current, stats.Newer, stats.Failed, time.Since(started).Seconds())

	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Println("stopped; run again to go on where it left off")
		}

		fail("%v", err)
	}

	if *dryRun {
		return
	}

	// Nothing reads the old model's collection any more; held in memory it
	// only takes room from the new one.
	if err := old.Release(context.Background()); err != nil {
		logger.Warnw("Releasing the old collection failed", "collection", source.Name, "error", err)
	}

	fmt.Printf("%s is kept, unloaded from memory. Once search looks right, delete it: make reembed ARGS='-drop %s'\n", source.Name, source.Name)
}

// choose is the collection named, or else the only other one holding chunks.
func choose(collections []vectorstore.Collection, current, name string) (vectorstore.Collection, error) {
	var candidates []vectorstore.Collection

	for _, c := range collections {
		switch {
		case name != "" && c.Name == name && name != current:
			return c, nil
		case name == "" && c.Name != current && c.Rows > 0:
			candidates = append(candidates, c)
		}
	}

	switch {
	case name != "":
		return vectorstore.Collection{}, fmt.Errorf("%s is the current collection or not one of this project's", name)
	case len(candidates) == 0:
		return vectorstore.Collection{}, errors.New("nothing to copy: no other collection holds chunks")
	case len(candidates) > 1:
		return vectorstore.Collection{}, errors.New("several collections hold chunks: name one with -from")
	}

	return candidates[0], nil
}

func known(collections []vectorstore.Collection, name string) bool {
	for _, c := range collections {
		if c.Name == name {
			return true
		}
	}

	return false
}

func show(collections []vectorstore.Collection, current string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "COLLECTION\tDIMENSIONS\tCHUNKS\t")

	for _, c := range collections {
		mark := ""
		if c.Name == current {
			mark = "current"
		}

		fmt.Fprintf(w, "%s\t%d\t%d\t%s\n", c.Name, c.Dimension, c.Rows, mark)
	}

	_ = w.Flush()
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
