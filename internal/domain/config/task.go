package config

import (
	"crypto/sha256"
	"encoding/hex"
)

type Task struct {
	ID           string `json:"id,omitempty"`
	URL          string `json:"url"`
	CurrentDepth int    `json:"current_depth"`

	Run *Run `json:"run"`

	ack func()
}

type ExtraTaskFlags struct {
	ShouldScreenshot  bool `json:"should_screenshot"`
	ParseRenderedHTML bool `json:"parse_rendered_html"`
}

// TaskID is derived from the run and the URL rather than generated, so a
// redelivered task is recognised as the same task and a URL is admitted at most
// once per run.
func TaskID(runID, url string) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + url))
	return hex.EncodeToString(sum[:16])
}

func (t *Task) EnsureID() string {
	if t.ID == "" {
		t.ID = TaskID(t.Run.ID, t.URL)
	}

	return t.ID
}

func (t *Task) SetAck(ack func()) {
	t.ack = ack
}

// Ack tells the queue the task is finished and its offset may be committed.
// Until then a crash redelivers it. Harmless on a task that did not come from a
// queue, and on a second call.
func (t *Task) Ack() {
	if ack := t.ack; ack != nil {
		t.ack = nil
		ack()
	}
}
