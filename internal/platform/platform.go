// Package platform builds what every binary in this module starts from: env,
// logger, config, Vault, tracing, and the embedding and vector store clients.
package platform

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"web-crawler/internal/appconfig"
	"web-crawler/internal/auth"
	"web-crawler/internal/database"
	"web-crawler/internal/embedding"
	"web-crawler/internal/llm"
	"web-crawler/internal/rerank"
	"web-crawler/internal/review"
	"web-crawler/internal/vectorstore"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/plugin/kotel"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/joho/godotenv"
	"github.com/lein3000zzz/vault-config-manager/pkg/manager"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.10.0"
	"go.uber.org/zap"
)

func LoadConfig(logger *zap.SugaredLogger) appconfig.Config {
	path, explicit := appconfig.Path()

	cfg, err := appconfig.Load(path)

	switch {
	case err == nil:
		logger.Infow("Loaded config", "path", path)
	case errors.Is(err, appconfig.ErrNotFound) && !explicit:
		logger.Warnw("No config file found, using defaults", "path", path)
	default:
		logger.Fatalf("Error loading config from %s: %v", path, err)
	}

	return cfg
}

func NewEmbedder(logger *zap.SugaredLogger, cfg appconfig.Config, sm manager.SecretManager) *embedding.OpenAIClient {
	baseURL, err := sm.GetSecretStringFromConfig("EMBEDDING_BASE_URL")
	if err != nil {
		logger.Fatal("EMBEDDING_BASE_URL is missing in Vault")
	}

	embedder, err := embedding.NewOpenAIClient(embedding.OpenAIConfig{
		BaseURL:        baseURL,
		APIKey:         optionalSecret(sm, "EMBEDDING_API_KEY"),
		Model:          cfg.Embedding.Model,
		Dimensions:     cfg.Embedding.Dimensions,
		BatchSize:      cfg.Embedding.BatchSize,
		RequestTimeout: cfg.Embedding.RequestTimeout.Std(),
		MaxRetries:     cfg.Embedding.MaxRetries,
	})
	if err != nil {
		logger.Fatal("Error initializing embedder:", err)
	}

	return embedder
}

func NewVectorStore(ctx context.Context, logger *zap.SugaredLogger, cfg appconfig.Config, sm manager.SecretManager, dimension int) *vectorstore.Milvus {
	addr, err := sm.GetSecretStringFromConfig("MILVUS_ADDR")
	if err != nil {
		logger.Fatal("MILVUS_ADDR is missing in Vault")
	}

	store, err := vectorstore.NewMilvus(ctx, vectorstore.MilvusConfig{
		Address:    addr,
		Token:      optionalSecret(sm, "MILVUS_TOKEN"),
		Collection: vectorstore.CollectionName(cfg.VectorStore.CollectionPrefix, cfg.Embedding.Model, dimension),
		Dimension:  dimension,
	})
	if err != nil {
		logger.Fatal("Error initializing vector store:", err)
	}

	return store
}

func optionalSecret(sm manager.SecretManager, key string) string {
	value, err := sm.GetSecretStringFromConfig(key)
	if err != nil {
		return ""
	}

	return value
}

func NewTracing(sm manager.SecretManager, serviceName string) *trace.TracerProvider {
	oltpString, err := sm.GetSecretStringFromConfig("OTLP_ENDPOINT")
	if err != nil {
		log.Fatalf("Error initializing tracing: %v", err)
	}

	exp, err := otlptracehttp.New(context.Background(), otlptracehttp.WithEndpoint(oltpString), otlptracehttp.WithInsecure())
	if err != nil {
		log.Fatalf("Error initializing jaeger: %v", err)
	}

	res, err := resource.New(context.Background(),
		resource.WithAttributes(semconv.ServiceNameKey.String(serviceName)),
	)
	if err != nil {
		log.Fatal("Error initializing otel resource:", err)
	}

	tracerProvider := trace.NewTracerProvider(
		trace.WithBatcher(exp),
		trace.WithResource(res),
	)

	otel.SetTracerProvider(tracerProvider)

	return tracerProvider
}

func NewSecretManager(logger *zap.SugaredLogger) manager.SecretManager {
	sm, err := manager.NewSecretManager(
		os.Getenv("VAULT_ADDRESS"),
		os.Getenv("VAULT_TOKEN"),
		manager.DefaultBasePathData+"main/",
		manager.DefaultBasePathMetaData+"main/",
		logger,
	)
	if err != nil {
		logger.Fatalf("Error initializing secret manager: %v", err)
	}

	keys := strings.Split(os.Getenv("VAULT_KEYS"), ",")

	sm.UnsealVault(keys)

	err = sm.ResetConfig()
	if err != nil {
		logger.Fatalw("failed to update config on start", "error", err)
	}

	return sm
}

func NewLogger() *zap.SugaredLogger {
	zapLogger, err := zap.NewProduction()
	if err != nil {
		log.Fatalf("Error initializing zap logger: %v", err)
		return nil
	}

	logger := zapLogger.Sugar()
	return logger
}

func LoadEnv() {
	if os.Getenv("APP_ENV") == "prod" {
		return
	}

	err := godotenv.Load("main.env")

	if err != nil {
		log.Fatalf("Error loading .env file")
	}
}

// OpenDatabase connects to Postgres and brings its schema up to date. Every binary
// migrates on start; the migration lock makes concurrent starts safe.
func OpenDatabase(ctx context.Context, logger *zap.SugaredLogger, sm manager.SecretManager, maxConns int32) *pgxpool.Pool {
	dsn, err := sm.GetSecretStringFromConfig("POSTGRES_DSN")
	if err != nil {
		logger.Fatal("POSTGRES_DSN is missing in Vault")
	}

	pool, err := database.Open(ctx, dsn, maxConns)
	if err != nil {
		logger.Fatal("Error connecting to postgres:", err)
	}

	if err := database.Migrate(ctx, pool); err != nil {
		logger.Fatal("Error migrating postgres:", err)
	}

	return pool
}

func NewAuthService(ctx context.Context, logger *zap.SugaredLogger, cfg appconfig.Config, pool *pgxpool.Pool) *auth.Service {
	service, err := auth.NewService(ctx, logger, auth.NewPostgresStore(pool),
		auth.NewHasher(auth.DefaultParams, cfg.Auth.HashConcurrency), auth.Config{
			SessionTTL:    cfg.Auth.SessionTTL.Std(),
			UserFailures:  cfg.Auth.UserFailures,
			IPFailures:    cfg.Auth.IPFailures,
			FailureWindow: cfg.Auth.FailureWindow.Std(),
			Lockout:       cfg.Auth.Lockout.Std(),
			MaxLockout:    cfg.Auth.MaxLockout.Std(),
		})
	if err != nil {
		logger.Fatal("Error initializing accounts:", err)
	}

	return service
}

// NewChatClient returns nil when no chat endpoint is configured: search still
// works, only answering is off.
func NewChatClient(logger *zap.SugaredLogger, cfg appconfig.Config, sm manager.SecretManager) llm.Client {
	baseURL, err := sm.GetSecretStringFromConfig("CHAT_BASE_URL")
	if err != nil || baseURL == "" {
		logger.Warn("CHAT_BASE_URL is not in Vault; answering is disabled")
		return nil
	}

	client, err := llm.New(llm.Config{
		Provider:   cfg.Chat.Provider,
		BaseURL:    baseURL,
		APIKey:     optionalSecret(sm, "CHAT_API_KEY"),
		Model:      cfg.Chat.Model,
		MaxRetries: cfg.Chat.MaxRetries,
	})
	if err != nil {
		logger.Fatal("Error initializing the chat model:", err)
	}

	return client
}

// NewReviewer returns nil when reviewing is off or no chat endpoint is
// configured: pages are then indexed unreviewed.
func NewReviewer(logger *zap.SugaredLogger, cfg appconfig.Config, sm manager.SecretManager, index review.Index, pool *pgxpool.Pool) *review.Reviewer {
	if !cfg.Review.Enabled {
		logger.Warn("Reviewing is off (review.enabled): every fetched page is indexed")
		return nil
	}

	if cfg.Review.Model != "" {
		cfg.Chat.Model = cfg.Review.Model
	}

	chat := NewChatClient(logger, cfg, sm)
	if chat == nil {
		logger.Warn("No chat model to review with: every fetched page is indexed")
		return nil
	}

	logger.Infow("Pages are reviewed before indexing", "model", chat.Model(), "maxToolCalls", cfg.Review.MaxToolCalls)

	return review.New(logger, chat, index, review.NewPostgresStore(pool), review.Config{
		MaxToolCalls: cfg.Review.MaxToolCalls,
		PartRunes:    cfg.Review.PartRunes,
		MaxTokens:    cfg.Review.MaxTokens,
		Timeout:      cfg.Review.Timeout.Std(),
	})
}

// NewReranker returns nil when no reranker endpoint is configured: search then
// runs without reranking.
func NewReranker(logger *zap.SugaredLogger, cfg appconfig.Config, sm manager.SecretManager) *rerank.Client {
	baseURL, err := sm.GetSecretStringFromConfig("RERANK_BASE_URL")
	if err != nil || baseURL == "" {
		logger.Warn("RERANK_BASE_URL is not in Vault; reranking is disabled")
		return nil
	}

	client, err := rerank.New(rerank.Config{
		BaseURL:        baseURL,
		APIKey:         optionalSecret(sm, "RERANK_API_KEY"),
		Model:          cfg.Rerank.Model,
		RequestTimeout: cfg.Rerank.RequestTimeout.Std(),
		MaxRetries:     cfg.Rerank.MaxRetries,
	})
	if err != nil {
		logger.Fatal("Error initializing the reranker:", err)
	}

	return client
}

// NewRunsProducer publishes to the runs topic, where the crawler takes runs from.
func NewRunsProducer(logger *zap.SugaredLogger, sm manager.SecretManager) *kgo.Client {
	addr, err1 := sm.GetSecretStringFromConfig("KAFKA_ADDR")
	user, err2 := sm.GetSecretStringFromConfig("KAFKA_USERNAME")
	password, err3 := sm.GetSecretStringFromConfig("KAFKA_PASSWORD")
	topic, err4 := sm.GetSecretStringFromConfig("KAFKA_TOPIC_RUNS")

	if err := errors.Join(err1, err2, err3, err4); err != nil {
		logger.Fatal("Kafka settings missing in Vault:", err)
	}

	client, err := kgo.NewClient(
		kgo.SeedBrokers(addr),
		kgo.DefaultProduceTopic(topic),
		kgo.AllowAutoTopicCreation(),
		kgo.SASL(plain.Auth{User: user, Pass: password}.AsMechanism()),
		kgo.WithHooks(kotel.NewKotel().Hooks()),
	)
	if err != nil {
		logger.Fatal("Error creating the Kafka producer:", err)
	}

	return client
}
