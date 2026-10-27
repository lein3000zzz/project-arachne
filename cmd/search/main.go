package main

import (
	"context"
	"errors"
	"net/http"
	"os/signal"
	"syscall"
	"time"
	"web-crawler/internal/answer"
	"web-crawler/internal/pages"
	"web-crawler/internal/platform"
	"web-crawler/internal/retrieval"
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

	cancelStart()

	retriever, err := retrieval.New(embedder, store, pages.NewGraphStore(db), retrieval.Config{
		QueryPrefix: cfg.Embedding.QueryPrefix,
		Candidates:  cfg.Retrieval.Candidates,
		RRFK:        cfg.Retrieval.RRFK,
	})
	if err != nil {
		logger.Fatal("Error initializing retriever:", err)
	}

	var answerer searchapi.Answerer

	if chat := platform.NewChatClient(logger, cfg, sm); chat != nil {
		service, err := answer.New(retriever, chat, answer.Config{
			Sources:     cfg.Chat.ContextChunks,
			MaxTokens:   cfg.Chat.MaxTokens,
			Temperature: cfg.Chat.Temperature,
		})
		if err != nil {
			logger.Fatal("Error initializing answering:", err)
		}

		answerer = service

		logger.Infow("Answering enabled", "provider", cfg.Chat.Provider, "model", chat.Model())
	}

	handler := searchapi.NewHandler(logger, retriever, answerer, searchapi.Config{
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

	db.Close()

	if err := store.Close(shutdownCtx); err != nil {
		logger.Warnw("Failed to close the vector store", "error", err)
	}

	if err := tp.Shutdown(shutdownCtx); err != nil {
		logger.Warnw("Failed to flush traces", "error", err)
	}

	logger.Info("Exited cleanly")
}
