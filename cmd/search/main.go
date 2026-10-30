package main

import (
	"context"
	"errors"
	"net/http"
	"os/signal"
	"syscall"
	"time"
	"web-crawler/internal/answer"
	"web-crawler/internal/gathering"
	"web-crawler/internal/labs"
	"web-crawler/internal/pages"
	"web-crawler/internal/platform"
	"web-crawler/internal/retrieval"
	"web-crawler/internal/runs"
	"web-crawler/internal/searchapi"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	startupTimeout = 2 * time.Minute
	// One short rank lookup per request.
	searchDBConns = 8
)

func main() {
	platform.LoadEnv()

	logger := platform.NewLogger()
	cfg := platform.LoadConfig(logger)
	sm := platform.NewSecretManager(logger)
	tp := platform.NewTracing(sm, "project-arachne-search")

	startCtx, cancelStart := context.WithTimeout(context.Background(), startupTimeout)

	embedder := platform.NewEmbedder(logger, cfg, sm)

	// The query vectors must match the stored ones, and the collection name says
	// which those are.
	dimension, err := embedder.Probe(startCtx)
	if err != nil {
		logger.Fatalw("Embedding endpoint is not usable", "model", cfg.Embedding.Model, "error", err)
	}

	store := platform.NewVectorStore(startCtx, logger, cfg, sm, dimension)

	db := platform.OpenDatabase(startCtx, logger, sm, searchDBConns)

	accounts := platform.NewAuthService(startCtx, logger, cfg, db)

	if n, err := accounts.UserCount(startCtx); err == nil && n == 0 {
		logger.Warn("No accounts yet, so nobody can sign in: create one with make user NAME=<name> ADMIN=1")
	}

	cancelStart()

	var reranker retrieval.Reranker

	if client := platform.NewReranker(logger, cfg, sm); client != nil {
		reranker = client

		logger.Infow("Reranking enabled", "model", client.Model(), "byDefault", cfg.Rerank.Enabled, "candidates", cfg.Rerank.Candidates)
	}

	retriever, err := retrieval.New(embedder, store, pages.NewGraphStore(db), reranker, retrieval.Config{
		QueryPrefix:      cfg.Embedding.QueryPrefix,
		Candidates:       cfg.Retrieval.Candidates,
		RRFK:             cfg.Retrieval.RRFK,
		PageRankWeight:   cfg.Retrieval.PageRankWeight,
		SiteRankWeight:   cfg.Retrieval.SiteRankWeight,
		RerankByDefault:  cfg.Rerank.Enabled,
		RerankCandidates: cfg.Rerank.Candidates,
	})
	if err != nil {
		logger.Fatal("Error initializing retriever:", err)
	}

	var answerer searchapi.Answerer

	chat := platform.NewChatClient(logger, cfg, sm)

	if chat != nil {
		service, err := answer.New(retriever, chat, answer.Config{
			Sources:       cfg.Chat.ContextChunks,
			SourceRunes:   cfg.Chat.SourceRunes,
			MinRelevance:  cfg.Chat.MinRelevance,
			MinLabSources: cfg.Labs.MinSources,
			MaxTokens:     cfg.Chat.MaxTokens,
			Temperature:   cfg.Chat.Temperature,
		})
		if err != nil {
			logger.Fatal("Error initializing answering:", err)
		}

		answerer = service

		logger.Infow("Answering enabled", "provider", cfg.Chat.Provider, "model", chat.Model())
	}

	services := searchapi.Services{Auth: accounts, Searcher: retriever, Answerer: answerer}

	// Interfaces, left nil when off: a nil *runs.Service inside one would not
	// compare equal to nil.
	var labRuns labs.Runs

	if cfg.CrawlAPI.Enabled {
		producer := platform.NewRunsProducer(logger, sm)
		defer producer.Close()

		runService := runs.NewService(runs.NewStore(logger, db, cfg.RunState.SlotLease.Std()), producer, runs.Limits{
			DefaultDepth: cfg.CrawlAPI.DefaultDepth,
			MaxDepth:     cfg.CrawlAPI.MaxDepth,
			DefaultLinks: cfg.CrawlAPI.DefaultLinks,
			MaxLinks:     cfg.CrawlAPI.MaxLinks,
		})
		services.Runs, labRuns = runService, runService

		logger.Infow("Crawl submission enabled", "maxDepth", cfg.CrawlAPI.MaxDepth, "maxLinks", cfg.CrawlAPI.MaxLinks)
	}

	labService := labs.NewService(labs.NewPostgresStore(db), labRuns, chat, labs.Limits{
		MaxSources:     cfg.Labs.MaxSources,
		DefaultEngines: cfg.WebSearch.DefaultEngines,
	})
	services.Labs = labService

	// Pages a gathering finds are crawled, so it needs crawling as well as the
	// engines and the model; a nil search makes it report itself unavailable.
	var search gathering.Search
	if web := platform.NewWebSearch(logger, cfg, sm); web != nil && labRuns != nil {
		search = web
	}

	gatherer := gathering.NewService(logger, gathering.NewPostgresStore(db), labService, search, chat, gathering.Config{
		Searches:       cfg.Gathering.Searches,
		Sources:        cfg.Gathering.Sources,
		Results:        cfg.Gathering.Results,
		MaxTokens:      cfg.Gathering.MaxTokens,
		Timeout:        cfg.Gathering.Timeout.Std(),
		DefaultEngines: cfg.WebSearch.DefaultEngines,
	})
	gatherer.Recover(context.Background())
	services.Gatherer = gatherer

	handler := searchapi.NewHandler(logger, services, searchapi.Config{
		DefaultLimit:   cfg.Retrieval.DefaultLimit,
		MaxLimit:       cfg.Retrieval.MaxLimit,
		RequestTimeout: cfg.Search.RequestTimeout.Std(),
		AnswerTimeout:  cfg.Chat.RequestTimeout.Std(),
	})

	server := &http.Server{
		Addr:              cfg.Search.Listen,
		Handler:           otelhttp.NewHandler(handler, "search"),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Infow("Search listening", "addr", cfg.Search.Listen, "collection", store.Collection(), "model", embedder.Model())

		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal("Search server failed:", err)
		}
	}()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Shutdown.Timeout.Std())
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warnw("Search server did not drain in time", "error", err)
	}

	if err := gatherer.Shutdown(shutdownCtx); err != nil {
		logger.Warnw("Gatherings did not stop in time", "error", err)
	}

	db.Close()

	if err := store.Close(shutdownCtx); err != nil {
		logger.Warnw("Failed to close the vector store", "error", err)
	}

	if err := tp.Shutdown(shutdownCtx); err != nil {
		logger.Warnw("Failed to flush traces", "error", err)
	}

	logger.Info("Exited cleanly")
}
