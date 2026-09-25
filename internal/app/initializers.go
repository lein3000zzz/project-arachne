package app

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"time"
	"web-crawler/internal/appconfig"
	"web-crawler/internal/chunker"
	"web-crawler/internal/documents"
	"web-crawler/internal/embedding"
	"web-crawler/internal/indexing"
	"web-crawler/internal/networker"
	"web-crawler/internal/networker/sugaredworker"
	"web-crawler/internal/pageparser"
	"web-crawler/internal/pages"
	"web-crawler/internal/parser"
	"web-crawler/internal/processor"
	"web-crawler/internal/processor/queue"
	"web-crawler/internal/utils"
	"web-crawler/internal/vectorstore"
	"web-crawler/internal/webcrawler"
	"web-crawler/internal/webcrawler/cache"
	"web-crawler/internal/webcrawler/runstates"

	"github.com/joho/godotenv"
	"github.com/lein3000zzz/vault-config-manager/pkg/manager"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	neoconfig "github.com/neo4j/neo4j-go-driver/v5/neo4j/config"
	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.10.0"
	"go.uber.org/zap"
)

const (
	redisDBPageCache      = 0
	redisDBRobotsCache    = 1
	redisDBRunState       = 2
	redisDBDocumentHashes = 3
)

// Covers the embedder probe, which may have to load the model first, and
// creating and loading the collection on a fresh Milvus.
const indexingStartupTimeout = 2 * time.Minute

func InitApp() *CrawlerApp {
	initEnv()

	logger := initLogger()
	cfg := initConfig(logger)
	sm := initSecretManager(logger)

	tp := initTracing(sm)

	neo4jDriver := initNeo4jDriver(sm, cfg)
	pageRepo := initPageRepo(logger, neo4jDriver)

	tasksQueue := initTasksQueue(logger, sm, cfg)
	runsQueue := initRunsQueue(logger, sm, cfg)

	redisURI, err1 := sm.GetSecretStringFromConfig("REDIS_URI")
	redisPassword, err2 := sm.GetSecretStringFromConfig("REDIS_PASSWORD")

	if err1 != nil || err2 != nil {
		log.Fatalf("Redis URI or password is missing")
	}

	redisRunStateClient := initRedisClient(logger, redisURI, redisPassword, redisDBRunState, cfg)

	nodeID, err := utils.GenerateID()
	if err != nil {
		logger.Fatal("Error generating node ID:", err)
	}

	runStateManager := runstates.NewRedisRunStateManager(redisRunStateClient, logger, nodeID, cfg.RunState.TTL.Std())

	processorQueue := processor.NewTaskProcessorKafka(logger, tasksQueue, runsQueue, runStateManager)

	fetcher := networker.NewNetworker(logger)

	contentParser := initParser(logger, cfg.Crawler.ParserEngine)

	documentSink := initDocumentSink(logger, cfg, sm, redisURI, redisPassword)

	redisPagesCacheClient := initRedisClient(logger, redisURI, redisPassword, redisDBPageCache, cfg)
	redisRobotsCacheClient := initRedisClient(logger, redisURI, redisPassword, redisDBRobotsCache, cfg)

	redisPagesCache := cache.NewRedisCache(redisPagesCacheClient, logger)
	redisRobotsCache := cache.NewRedisCache(redisRobotsCacheClient, logger)

	extraWorker, errRod := sugaredworker.NewExtraRodParser(logger)
	if errRod != nil {
		logger.Fatal("Error initializing extra worker parser:", errRod)
	}

	crawler := webcrawler.NewCrawlerRepo(logger, contentParser, fetcher, extraWorker, redisPagesCache, redisRobotsCache, runStateManager, documentSink, cfg)

	return NewCrawlerApp(logger, crawler, pageRepo, processorQueue, runStateManager, tp, cfg)
}

func initConfig(logger *zap.SugaredLogger) appconfig.Config {
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

func initParser(logger *zap.SugaredLogger, engine string) parser.Parser {
	logger.Infow("Selected parser engine", "engine", engine)

	// Validate() already rejected anything else.
	if engine == appconfig.ParserEngineLegacy {
		return pageparser.NewLegacyAdapter(logger)
	}

	katana, err := parser.NewKatanaParser(logger)
	if err != nil {
		logger.Fatal("Error initializing katana parser:", err)
	}

	return katana
}

func initDocumentSink(logger *zap.SugaredLogger, cfg appconfig.Config, sm manager.SecretManager, redisURI, redisPassword string) documents.Sink {
	ctx, cancel := context.WithTimeout(context.Background(), indexingStartupTimeout)
	defer cancel()

	textChunker, err := chunker.NewParagraphChunker(chunker.Settings{
		MaxRunes:         cfg.Chunker.MaxRunes,
		OverlapRunes:     cfg.Chunker.OverlapRunes,
		MinDocumentRunes: cfg.Chunker.MinDocumentRunes,
	})
	if err != nil {
		logger.Fatal("Error initializing chunker:", err)
	}

	embedder := initEmbedder(logger, cfg, sm)

	dimension, err := embedder.Probe(ctx)
	if err != nil {
		logger.Fatalw("Embedding endpoint is not usable", "model", cfg.Embedding.Model, "error", err)
	}

	store := initVectorStore(ctx, logger, cfg, sm, dimension)

	logger.Infow("Indexing ready",
		"chunker", textChunker.Fingerprint(),
		"model", embedder.Model(),
		"dimension", dimension,
		"collection", store.Collection(),
	)

	hashClient := initRedisClient(logger, redisURI, redisPassword, redisDBDocumentHashes, cfg)
	hashes := indexing.NewRedisHashStore(hashClient, cfg.Cache.DocumentHashTTL.Std())

	chunking := indexing.NewChunkingSink(logger, textChunker, hashes, indexing.NewVectorSink(embedder, store))

	return indexing.NewAsyncSink(logger, chunking, cfg.Indexer.Workers, cfg.Indexer.QueueSize, cfg.Indexer.EnqueueTimeout.Std())
}

func initEmbedder(logger *zap.SugaredLogger, cfg appconfig.Config, sm manager.SecretManager) *embedding.OpenAIClient {
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

func initVectorStore(ctx context.Context, logger *zap.SugaredLogger, cfg appconfig.Config, sm manager.SecretManager, dimension int) *vectorstore.Milvus {
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

func initRedisClient(logger *zap.SugaredLogger, uri, password string, db int, cfg appconfig.Config) *redis.Client {
	rdb := redis.NewClient(&redis.Options{
		Addr:     uri,
		Password: password,
		DB:       db,
	})

	if err := redisotel.InstrumentTracing(rdb); err != nil {
		log.Fatalf("redisotel tracing err: %v", err)
	}

	if err := redisotel.InstrumentMetrics(rdb); err != nil {
		log.Fatalf("redisotel metrics err: %v", err)
	}

	if err := rdb.ConfigSet(context.Background(), "maxmemory", cfg.Cache.RedisMaxMemory).Err(); err != nil {
		log.Fatalf("failed to set redis maxmemory: %v", err)
	}

	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		logger.Fatal("Failed to connect to Redis for run state:", err)
	}

	logger.Infow("Connected to Redis for run state management", "addr", uri)
	return rdb
}

func initTasksQueue(logger *zap.SugaredLogger, sm manager.SecretManager, cfg appconfig.Config) queue.Queue {
	addr, err1 := sm.GetSecretStringFromConfig("KAFKA_ADDR")
	kafkaUser, err2 := sm.GetSecretStringFromConfig("KAFKA_USERNAME")
	kafkaPassword, err3 := sm.GetSecretStringFromConfig("KAFKA_PASSWORD")
	tasksConsumerGroup, err4 := sm.GetSecretStringFromConfig("KAFKA_TASKS_CONSUMER_GROUP")
	tasksConsumerTopic, err5 := sm.GetSecretStringFromConfig("KAFKA_TOPIC_TASKS")

	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil {
		logger.Fatal("Error initializing tasks queue: one or more Kafka keys missing in Vault")
	}

	kafkaTasksCfg := queue.KafkaConfig{
		Seeds:         []string{addr},
		ConsumerGroup: tasksConsumerGroup,
		Topic:         tasksConsumerTopic,
		User:          kafkaUser,
		Password:      kafkaPassword,

		ChannelBuffer:   cfg.Queue.ChannelBuffer,
		RequestTimeout:  cfg.Queue.RequestTimeout.Std(),
		ConsumerTimeout: cfg.Queue.ConsumerTimeout.Std(),
		FlushInterval:   cfg.Queue.FlushInterval.Std(),
	}

	tasksQueue, err := queue.NewKafkaQueue(logger, &kafkaTasksCfg)
	if err != nil {
		logger.Fatal("Error initializing tasks queue:", err)
	}

	return tasksQueue
}

func initRunsQueue(logger *zap.SugaredLogger, sm manager.SecretManager, cfg appconfig.Config) queue.Queue {
	addr, err1 := sm.GetSecretStringFromConfig("KAFKA_ADDR")
	kafkaUser, err2 := sm.GetSecretStringFromConfig("KAFKA_USERNAME")
	kafkaPassword, err3 := sm.GetSecretStringFromConfig("KAFKA_PASSWORD")
	runsConsumerGroup, err4 := sm.GetSecretStringFromConfig("KAFKA_RUNS_CONSUMER_GROUP")
	runsConsumerTopic, err5 := sm.GetSecretStringFromConfig("KAFKA_TOPIC_RUNS")

	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil {
		logger.Fatal("Error: One or more Kafka keys missing in Vault config")
	}

	kafkaRunsCfg := queue.KafkaConfig{
		Seeds:         []string{addr},
		ConsumerGroup: runsConsumerGroup,
		Topic:         runsConsumerTopic,
		User:          kafkaUser,
		Password:      kafkaPassword,

		ChannelBuffer:   cfg.Queue.ChannelBuffer,
		RequestTimeout:  cfg.Queue.RequestTimeout.Std(),
		ConsumerTimeout: cfg.Queue.ConsumerTimeout.Std(),
		FlushInterval:   cfg.Queue.FlushInterval.Std(),
	}

	runsQueue, err := queue.NewKafkaQueue(logger, &kafkaRunsCfg)
	if err != nil {
		logger.Fatal("Error initializing runs queue:", err)
	}

	return runsQueue
}

func initPageRepo(logger *zap.SugaredLogger, neo4jDriver neo4j.DriverWithContext) pages.PageRepo {
	pageRepo := pages.NewNeo4jRepo(logger, neo4jDriver)

	err := pageRepo.EnsureConnectivity()
	if err != nil {
		logger.Fatal("Error connecting to neo4j:", err)
	}

	return pageRepo
}

func initNeo4jDriver(sm manager.SecretManager, cfg appconfig.Config) neo4j.DriverWithContext {
	neo4jURI, err1 := sm.GetSecretStringFromConfig("NEO4J_URI")
	neo4jUser, err2 := sm.GetSecretStringFromConfig("NEO4J_USER")
	neo4jPassword, err3 := sm.GetSecretStringFromConfig("NEO4J_PASSWORD")

	if err1 != nil || err2 != nil || err3 != nil {
		log.Fatalf("Error initializing neo4j driver, some key is not found")
	}

	neo4jDriver, err := neo4j.NewDriverWithContext(neo4jURI, neo4j.BasicAuth(neo4jUser, neo4jPassword, ""), func(config *neoconfig.Config) {
		config.MaxConnectionPoolSize = cfg.Crawler.TaskWorkers
	})

	if err != nil {
		log.Fatal("Error initializing neo4j:", err)
	}

	return neo4jDriver
}

func initTracing(sm manager.SecretManager) *trace.TracerProvider {
	oltpString, err := sm.GetSecretStringFromConfig("OTLP_ENDPOINT")
	if err != nil {
		log.Fatalf("Error initializing tracing: %v", err)
	}

	exp, err := otlptracehttp.New(context.Background(), otlptracehttp.WithEndpoint(oltpString), otlptracehttp.WithInsecure())
	if err != nil {
		log.Fatalf("Error initializing jaeger: %v", err)
	}

	res, err := resource.New(context.Background(),
		resource.WithAttributes(semconv.ServiceNameKey.String("project-arachne")),
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

func initSecretManager(logger *zap.SugaredLogger) manager.SecretManager {
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

func initLogger() *zap.SugaredLogger {
	zapLogger, err := zap.NewProduction()
	if err != nil {
		log.Fatalf("Error initializing zap logger: %v", err)
		return nil
	}

	logger := zapLogger.Sugar()
	return logger
}

func initEnv() {
	if os.Getenv("APP_ENV") == "prod" {
		return
	}

	err := godotenv.Load("main.env")

	if err != nil {
		log.Fatalf("Error loading .env file")
	}
}
