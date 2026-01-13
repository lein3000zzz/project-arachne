package main

import "web-crawler/internal/app"

func main() {
	crawlerApp := app.InitApp()

	err := crawlerApp.StartApp()
	if err != nil {
		return
	}

	select {}
}
