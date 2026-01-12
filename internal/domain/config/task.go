package config

type Task struct {
	URL          string
	CurrentDepth int

	Run *Run
}

type ExtraTaskFlags struct {
	ShouldScreenshot  bool
	ParseRenderedHTML bool
}
