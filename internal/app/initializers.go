package app

import (
	"context"
	"log"
	"time"
	"web-crawler/internal/appconfig"
	"web-crawler/internal/chunker"
	"web-crawler/internal/documents"
	"web-crawler/internal/indexing"
	"web-crawler/internal/networker"
	"web-crawler/internal/networker/sugaredworker"
	"web-crawler/internal/pageparser"
	"web-crawler/internal/pages"
	"web-crawler/internal/parser"
	"web-crawler/internal/platform"
	"web-crawler/internal/processor"
	"web-crawler/internal/processor/queue"
	"web-crawler/internal/utils"
	"web-crawler/internal/webcrawler"
	"web-crawler/internal/webcrawler/cache"
	"web-crawler/internal/webcrawler/runstates"

	"github.com/lein3000zzz/vault-config-manager/pkg/manager"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	neoconfig "github.com/neo4j/neo4j-go-driver/v5/neo4j/config"
	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
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
	platform.LoadEnv()

	logger := platform.NewLogger()
	cfg := platform.LoadConfig(logger)
	sm := platform.NewSecretManager(logger)

	tp := platform.NewTracing(sm, "project-arachne")

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

	fetcher := networker.NewNetworker(logger, cfg.Crawler.UserAgent, cfg.Crawler.FetchTimeout.Std())

	contentParser := initParser(logger, cfg.Crawler.ParserEngine)

	documentSink := initDocumentSink(logger, cfg, sm, redisURI, redisPassword)

	redisPagesCacheClient := initRedisClient(logger, redisURI, redisPassword, redisDBPageCache, cfg)
	redisRobotsCacheClient := initRedisClient(logger, redisURI, redisPassword, redisDBRobotsCache, cfg)

	redisPagesCache := cache.NewRedisCache(redisPagesCacheClient, logger)
	redisRobotsCache := cache.NewRedisCache(redisRobotsCacheClient, logger)

	extraWorker, errRod := sugaredworker.NewExtraRodParser(logger, cfg.Crawler.UserAgent)
	if errRod != nil {
		logger.Fatal("Error initializing extra worker parser:", errRod)
	}

	crawler := webcrawler.NewCrawlerRepo(logger, contentParser, fetcher, extraWorker, redisPagesCache, redisRobotsCache, runStateManager, documentSink, cfg)

	initGraphSchema(logger, pageRepo, crawler, documentSink)

	return NewCrawlerApp(logger, crawler, pageRepo, processorQueue, runStateManager, tp, cfg)
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

	embedder := platform.NewEmbedder(logger, cfg, sm)

	dimension, err := embedder.Probe(ctx)
	if err != nil {
		logger.Fatalw("Embedding endpoint is not usable", "model", cfg.Embedding.Model, "error", err)
	}

	store := platform.NewVectorStore(ctx, logger, cfg, sm, dimension)

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

		ChannelBuffer:  cfg.Queue.ChannelBuffer,
		RequestTimeout: cfg.Queue.RequestTimeout.Std(),
		FlushInterval:  cfg.Queue.FlushInterval.Std(),
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

		ChannelBuffer:  cfg.Queue.ChannelBuffer,
		RequestTimeout: cfg.Queue.RequestTimeout.Std(),
		FlushInterval:  cfg.Queue.FlushInterval.Std(),
	}

	runsQueue, err := queue.NewKafkaQueue(logger, &kafkaRunsCfg)
	if err != nil {
		logger.Fatal("Error initializing runs queue:", err)
	}

	return runsQueue
}

// The first start against a graph without constraints fetches every page that has
// a scheme or trailing-slash twin, one at a time.
const graphSchemaTimeout = 15 * time.Minute

func initGraphSchema(logger *zap.SugaredLogger, pageRepo pages.PageRepo, crawler *webcrawler.CrawlerRepo, sink documents.Sink) {
	ctx, cancel := context.WithTimeout(context.Background(), graphSchemaTimeout)
	defer cancel()

	removed, err := pageRepo.EnsureSchema(ctx, crawler.ResolveIdentity)
	if err != nil {
		logger.Fatal("Error preparing the page graph:", err)
	}

	for _, pageURL := range removed {
		if err := sink.Submit(ctx, &documents.Document{URL: pageURL, FetchedAt: time.Now()}); err != nil {
			logger.Warnw("Failed to drop a merged page from the index", "url", pageURL, "err", err)
		}
	}
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
