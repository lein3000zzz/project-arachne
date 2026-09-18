// Package appconfig holds non-secret runtime tuning. Secrets stay in Vault and
// bootstrap values stay in the environment; this file is for everything else.
package appconfig

import (
	"fmt"
	"time"
)

type Config struct {
	Crawler  Crawler  `yaml:"crawler"`
	Cache    Cache    `yaml:"cache"`
	Queue    Queue    `yaml:"queue"`
	RunState RunState `yaml:"run_state"`
	Shutdown Shutdown `yaml:"shutdown"`
}

type Crawler struct {
	TaskWorkers      int      `yaml:"task_workers"`
	RunWorkers       int      `yaml:"run_workers"`
	SaverWorkers     int      `yaml:"saver_workers"`
	ParserEngine     string   `yaml:"parser_engine"`
	SaverSendTimeout Duration `yaml:"saver_send_timeout"`
	TaskBuffer       int      `yaml:"task_buffer"`
}

type Cache struct {
	PageTTL        Duration `yaml:"page_ttl"`
	RobotsTTL      Duration `yaml:"robots_ttl"`
	RedisMaxMemory string   `yaml:"redis_max_memory"`
}

type Queue struct {
	ChannelBuffer   int      `yaml:"channel_buffer"`
	RequestTimeout  Duration `yaml:"request_timeout"`
	ConsumerTimeout Duration `yaml:"consumer_timeout"`
	FlushInterval   Duration `yaml:"flush_interval"`
}

type RunState struct {
	TTL     Duration `yaml:"ttl"`
	LockTTL Duration `yaml:"lock_ttl"`
}

type Shutdown struct {
	Timeout          Duration `yaml:"timeout"`
	ComponentTimeout Duration `yaml:"component_timeout"`
}

const (
	ParserEngineKatana = "katana"
	ParserEngineLegacy = "legacy"
)

func Default() Config {
	return Config{
		Crawler: Crawler{
			TaskWorkers:      20,
			RunWorkers:       1,
			SaverWorkers:     10,
			ParserEngine:     ParserEngineKatana,
			SaverSendTimeout: Duration(3 * time.Second),
			TaskBuffer:       100,
		},
		Cache: Cache{
			PageTTL:        Duration(12 * time.Hour),
			RobotsTTL:      Duration(12 * time.Hour),
			RedisMaxMemory: "512mb",
		},
		Queue: Queue{
			ChannelBuffer:   50,
			RequestTimeout:  Duration(30 * time.Second),
			ConsumerTimeout: Duration(time.Minute),
			FlushInterval:   Duration(time.Second),
		},
		RunState: RunState{
			TTL:     Duration(24 * time.Hour),
			LockTTL: Duration(30 * time.Second),
		},
		Shutdown: Shutdown{
			Timeout:          Duration(30 * time.Second),
			ComponentTimeout: Duration(10 * time.Second),
		},
	}
}

func (c *Config) Validate() error {
	positive := []struct {
		name  string
		value int
	}{
		{"crawler.task_workers", c.Crawler.TaskWorkers},
		{"crawler.run_workers", c.Crawler.RunWorkers},
		{"crawler.saver_workers", c.Crawler.SaverWorkers},
		{"crawler.task_buffer", c.Crawler.TaskBuffer},
		{"queue.channel_buffer", c.Queue.ChannelBuffer},
	}

	for _, field := range positive {
		if field.value <= 0 {
			return fmt.Errorf("%w: %s must be > 0, got %d", ErrInvalidConfig, field.name, field.value)
		}
	}

	durations := []struct {
		name  string
		value Duration
	}{
		{"crawler.saver_send_timeout", c.Crawler.SaverSendTimeout},
		{"cache.page_ttl", c.Cache.PageTTL},
		{"cache.robots_ttl", c.Cache.RobotsTTL},
		{"queue.request_timeout", c.Queue.RequestTimeout},
		{"queue.consumer_timeout", c.Queue.ConsumerTimeout},
		{"queue.flush_interval", c.Queue.FlushInterval},
		{"run_state.ttl", c.RunState.TTL},
		{"run_state.lock_ttl", c.RunState.LockTTL},
		{"shutdown.timeout", c.Shutdown.Timeout},
		{"shutdown.component_timeout", c.Shutdown.ComponentTimeout},
	}

	for _, field := range durations {
		if field.value <= 0 {
			return fmt.Errorf("%w: %s must be > 0, got %s", ErrInvalidConfig, field.name, field.value.Std())
		}
	}

	switch c.Crawler.ParserEngine {
	case ParserEngineKatana, ParserEngineLegacy:
	default:
		return fmt.Errorf("%w: crawler.parser_engine %q must be %q or %q",
			ErrInvalidConfig, c.Crawler.ParserEngine, ParserEngineKatana, ParserEngineLegacy)
	}

	if c.Cache.RedisMaxMemory == "" {
		return fmt.Errorf("%w: cache.redis_max_memory must not be empty", ErrInvalidConfig)
	}

	// A component timeout above the overall budget cannot be honoured: StopApp
	// caps every component with the outer deadline.
	if c.Shutdown.ComponentTimeout > c.Shutdown.Timeout {
		return fmt.Errorf("%w: shutdown.component_timeout (%s) exceeds shutdown.timeout (%s)",
			ErrInvalidConfig, c.Shutdown.ComponentTimeout.Std(), c.Shutdown.Timeout.Std())
	}

	return nil
}
