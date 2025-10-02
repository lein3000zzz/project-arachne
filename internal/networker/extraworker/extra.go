package extraworker

const outDir = "output/screenshots"

type ExtraTaskFlags struct {
	shouldScreenshot bool
	shouldHTML       bool
}

type ExtraTaskRes struct {
	HTMLTask string
}

type ExtraWorker interface {
	RestartBrowserAndLauncher() error
	PerformExtraTask(pageURL string, flags ExtraTaskFlags) *ExtraTaskRes
}
