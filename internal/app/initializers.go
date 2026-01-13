package app

import (
	"log"
	"os"
	"web-crawler/internal/networker"
	"web-crawler/internal/networker/sugaredworker"
	"web-crawler/internal/pageparser"
	"web-crawler/internal/pages"
	"web-crawler/internal/processor"
	"web-crawler/internal/processor/queue"
	"web-crawler/internal/webcrawler"
	"web-crawler/internal/webcrawler/cache"

	"github.com/joho/godotenv"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	neoconfig "github.com/neo4j/neo4j-go-driver/v5/neo4j/config"
	"go.uber.org/zap"
)

func InitApp() *CrawlerApp {
	initEnv()

	logger := initLogger()

	neo4jDriver := initNeo4jDriver()
	pageRepo := initPageRepo(logger, neo4jDriver)

	tasksQueue := initTasksQueue(logger)
	runsQueue := initRunsQueue(logger)

	processorQueue := processor.NewTaskProcessorKafka(logger, tasksQueue, runsQueue)

	fetcher := networker.NewNetworker(logger)
	parser := pageparser.NewParserRepo(logger)

	redisPagesCache := cache.NewRedisCache(os.Getenv("REDIS_URI"), os.Getenv("REDIS_PASSWORD"), 0, logger)
	redisRobotsCache := cache.NewRedisCache(os.Getenv("REDIS_URI"), os.Getenv("REDIS_PASSWORD"), 1, logger)

	extraWorker, errRod := sugaredworker.NewExtraRodParser(logger)
	if errRod != nil {
		logger.Fatal("Error initializing extra worker parser:", errRod)
	}

	crawler := webcrawler.NewCrawlerRepo(logger, parser, fetcher, extraWorker, redisPagesCache, redisRobotsCache)

	return NewCrawlerApp(logger, crawler, pageRepo, processorQueue)
}

func initTasksQueue(logger *zap.SugaredLogger) queue.Queue {
	addr := os.Getenv("KAFKA_ADDR")
	kafkaUser := os.Getenv("KAFKA_USERNAME")
	kafkaPassword := os.Getenv("KAFKA_PASSWORD")
	tasksConsumerGroup := os.Getenv("KAFKA_TASKS_CONSUMER_GROUP")
	tasksConsumerTopic := os.Getenv("KAFKA_TASKS_CONSUMER_TOPIC")

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
	runsConsumerTopic := os.Getenv("KAFKA_RUNS_CONSUMER_TOPIC")

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
