package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
	"web-crawler/internal/app"
)

func main() {
	crawlerApp := app.InitApp()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := crawlerApp.StartApp(ctx)
	if err != nil {
		log.Fatalf("failed to start crawler: %v", err)
		return
	}

	<-ctx.Done()

	log.Println("Shutting down the crawler...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := crawlerApp.StopApp(shutdownCtx); err != nil {
		log.Fatalf("failed to stop crawler gracefully: %v", err)
	}

	log.Println("Exited cleanly")
}
