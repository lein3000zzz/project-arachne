package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"web-crawler/internal/config"
	"web-crawler/internal/networker"
	"web-crawler/internal/networker/sugaredworker"
	"web-crawler/internal/pageparser"
	"web-crawler/internal/processor"
	"web-crawler/internal/webcrawler"
	"web-crawler/internal/webcrawler/cache"
)

func main() {
	// TODO добавить всё это чудо
	//defer neo4jDriver.Close(ctx)

	//defer tasksQueue.CloseQueue()
	//defer runsQueue.CloseQueue()
	//
	//go tasksQueue.StartQueueProducer()
	//go tasksQueue.StartQueueConsumer()
	//go runsQueue.StartQueueProducer()
	//go runsQueue.StartQueueConsumer()

	processorQueue := processor.NewTaskProcessorKafka(logger, tasksQueue, runsQueue)

	fetcher := networker.NewNetworker(logger)
	parser := pageparser.NewParserRepo(logger)
	redisPagesCache := cache.NewRedisCache(os.Getenv("REDIS_URI"), os.Getenv("REDIS_PASSWORD"), 0, logger)
	redisRobotsCache := cache.NewRedisCache(os.Getenv("REDIS_URI"), os.Getenv("REDIS_PASSWORD"), 1, logger)
	extraWorker, errRod := sugaredworker.NewExtraRodParser(logger)
	if errRod != nil {
		logger.Fatal("Error initializing extra worker parser:", err)
	}

	crawler := webcrawler.NewCrawlerRepo(logger, parser, fetcher, extraWorker, pageRepo, redisPagesCache, redisRobotsCache, processorQueue)

	// TODO старт кроулер
	//errCrawl := crawler.StartCrawler()
	//if errCrawl != nil {
	//	logger.Fatal("Error starting crawler:", err)
	//}

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

	// will be reworked in the future ofc, just a test version, nw
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
