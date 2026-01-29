package config

type Task struct {
	URL          string `json:"url"`
	CurrentDepth int    `json:"current_depth"`

	Run *Run `json:"run"`
}

type ExtraTaskFlags struct {
	ShouldScreenshot  bool `json:"should_screenshot"`
	ParseRenderedHTML bool `json:"parse_rendered_html"`
}
