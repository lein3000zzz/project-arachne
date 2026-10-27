package config

import (
	"encoding/json"
	"testing"
)

func TestTaskIDIsStablePerRunAndURL(t *testing.T) {
	a := TaskID("run-1", "https://example.com/")

	if a != TaskID("run-1", "https://example.com/") {
		t.Error("the same run and URL must give the same ID")
	}

	if a == TaskID("run-2", "https://example.com/") || a == TaskID("run-1", "https://example.com/x") {
		t.Error("a different run or URL must give a different ID")
	}
}

func TestAckRunsOnceAndNeverTravels(t *testing.T) {
	calls := 0
	task := &Task{URL: "https://example.com/", Run: &Run{ID: "r"}}
	task.SetAck(func() { calls++ })

	payload, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}

	var decoded Task
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}

	decoded.Ack()
	if calls != 0 {
		t.Error("a decoded copy must not carry the original's ack")
	}

	task.Ack()
	task.Ack()
	if calls != 1 {
		t.Errorf("ack called %d times, want 1", calls)
	}
}
