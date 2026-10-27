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
	"web-crawler/internal/database"
	"web-crawler/internal/embedding"
	"web-crawler/internal/vectorstore"

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
