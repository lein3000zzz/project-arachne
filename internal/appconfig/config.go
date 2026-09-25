package appconfig

import (
	"fmt"
	"regexp"
	"time"
)

type Config struct {
	Crawler     Crawler     `yaml:"crawler"`
	Chunker     Chunker     `yaml:"chunker"`
	Embedding   Embedding   `yaml:"embedding"`
	VectorStore VectorStore `yaml:"vector_store"`
	Indexer     Indexer     `yaml:"indexer"`
	Cache       Cache       `yaml:"cache"`
	Queue       Queue       `yaml:"queue"`
	RunState    RunState    `yaml:"run_state"`
	Shutdown    Shutdown    `yaml:"shutdown"`
}

type Crawler struct {
	TaskWorkers      int      `yaml:"task_workers"`
	RunWorkers       int      `yaml:"run_workers"`
	SaverWorkers     int      `yaml:"saver_workers"`
	ParserEngine     string   `yaml:"parser_engine"`
	SaverSendTimeout Duration `yaml:"saver_send_timeout"`
	TaskBuffer       int      `yaml:"task_buffer"`
}

type Chunker struct {
	MaxRunes         int `yaml:"max_runes"`
	OverlapRunes     int `yaml:"overlap_runes"`
	MinDocumentRunes int `yaml:"min_document_runes"`
}

type Embedding struct {
	Model string `yaml:"model"`
	// 0 keeps the model's native size. Anything else is sent as `dimensions`,
	// which only Matryoshka-trained models can honour.
	Dimensions     int      `yaml:"dimensions"`
	BatchSize      int      `yaml:"batch_size"`
	RequestTimeout Duration `yaml:"request_timeout"`
	MaxRetries     int      `yaml:"max_retries"`
}

type VectorStore struct {
	CollectionPrefix string `yaml:"collection_prefix"`
}

type Indexer struct {
	Workers        int      `yaml:"workers"`
	QueueSize      int      `yaml:"queue_size"`
	EnqueueTimeout Duration `yaml:"enqueue_timeout"`
}

type Cache struct {
	PageTTL         Duration `yaml:"page_ttl"`
	RobotsTTL       Duration `yaml:"robots_ttl"`
	DocumentHashTTL Duration `yaml:"document_hash_ttl"`
	RedisMaxMemory  string   `yaml:"redis_max_memory"`
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

// Milvus stores text in a 65535-byte varchar alongside a title of up to 512
// runes; 12000 runes of worst-case 4-byte UTF-8 still fits with the title.
const maxChunkRunes = 12000

var collectionPrefixPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

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
		Chunker: Chunker{
			MaxRunes:         1200,
			OverlapRunes:     150,
			MinDocumentRunes: 80,
		},
		Embedding: Embedding{
			Model:          "qwen3-embedding:0.6b",
			BatchSize:      32,
			RequestTimeout: Duration(2 * time.Minute),
			MaxRetries:     4,
		},
		VectorStore: VectorStore{
			CollectionPrefix: "chunks",
		},
		Indexer: Indexer{
			Workers:        4,
			QueueSize:      64,
			EnqueueTimeout: Duration(5 * time.Second),
		},
		Cache: Cache{
			PageTTL:         Duration(12 * time.Hour),
			RobotsTTL:       Duration(12 * time.Hour),
			DocumentHashTTL: Duration(30 * 24 * time.Hour),
			RedisMaxMemory:  "512mb",
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
		{"chunker.max_runes", c.Chunker.MaxRunes},
		{"embedding.batch_size", c.Embedding.BatchSize},
		{"indexer.workers", c.Indexer.Workers},
		{"indexer.queue_size", c.Indexer.QueueSize},
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
		{"cache.document_hash_ttl", c.Cache.DocumentHashTTL},
		{"embedding.request_timeout", c.Embedding.RequestTimeout},
		{"indexer.enqueue_timeout", c.Indexer.EnqueueTimeout},
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

	if c.Chunker.MaxRunes > maxChunkRunes {
		return fmt.Errorf("%w: chunker.max_runes must be <= %d, got %d", ErrInvalidConfig, maxChunkRunes, c.Chunker.MaxRunes)
	}

	if c.Embedding.Model == "" {
		return fmt.Errorf("%w: embedding.model must not be empty", ErrInvalidConfig)
	}

	if c.Embedding.Dimensions < 0 {
		return fmt.Errorf("%w: embedding.dimensions must be >= 0, got %d", ErrInvalidConfig, c.Embedding.Dimensions)
	}

	if c.Embedding.MaxRetries < 0 {
		return fmt.Errorf("%w: embedding.max_retries must be >= 0, got %d", ErrInvalidConfig, c.Embedding.MaxRetries)
	}

	if !collectionPrefixPattern.MatchString(c.VectorStore.CollectionPrefix) {
		return fmt.Errorf("%w: vector_store.collection_prefix %q must be lowercase letters, digits and underscores, starting with a letter",
			ErrInvalidConfig, c.VectorStore.CollectionPrefix)
	}

	if c.Chunker.OverlapRunes < 0 || c.Chunker.OverlapRunes >= c.Chunker.MaxRunes {
		return fmt.Errorf("%w: chunker.overlap_runes must be in [0, %d), got %d",
			ErrInvalidConfig, c.Chunker.MaxRunes, c.Chunker.OverlapRunes)
	}

	if c.Chunker.MinDocumentRunes < 0 {
		return fmt.Errorf("%w: chunker.min_document_runes must be >= 0, got %d",
			ErrInvalidConfig, c.Chunker.MinDocumentRunes)
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
