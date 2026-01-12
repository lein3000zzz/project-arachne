package sugaredworker

import (
	"web-crawler/internal/domain/config"
)

const outDir = "output/screenshots"

type ExtraTaskRes struct {
	HTMLTask []byte
}

type SugaredWorker interface {
	RestartBrowserAndLauncher() error
	PerformExtraTask(pageURL string, flags *config.ExtraTaskFlags) *ExtraTaskRes
}
