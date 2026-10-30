package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"strings"
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
	"web-crawler/internal/runs"
	"web-crawler/internal/utils"
	"web-crawler/internal/webcrawler"
	"web-crawler/internal/webcrawler/cache"
	"web-crawler/internal/webcrawler/runstates"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lein3000zzz/vault-config-manager/pkg/manager"
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

const (
	databaseStartupTimeout = time.Minute
	graphSchemaTimeout     = 10 * time.Minute
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

	db := initDatabase(logger, sm, cfg)
	pageRepo := pages.NewPostgresRepo(logger, db)
	runStore := runs.NewStore(logger, db, cfg.RunState.SlotLease.Std())

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

	documentSink := initDocumentSink(logger, cfg, sm, redisURI, redisPassword, runStore, db)

	redisPagesCacheClient := initRedisClient(logger, redisURI, redisPassword, redisDBPageCache, cfg)
	redisRobotsCacheClient := initRedisClient(logger, redisURI, redisPassword, redisDBRobotsCache, cfg)

	redisPagesCache := cache.NewRedisCache(redisPagesCacheClient, logger)
	redisRobotsCache := cache.NewRedisCache(redisRobotsCacheClient, logger)

	extraWorker, errRod := sugaredworker.NewExtraRodParser(logger, cfg.Crawler.UserAgent)
	if errRod != nil {
		logger.Fatal("Error initializing extra worker parser:", errRod)
	}

	crawler := webcrawler.NewCrawlerRepo(logger, contentParser, fetcher, extraWorker, redisPagesCache, redisRobotsCache, runStateManager, documentSink, runStore, cfg)

	initGraphSchema(logger, pageRepo, documentSink, crawler.Excluded, cfg.Crawler.Exclude)

	return NewCrawlerApp(logger, crawler, pageRepo, processorQueue, runStateManager, runStore, tp, cfg)
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

func initDocumentSink(logger *zap.SugaredLogger, cfg appconfig.Config, sm manager.SecretManager, redisURI, redisPassword string,
	progress indexing.Progress, db *pgxpool.Pool) documents.Sink {
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

	// A nil *Reviewer in the interface would not compare equal to nil.
	if reviewer := platform.NewReviewer(logger, cfg, sm, store, db); reviewer != nil {
		chunking.Gated(reviewer)
	}

	return indexing.NewAsyncSink(logger, chunking, progress, cfg.Indexer.Workers, cfg.Indexer.QueueSize, cfg.Indexer.EnqueueTimeout.Std())
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

func initGraphSchema(logger *zap.SugaredLogger, pageRepo pages.PageRepo, sink documents.Sink, excluded func(string) bool, patterns []string) {
	ctx, cancel := context.WithTimeout(context.Background(), graphSchemaTimeout)
	defer cancel()

	merged, err := pageRepo.EnsureCanonical(ctx)
	if err != nil {
		logger.Fatal("Error re-canonicalizing stored URLs:", err)
	}

	sum := sha256.Sum256([]byte(strings.Join(patterns, "\n")))

	unwanted, err := pageRepo.EnsureExcluded(ctx, hex.EncodeToString(sum[:]), excluded)
	if err != nil {
		logger.Fatal("Error applying the exclude list to stored pages:", err)
	}

	// An empty document removes its URL from the index.
	for _, pageURL := range append(merged, unwanted...) {
		if err := sink.Submit(ctx, &documents.Document{URL: pageURL, FetchedAt: time.Now()}); err != nil {
			logger.Warnw("Failed to drop a page from the index", "url", pageURL, "err", err)
		}
	}
}

// The page repository owns the pool and closes it last, after the crawl workers
// that also record runs through it have stopped.
func initDatabase(logger *zap.SugaredLogger, sm manager.SecretManager, cfg appconfig.Config) *pgxpool.Pool {
	ctx, cancel := context.WithTimeout(context.Background(), databaseStartupTimeout)
	defer cancel()

	// A connection per saver, plus a few for run bookkeeping and startup work.
	return platform.OpenDatabase(ctx, logger, sm, int32(cfg.Crawler.SaverWorkers+3))
}
