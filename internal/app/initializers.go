package app

import (
	"context"
	"log"
	"os"
	"web-crawler/internal/networker"
	"web-crawler/internal/networker/sugaredworker"
	"web-crawler/internal/pageparser"
	"web-crawler/internal/pages"
	"web-crawler/internal/processor"
	"web-crawler/internal/processor/queue"
	"web-crawler/internal/utils"
	"web-crawler/internal/webcrawler"
	"web-crawler/internal/webcrawler/cache"
	"web-crawler/internal/webcrawler/runstates"

	"github.com/joho/godotenv"
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

func InitApp() *CrawlerApp {
	initEnv()

	tp := initTracing()

	logger := initLogger()

	neo4jDriver := initNeo4jDriver()
	pageRepo := initPageRepo(logger, neo4jDriver)

	tasksQueue := initTasksQueue(logger)
	runsQueue := initRunsQueue(logger)

	redisRunStateClient := initRedisClient(logger, os.Getenv("REDIS_URI"), os.Getenv("REDIS_PASSWORD"), 2)

	nodeID, err := utils.GenerateID()
	if err != nil {
		logger.Fatal("Error generating node ID:", err)
	}

	runStateManager := runstates.NewRedisRunStateManager(redisRunStateClient, logger, nodeID)

	processorQueue := processor.NewTaskProcessorKafka(logger, tasksQueue, runsQueue, runStateManager)

	fetcher := networker.NewNetworker(logger)
	parser := pageparser.NewParserRepo(logger)

	redisPagesCacheClient := initRedisClient(logger, os.Getenv("REDIS_URI"), os.Getenv("REDIS_PASSWORD"), 0)
	redisRobotsCacheClient := initRedisClient(logger, os.Getenv("REDIS_URI"), os.Getenv("REDIS_PASSWORD"), 1)

	redisPagesCache := cache.NewRedisCache(redisPagesCacheClient, logger)
	redisRobotsCache := cache.NewRedisCache(redisRobotsCacheClient, logger)

	extraWorker, errRod := sugaredworker.NewExtraRodParser(logger)
	if errRod != nil {
		logger.Fatal("Error initializing extra worker parser:", errRod)
	}

	crawler := webcrawler.NewCrawlerRepo(logger, parser, fetcher, extraWorker, redisPagesCache, redisRobotsCache, runStateManager)

	return NewCrawlerApp(logger, crawler, pageRepo, processorQueue, runStateManager, tp)
}

func initRedisClient(logger *zap.SugaredLogger, uri, password string, db int) *redis.Client {
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

	if err := rdb.ConfigSet(context.Background(), "maxmemory", "512mb").Err(); err != nil {
		log.Fatalf("failed to set redis maxmemory: %v", err)
	}

	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		logger.Fatal("Failed to connect to Redis for run state:", err)
	}

	logger.Infow("Connected to Redis for run state management", "addr", uri)
	return rdb
}

func initTasksQueue(logger *zap.SugaredLogger) queue.Queue {
	addr := os.Getenv("KAFKA_ADDR")
	kafkaUser := os.Getenv("KAFKA_USERNAME")
	kafkaPassword := os.Getenv("KAFKA_PASSWORD")
	tasksConsumerGroup := os.Getenv("KAFKA_TASKS_CONSUMER_GROUP")
	tasksConsumerTopic := os.Getenv("KAFKA_TOPIC_TASKS")

	kafkaTasksCfg := queue.KafkaConfig{
		Seeds:         []string{addr},
		ConsumerGroup: tasksConsumerGroup,
		Topic:         tasksConsumerTopic,
		User:          kafkaUser,
		Password:      kafkaPassword,
	}

	tasksQueue, err := queue.NewKafkaQueue(logger, &kafkaTasksCfg)
	if err != nil {
		logger.Fatal("Error initializing tasks queue:", err)
	}

	return tasksQueue
}

func initRunsQueue(logger *zap.SugaredLogger) queue.Queue {
	addr := os.Getenv("KAFKA_ADDR")
	kafkaUser := os.Getenv("KAFKA_USERNAME")
	kafkaPassword := os.Getenv("KAFKA_PASSWORD")
	runsConsumerGroup := os.Getenv("KAFKA_RUNS_CONSUMER_GROUP")
	runsConsumerTopic := os.Getenv("KAFKA_TOPIC_RUNS")

	kafkaRunsCfg := queue.KafkaConfig{
		Seeds:         []string{addr},
		ConsumerGroup: runsConsumerGroup,
		Topic:         runsConsumerTopic,
		User:          kafkaUser,
		Password:      kafkaPassword,
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

func initNeo4jDriver() neo4j.DriverWithContext {
	neo4jURI := os.Getenv("NEO4J_URI")
	neo4jUser := os.Getenv("NEO4J_USER")
	neo4jPassword := os.Getenv("NEO4J_PASSWORD")

	neo4jDriver, err := neo4j.NewDriverWithContext(neo4jURI, neo4j.BasicAuth(neo4jUser, neo4jPassword, ""), func(config *neoconfig.Config) {
		config.MaxConnectionPoolSize = DefaultConcurrentTasksWorkers
	})

	if err != nil {
		log.Fatal("Error initializing neo4j:", err)
	}

	return neo4jDriver
}

func initTracing() *trace.TracerProvider {
	exp, err := otlptracehttp.New(context.Background(), otlptracehttp.WithEndpoint(os.Getenv("OTLP_ENDPOINT")), otlptracehttp.WithInsecure())
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
