package main

import (
	"context"
	"errors"
	"net/http"
	"os/signal"
	"syscall"
	"time"
	"web-crawler/internal/answer"
	"web-crawler/internal/artifacts"
	"web-crawler/internal/copies"
	"web-crawler/internal/endpoints"
	"web-crawler/internal/facts"
	"web-crawler/internal/labs"
	"web-crawler/internal/llm"
	"web-crawler/internal/pages"
	"web-crawler/internal/platform"
	"web-crawler/internal/research"
	"web-crawler/internal/retrieval"
	"web-crawler/internal/review"
	"web-crawler/internal/runs"
	"web-crawler/internal/searchapi"
	"web-crawler/internal/uploads"
	"web-crawler/internal/weblookup"

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

	var (
		answerer searchapi.Answerer
		// Interfaces, left nil without a model, for reports to write with.
		reportAnswers artifacts.Answers
		reportChat    llm.Client
		chat          llm.Client
		serverModel   searchapi.ServerModel
		ownEndpoints  endpoints.Clients
	)

	// The server's model may be none: users who bring their own still get
	// answers, and the rest are told to set one.
	serverChat := platform.NewChatClient(logger, cfg, sm)
	if serverChat != nil {
		serverModel = searchapi.ServerModel{Provider: cfg.Chat.Provider, Model: serverChat.Model()}
	}

	own := platform.NewEndpoints(logger, cfg, sm, db)
	if own != nil {
		ownEndpoints = own
	}

	if serverChat != nil || own != nil {
		chat = endpoints.NewRouter(ownEndpoints, serverChat)
	}

	// The ledger digests lab pages with the server's model, never a user's.
	factStore := facts.NewPostgresStore(db)

	var digester *facts.Digester

	if cfg.Facts.Enabled && serverChat != nil {
		digester = facts.NewDigester(logger, factStore, store, facts.NewExtractor(serverChat), facts.DigestConfig{
			MaxChunks: cfg.Facts.MaxChunks,
			Every:     cfg.Facts.Every.Std(),
		}).WithRelater(facts.NewRelater(logger, factStore, embedder, serverChat, review.NewPostgresStore(db), facts.RelateConfig{
			Shortlist:     cfg.Facts.Shortlist,
			PairsPerSweep: cfg.Facts.PairsPerSweep,
		}))
	}

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

		service.ComparesPages(review.NewPostgresStore(db))

		if cfg.Facts.Enabled && cfg.Facts.Answers {
			service.ReadsFacts(facts.NewAnswerLedger(factStore, digester))
		}

		if cfg.Verify.Enabled {
			// A user's own endpoint checks their answers too; the rest get the
			// server's checking model, which may be none.
			var serverVerifier llm.Client
			if serverChat != nil {
				serverVerifier = platform.NewVerifyClient(logger, cfg, sm, serverChat)
			}

			service.Verifies(endpoints.NewRouter(ownEndpoints, serverVerifier), answer.VerifyConfig{
				MaxClaims: cfg.Verify.MaxClaims,
				MaxTokens: cfg.Verify.MaxTokens,
				Timeout:   cfg.Verify.Timeout.Std(),
				Conflicts: cfg.Verify.Conflicts,
			})

			logger.Infow("Answers are checked claim by claim", "maxClaims", cfg.Verify.MaxClaims)
		}

		answerer, reportAnswers, reportChat = service, service, chat

		logger.Infow("Answering enabled", "serverModel", serverModel.Model, "ownEndpoints", own != nil)
	}

	services := searchapi.Services{Auth: accounts, Tokens: accounts, Searcher: retriever, Answerer: answerer,
		Base: pages.NewStatsReader(db, time.Minute)}
	if own != nil {
		services.Endpoints = own
	}

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

	uploadStore := uploads.NewStore(db, func(ctx context.Context, url string) error { return store.Replace(ctx, url, nil) })

	labService := labs.NewService(labs.NewPostgresStore(db), labRuns, chat, labs.Limits{
		MaxSources:     cfg.Labs.MaxSources,
		DefaultEngines: cfg.WebSearch.DefaultEngines,
		Documents:      cfg.Documents.Enabled,
	}).KeepsUploads(uploadStore)
	services.Labs, services.Uploads = labService, uploadStore

	// Looking on the web crawls what it finds, so it needs crawling as well as
	// the engines; research can map a topic without crawling, and reads
	// nothing then. Left nil, they say they are unavailable. Interfaces, not
	// pointers: a nil pointer inside one is not nil.
	var (
		lookSearch     weblookup.Search
		lookRuns       weblookup.Runs
		researchSearch research.Search
		scholar        research.Scholar
	)

	web := platform.NewWebSearch(logger, cfg, sm)
	if web != nil {
		researchSearch, services.Engines = web, web
	}

	if web != nil && labRuns != nil {
		lookSearch, lookRuns = web, labRuns
	}

	if client := platform.NewOpenAlex(logger, cfg, sm); client != nil {
		scholar = client
	}

	if cfg.WebLookup.Enabled && lookSearch != nil {
		services.WebLookup = weblookup.New(lookSearch, lookRuns, weblookup.NewPostgresPages(db), chat, weblookup.Config{
			Engines:      cfg.WebSearch.DefaultEngines,
			Results:      cfg.WebLookup.Results,
			Read:         cfg.WebLookup.Read,
			FreshFor:     cfg.WebLookup.FreshFor.Std(),
			Timeout:      cfg.WebLookup.Timeout.Std(),
			ChooseTokens: cfg.WebLookup.ChooseTokens,
			Poll:         time.Second,
		})
	}

	researchService := research.NewService(logger, research.NewPostgresStore(db), labService, researchSearch, scholar, chat, research.Config{
		Budget:        research.Budget{Found: cfg.Research.Found, Read: cfg.Research.Read},
		MaxBudget:     research.Budget{Found: cfg.Research.MaxFound, Read: cfg.Research.MaxRead},
		Rounds:        cfg.Research.Rounds,
		JudgeBatch:    cfg.Research.JudgeBatch,
		SearchResults: cfg.Research.SearchResults,
		PaperResults:  cfg.Research.PaperResults,
		PerWork:       cfg.Research.PerWork,
		MaxTokens:     cfg.Research.MaxTokens,
		RoundWait:     cfg.Research.RoundWait.Std(),
		Timeout:       cfg.Research.Timeout.Std(),
		Poll:          5 * time.Second,
		Documents:     cfg.Documents.Enabled,
	})
	researchService.Recover(context.Background())

	if cfg.Research.Enabled {
		services.Research = researchService
	}

	artifactService := artifacts.NewService(logger, artifacts.NewPostgresStore(db), labService, reportAnswers, reportChat, artifacts.Config{
		MaxSections:   cfg.Artifacts.MaxSections,
		SectionTokens: cfg.Artifacts.SectionTokens,
		OutlineTokens: cfg.Artifacts.OutlineTokens,
		Timeout:       cfg.Artifacts.Timeout.Std(),
		Verify:        cfg.Artifacts.Verify,
	})
	artifactService.Recover(context.Background())
	services.Artifacts = artifactService

	if cfg.Facts.Enabled {
		services.Facts = facts.NewLedgerService(factStore)
	}

	handler := searchapi.NewHandler(logger, services, searchapi.Config{
		ServerModel:      serverModel,
		WebLookupTimeout: cfg.WebLookup.Timeout.Std(),
		MCP:              cfg.MCP.Enabled,
		DefaultEngines:   cfg.WebSearch.DefaultEngines,
		ListSize:         cfg.UI.ListSize,
		DefaultLimit:     cfg.Retrieval.DefaultLimit,
		MaxLimit:         cfg.Retrieval.MaxLimit,
		RequestTimeout:   cfg.Search.RequestTimeout.Std(),
		AnswerTimeout:    cfg.Chat.RequestTimeout.Std(),
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

	// Pages reviewed before sketches were kept get theirs from their chunks,
	// for the quality report to see near copies among them.
	sketched := make(chan struct{})

	go func() {
		defer close(sketched)
		copies.Backfill(ctx, logger, review.NewPostgresStore(db), store)
	}()

	if digester != nil {
		go digester.Run(ctx)

		logger.Infow("Digesting lab pages into facts", "maxChunks", cfg.Facts.MaxChunks, "model", serverChat.Model(), "answersReadFacts", cfg.Facts.Answers)
	}

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Shutdown.Timeout.Std())
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warnw("Search server did not drain in time", "error", err)
	}

	if err := artifactService.Shutdown(shutdownCtx); err != nil {
		logger.Warnw("Artifact builds did not stop in time", "error", err)
	}

	if err := researchService.Shutdown(shutdownCtx); err != nil {
		logger.Warnw("Research did not stop in time", "error", err)
	}

	<-sketched
	db.Close()

	if err := store.Close(shutdownCtx); err != nil {
		logger.Warnw("Failed to close the vector store", "error", err)
	}

	if err := tp.Shutdown(shutdownCtx); err != nil {
		logger.Warnw("Failed to flush traces", "error", err)
	}

	logger.Info("Exited cleanly")
}
