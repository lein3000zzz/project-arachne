package sugaredworker

import (
	"web-crawler/internal/processor"
)

const outDir = "output/screenshots"

type ExtraTaskRes struct {
	HTMLTask []byte
}

type SugaredWorker interface {
	RestartBrowserAndLauncher() error
	PerformExtraTask(pageURL string, flags *processor.ExtraTaskFlags) *ExtraTaskRes
}
