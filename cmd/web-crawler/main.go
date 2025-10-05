package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"web-crawler/internal/config"
	"web-crawler/internal/networker"
	"web-crawler/internal/networker/sugaredworker"
	"web-crawler/internal/pageparser"
	"web-crawler/internal/pages"
	"web-crawler/internal/processor"
	"web-crawler/internal/processor/queue"
	"web-crawler/internal/webcrawler"
	"web-crawler/internal/webcrawler/cache"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"go.uber.org/zap"
)

func main() {
	zapLogger, err := zap.NewProduction()
	if err != nil {
		fmt.Println("Error initializing zap logger:", err)
		return
	}

	defer func(zapLogger *zap.Logger) {
		err := zapLogger.Sync()
		if err != nil {
			fmt.Println("Error syncing zap logger:", err)
		}
	}(zapLogger)

	logger := zapLogger.Sugar()

	neo4jURI := "neo4j://localhost:7687"
	neo4jUser := "neo4j"
	neo4jPassword := "testtest"
	ctx := context.Background()
	neo4jDriver, err := neo4j.NewDriverWithContext(neo4jURI, neo4j.BasicAuth(neo4jUser, neo4jPassword, ""))

	if err != nil {
		logger.Fatal("Error initializing neo4j:", err)
	}

	defer neo4jDriver.Close(ctx)

	pageRepo := pages.NewNeo4jRepo(logger, neo4jDriver)

	err = pageRepo.EnsureConnectivity()
	if err != nil {
		logger.Fatal("Error connecting to neo4j:", err)
	}

	// TODO закрывать кафку в дефере
	seeds := []string{"localhost:29092"}

	tasksQueue, errTQ := queue.NewKafkaQueue(logger, seeds, "arachne-tasks", "tasks")
	runsQueue, errRQ := queue.NewKafkaQueue(logger, seeds, "arachne-runs", "runs")

	if errTQ != nil || errRQ != nil {
		logger.Fatal("Error initializing tasks queue:", errTQ, errRQ)
	}

	defer tasksQueue.KafkaClient.Close()
	defer runsQueue.KafkaClient.Close()

	go tasksQueue.StartQueueProducer()
	go tasksQueue.StartQueueConsumer()
	go runsQueue.StartQueueProducer()
	go runsQueue.StartQueueConsumer()

	processorQueue := processor.NewTaskProcessorKafka(logger, tasksQueue, runsQueue)

	fetcher := networker.NewNetworker(logger)
	parser := pageparser.NewParserRepo(logger)
	redisPagesCache := cache.NewRedisCache("localhost:6379", "", 0, logger)
	redisRobotsCache := cache.NewRedisCache("localhost:6379", "", 1, logger)
	extraWorker, errRod := sugaredworker.NewExtraRodParser(logger)
	if errRod != nil {
		logger.Fatal("Error initializing extra worker parser:", err)
	}

	crawler := webcrawler.NewCrawlerRepo(logger, parser, fetcher, extraWorker, pageRepo, redisPagesCache, redisRobotsCache, processorQueue)

	errCrawl := crawler.StartCrawler()
	if errCrawl != nil {
		logger.Fatal("Error starting crawler:", err)
	}

	//id, _ := utils.GenerateID()

	//newRun := &processor.Run{
	//	ID: id,
	//
	//	UseCacheFlag: true,
	//	MaxDepth:     3,
	//	MaxLinks:     100,
	//	ExtraFlags:   nil,
	//
	//	StartURL: "example.com",
	//
	//	CurrentLinks: 0,
	//	ActiveTasks:  0,
	//}
	//
	//processorQueue.QueueRun(newRun)

	//time.Sleep(20 * time.Second) // оно может не совсем моментально быть обработано

	go crawler.StartRunListener()

	http.Handle("/abobus", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		val := r.FormValue("URL")
		maxDepth, _ := strconv.Atoi(r.FormValue("maxDepth"))
		maxLinks, _ := strconv.Atoi(r.FormValue("maxLinks"))
		makeScreenshot, _ := strconv.ParseBool(r.FormValue("makeScreenshot"))
		useRenderedHTML, _ := strconv.ParseBool(r.FormValue("useRenderedHTML"))

		var flags *config.ExtraTaskFlags

		if makeScreenshot || useRenderedHTML {
			flags = &config.ExtraTaskFlags{
				ShouldScreenshot:  makeScreenshot,
				ParseRenderedHTML: useRenderedHTML,
			}
		}

		s := config.NewRun(val, maxDepth, maxLinks, flags)

		crawler.Processor.QueueRun(s)
		fmt.Println(val)
		w.Write([]byte("Run queued"))
	}))

	if err := http.ListenAndServe(":28181", nil); err != nil {
		log.Fatal("HTTP server error:", err)
	}

	//return
	//s := pages.PageData{
	//	URL:           "asd.com",
	//	Status:        500,
	//	Links:         []string{"https://asdik.com"},
	//	LastRunID:     "asdasdasd",
	//	LastUpdatedAt: time.Now(),
	//	FoundAt:       time.Now(),
	//	ContentType:   "text/html",
	//}

	//s2 := pages.PageData{
	//	URL:           "asdik.com",
	//	Status:        500,
	//	Links:         []string{"https://asd.com/abobus"},
	//	LastRunID:     "asdasdasd",
	//	LastUpdatedAt: time.Now(),
	//	FoundAt:       time.Now(),
	//	ContentType:   "text/html",
	//}

	//errPage := pageRepo.SavePage(&s)
	//errPage2 := pageRepo.SavePage(s2)
	//logger.Fatal(errPage)
	//logger.Fatal(errPage2)
}
