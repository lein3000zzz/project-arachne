package sugaredworker

import (
	"context"
	"web-crawler/internal/domain/config"
)

const (
	defaultOutDir         = "output/screenshots"
	containerChromiumPath = "/usr/bin/chromium-browser"
)

type ExtraTaskRes struct {
	HTMLTask []byte
}

type SugaredWorker interface {
	RestartBrowserAndLauncher() error
	PerformExtraTask(pageURL string, flags *config.ExtraTaskFlags) *ExtraTaskRes
	Shutdown(ctx context.Context) error
}
