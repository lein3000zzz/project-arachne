package appconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	return path
}

func TestDefaultIsValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("shipped defaults must validate: %v", err)
	}
}

func TestLoadOverlaysOntoDefaults(t *testing.T) {
	path := writeConfig(t, `
crawler:
  task_workers: 4
cache:
  page_ttl: 90m
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Crawler.TaskWorkers != 4 {
		t.Errorf("task_workers = %d, want 4", cfg.Crawler.TaskWorkers)
	}

	if cfg.Cache.PageTTL.Std() != 90*time.Minute {
		t.Errorf("page_ttl = %s, want 90m", cfg.Cache.PageTTL.Std())
	}

	if cfg.Crawler.RunWorkers != Default().Crawler.RunWorkers {
		t.Errorf("unset key should keep its default, got %d", cfg.Crawler.RunWorkers)
	}

	if cfg.Queue.FlushInterval != Default().Queue.FlushInterval {
		t.Errorf("unset section should keep its defaults, got %s", cfg.Queue.FlushInterval.Std())
	}
}

func TestLoadMissingFileReportsNotFound(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yml"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("defaults must still be usable when the file is absent: %v", err)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"zero workers", "crawler:\n  task_workers: 0\n"},
		{"negative workers", "crawler:\n  run_workers: -1\n"},
		{"unknown parser engine", "crawler:\n  parser_engine: goja\n"},
		{"unparseable duration", "cache:\n  page_ttl: half an hour\n"},
		{"duration as bare number", "cache:\n  page_ttl: 3600\n"},
		{"empty redis maxmemory", "cache:\n  redis_max_memory: \"\"\n"},
		{"component timeout over budget", "shutdown:\n  timeout: 5s\n  component_timeout: 10s\n"},
		{"malformed yaml", "crawler: [\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, tc.body)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestLoadAcceptsBothParserEngines(t *testing.T) {
	for _, engine := range []string{ParserEngineKatana, ParserEngineLegacy} {
		cfg, err := Load(writeConfig(t, "crawler:\n  parser_engine: "+engine+"\n"))
		if err != nil {
			t.Fatalf("engine %q: %v", engine, err)
		}

		if cfg.Crawler.ParserEngine != engine {
			t.Errorf("engine = %q, want %q", cfg.Crawler.ParserEngine, engine)
		}
	}
}

func TestPathPrefersEnvVar(t *testing.T) {
	t.Setenv(PathEnvVar, "")

	if path, explicit := Path(); path != DefaultPath || explicit {
		t.Errorf("unset: got (%q, %v), want (%q, false)", path, explicit, DefaultPath)
	}

	t.Setenv(PathEnvVar, "/etc/arachne.yml")

	if path, explicit := Path(); path != "/etc/arachne.yml" || !explicit {
		t.Errorf("set: got (%q, %v), want (/etc/arachne.yml, true)", path, explicit)
	}
}

func TestShippedConfigFileIsValid(t *testing.T) {
	cfg, err := Load("../../configs/config.yml")
	if err != nil {
		t.Fatalf("the committed configs/config.yml must load: %v", err)
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("the committed configs/config.yml must validate: %v", err)
	}
}
