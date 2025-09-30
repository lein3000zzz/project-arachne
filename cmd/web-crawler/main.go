package main

import (
	"context"
	"fmt"
	"time"
	"web-crawler/internal/cache"
	"web-crawler/internal/networker"
	"web-crawler/internal/pageparser"
	"web-crawler/internal/pages"
	"web-crawler/internal/webcrawler"

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

	//crawler := webcrawler.NewCrawlerRepo(logger)

	fetcher := networker.NewNetworker(logger)
	parser := pageparser.NewParserRepo(logger)
	redisCache := cache.NewRedisCache("localhost:6379", "", 0, logger)
	crawler := webcrawler.NewCrawlerRepo(logger, parser, fetcher, pageRepo, redisCache)

	errCrawl := crawler.StartCrawler("https://lein3000.live/", 2)
	if errCrawl != nil {
		logger.Fatal("Error starting crawler:", err)
	}

	return
	s := pages.PageData{
		URL:           "asd.com",
		Status:        500,
		Links:         []string{"https://asdik.com"},
		LastRunID:     "asdasdasd",
		LastUpdatedAt: time.Now(),
		FoundAt:       time.Now(),
		ContentType:   "text/html",
	}

	//s2 := pages.PageData{
	//	URL:           "asdik.com",
	//	Status:        500,
	//	Links:         []string{"https://asd.com/abobus"},
	//	LastRunID:     "asdasdasd",
	//	LastUpdatedAt: time.Now(),
	//	FoundAt:       time.Now(),
	//	ContentType:   "text/html",
	//}

	errPage := pageRepo.SavePage(&s)
	//errPage2 := pageRepo.SavePage(s2)
	logger.Fatal(errPage)
	//logger.Fatal(errPage2)
}
