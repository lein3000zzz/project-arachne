package searchapi

import "testing"

func TestIDsMustBeUUIDs(t *testing.T) {
	for raw, want := range map[string]string{
		"0190f3a1-7c2e-7b4d-9a6f-2d1e8c4b5a60":  "0190f3a1-7c2e-7b4d-9a6f-2d1e8c4b5a60",
		"0190F3A1-7C2E-7B4D-9A6F-2D1E8C4B5A60":  "0190f3a1-7c2e-7b4d-9a6f-2d1e8c4b5a60",
		"12":                                    "",
		"":                                      "",
		"0190f3a1-7c2e-7b4d-9a6f-2d1e8c4b5a6":   "",
		"0190f3a1-7c2e-7b4d-9a6f-2d1e8c4b5a600": "",
		"0190f3a1x7c2e-7b4d-9a6f-2d1e8c4b5a60":  "",
		"0190f3a1-7c2e-7b4d-9a6f-2d1e8c4b5a6g":  "",
		"{190f3a1-7c2e-7b4d-9a6f-2d1e8c4b5a6}":  "",
	} {
		if got, ok := parseID(raw); got != want || ok != (want != "") {
			t.Errorf("%q: %q, %v", raw, got, ok)
		}
	}

	if id, ok := optionalID(""); id != "" || !ok {
		t.Error("a left-out ID is refused")
	}
}
