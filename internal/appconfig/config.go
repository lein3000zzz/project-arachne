package appconfig

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

type Config struct {
	Crawler     Crawler     `yaml:"crawler"`
	Chunker     Chunker     `yaml:"chunker"`
	Embedding   Embedding   `yaml:"embedding"`
	VectorStore VectorStore `yaml:"vector_store"`
	Indexer     Indexer     `yaml:"indexer"`
	Retrieval   Retrieval   `yaml:"retrieval"`
	Search      Search      `yaml:"search"`
	Rank        Rank        `yaml:"rank"`
	Chat        Chat        `yaml:"chat"`
	Cache       Cache       `yaml:"cache"`
	Queue       Queue       `yaml:"queue"`
	RunState    RunState    `yaml:"run_state"`
	Shutdown    Shutdown    `yaml:"shutdown"`
}

type Crawler struct {
	UserAgent        string   `yaml:"user_agent"`
	TaskWorkers      int      `yaml:"task_workers"`
	RunWorkers       int      `yaml:"run_workers"`
	SaverWorkers     int      `yaml:"saver_workers"`
	ParserEngine     string   `yaml:"parser_engine"`
	SaverSendTimeout Duration `yaml:"saver_send_timeout"`
	FetchTimeout     Duration `yaml:"fetch_timeout"`
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
	// Prepended to queries, never to documents. Instruction-tuned embedders such
	// as Qwen3 retrieve markedly worse without it; plain ones need it empty.
	QueryPrefix string `yaml:"query_prefix"`
}

type VectorStore struct {
	CollectionPrefix string `yaml:"collection_prefix"`
}

type Indexer struct {
	Workers        int      `yaml:"workers"`
	QueueSize      int      `yaml:"queue_size"`
	EnqueueTimeout Duration `yaml:"enqueue_timeout"`
}

type Retrieval struct {
	// Chunks taken from each of the dense and BM25 lists before they are folded
	// into pages and fused. Pages with many matching chunks use up several.
	Candidates   int `yaml:"candidates"`
	RRFK         int `yaml:"rrf_k"`
	DefaultLimit int `yaml:"default_limit"`
	MaxLimit     int `yaml:"max_limit"`
}

type Search struct {
	Listen         string   `yaml:"listen"`
	RequestTimeout Duration `yaml:"request_timeout"`
}

type Rank struct {
	Damping       float64 `yaml:"damping"`
	Tolerance     float64 `yaml:"tolerance"`
	MaxIterations int     `yaml:"max_iterations"`
	// Placeholder twins asked about per run before ranking; each is one fetch.
	TwinChecks int `yaml:"twin_checks"`
}

type Chat struct {
	// openai-compatible (Ollama, vLLM, OpenRouter…), openai, or anthropic. The
	// endpoint and key are in Vault.
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	MaxTokens int    `yaml:"max_tokens"`
	// Unset sends none: some models accept only their default.
	Temperature    *float64 `yaml:"temperature"`
	RequestTimeout Duration `yaml:"request_timeout"`
	MaxRetries     int      `yaml:"max_retries"`
	// Search results handed to the model as sources, one excerpt each.
	ContextChunks int `yaml:"context_chunks"`
}

type Cache struct {
	PageTTL         Duration `yaml:"page_ttl"`
	RobotsTTL       Duration `yaml:"robots_ttl"`
	DocumentHashTTL Duration `yaml:"document_hash_ttl"`
	RedisMaxMemory  string   `yaml:"redis_max_memory"`
}

type Queue struct {
	ChannelBuffer  int      `yaml:"channel_buffer"`
	RequestTimeout Duration `yaml:"request_timeout"`
	FlushInterval  Duration `yaml:"flush_interval"`
}

type RunState struct {
	TTL Duration `yaml:"ttl"`
	// A run that makes no progress for this long loses its slot. Any task starting
	// or finishing renews it, on whichever node.
	SlotLease Duration `yaml:"slot_lease"`
}

type Shutdown struct {
	Timeout          Duration `yaml:"timeout"`
	ComponentTimeout Duration `yaml:"component_timeout"`
}

// The instruction the Qwen3 embedding models were trained with for web search.
const Qwen3QueryPrefix = "Instruct: Given a web search query, retrieve relevant passages that answer the query\nQuery: "

const (
	ChatProviderOpenAICompatible = "openai-compatible"
	ChatProviderOpenAI           = "openai"
	ChatProviderAnthropic        = "anthropic"
)

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
			UserAgent:        "project-arachne/0.1 (+https://github.com/lein3000zzz/project-arachne)",
			TaskWorkers:      20,
			RunWorkers:       1,
			SaverWorkers:     10,
			ParserEngine:     ParserEngineKatana,
			SaverSendTimeout: Duration(3 * time.Second),
			FetchTimeout:     Duration(30 * time.Second),
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
			QueryPrefix:    Qwen3QueryPrefix,
		},
		VectorStore: VectorStore{
			CollectionPrefix: "chunks",
		},
		Indexer: Indexer{
			Workers:        4,
			QueueSize:      64,
			EnqueueTimeout: Duration(5 * time.Second),
		},
		Retrieval: Retrieval{
			Candidates:   100,
			RRFK:         60,
			DefaultLimit: 10,
			MaxLimit:     100,
		},
		Search: Search{
			Listen:         ":8090",
			RequestTimeout: Duration(30 * time.Second),
		},
		Rank: Rank{
			Damping:       0.85,
			Tolerance:     1e-9,
			MaxIterations: 100,
			TwinChecks:    500,
		},
		Chat: Chat{
			Provider:       ChatProviderOpenAICompatible,
			Model:          "qwen3:4b-instruct",
			MaxTokens:      1024,
			Temperature:    ptr(0.2),
			RequestTimeout: Duration(5 * time.Minute),
			MaxRetries:     2,
			ContextChunks:  6,
		},
		Cache: Cache{
			PageTTL:         Duration(12 * time.Hour),
			RobotsTTL:       Duration(12 * time.Hour),
			DocumentHashTTL: Duration(30 * 24 * time.Hour),
			RedisMaxMemory:  "512mb",
		},
		Queue: Queue{
			ChannelBuffer:  50,
			RequestTimeout: Duration(30 * time.Second),
			FlushInterval:  Duration(time.Second),
		},
		RunState: RunState{
			TTL:       Duration(24 * time.Hour),
			SlotLease: Duration(10 * time.Minute),
		},
		Shutdown: Shutdown{
			Timeout:          Duration(30 * time.Second),
			ComponentTimeout: Duration(10 * time.Second),
		},
	}
}

// RobotsAgent is the product name robots.txt rules are matched against: the
// leading letters, '-' and '_' of the User-Agent, as Google's matcher extracts it.
// The matcher ignores anything that is not such a bare token.
func (c Crawler) RobotsAgent() string {
	end := strings.IndexFunc(c.UserAgent, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-' || r == '_')
	})
	if end < 0 {
		return c.UserAgent
	}

	return c.UserAgent[:end]
}

func (c *Config) Validate() error {
	positive := []struct {
		name  string
		value int
	}{
		{"crawler.task_workers", c.Crawler.TaskWorkers},
		{"crawler.run_workers", c.Crawler.RunWorkers},
		{"crawler.saver_workers", c.Crawler.SaverWorkers},
		{"chunker.max_runes", c.Chunker.MaxRunes},
		{"embedding.batch_size", c.Embedding.BatchSize},
		{"indexer.workers", c.Indexer.Workers},
		{"indexer.queue_size", c.Indexer.QueueSize},
		{"queue.channel_buffer", c.Queue.ChannelBuffer},
		{"retrieval.candidates", c.Retrieval.Candidates},
		{"retrieval.rrf_k", c.Retrieval.RRFK},
		{"retrieval.default_limit", c.Retrieval.DefaultLimit},
		{"retrieval.max_limit", c.Retrieval.MaxLimit},
		{"rank.max_iterations", c.Rank.MaxIterations},
		{"rank.twin_checks", c.Rank.TwinChecks},
		{"chat.max_tokens", c.Chat.MaxTokens},
		{"chat.context_chunks", c.Chat.ContextChunks},
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
		{"crawler.fetch_timeout", c.Crawler.FetchTimeout},
		{"cache.page_ttl", c.Cache.PageTTL},
		{"cache.robots_ttl", c.Cache.RobotsTTL},
		{"cache.document_hash_ttl", c.Cache.DocumentHashTTL},
		{"embedding.request_timeout", c.Embedding.RequestTimeout},
		{"indexer.enqueue_timeout", c.Indexer.EnqueueTimeout},
		{"queue.request_timeout", c.Queue.RequestTimeout},
		{"queue.flush_interval", c.Queue.FlushInterval},
		{"run_state.ttl", c.RunState.TTL},
		{"run_state.slot_lease", c.RunState.SlotLease},
		{"shutdown.timeout", c.Shutdown.Timeout},
		{"shutdown.component_timeout", c.Shutdown.ComponentTimeout},
		{"search.request_timeout", c.Search.RequestTimeout},
		{"chat.request_timeout", c.Chat.RequestTimeout},
	}

	for _, field := range durations {
		if field.value <= 0 {
			return fmt.Errorf("%w: %s must be > 0, got %s", ErrInvalidConfig, field.name, field.value.Std())
		}
	}

	if c.Crawler.UserAgent == "" || strings.ContainsFunc(c.Crawler.UserAgent, unicode.IsControl) {
		return fmt.Errorf("%w: crawler.user_agent must be a non-empty single line", ErrInvalidConfig)
	}

	if c.Crawler.RobotsAgent() == "" {
		return fmt.Errorf("%w: crawler.user_agent %q must start with a product name (letters, '-' or '_')",
			ErrInvalidConfig, c.Crawler.UserAgent)
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

	if c.Retrieval.DefaultLimit > c.Retrieval.MaxLimit {
		return fmt.Errorf("%w: retrieval.default_limit (%d) exceeds retrieval.max_limit (%d)",
			ErrInvalidConfig, c.Retrieval.DefaultLimit, c.Retrieval.MaxLimit)
	}

	if c.Rank.Damping <= 0 || c.Rank.Damping >= 1 {
		return fmt.Errorf("%w: rank.damping must be in (0, 1), got %v", ErrInvalidConfig, c.Rank.Damping)
	}

	if c.Rank.Tolerance <= 0 {
		return fmt.Errorf("%w: rank.tolerance must be > 0, got %v", ErrInvalidConfig, c.Rank.Tolerance)
	}

	switch c.Chat.Provider {
	case ChatProviderOpenAICompatible, ChatProviderOpenAI, ChatProviderAnthropic:
	default:
		return fmt.Errorf("%w: chat.provider %q must be %q, %q or %q", ErrInvalidConfig, c.Chat.Provider,
			ChatProviderOpenAICompatible, ChatProviderOpenAI, ChatProviderAnthropic)
	}

	if c.Chat.Model == "" {
		return fmt.Errorf("%w: chat.model must not be empty", ErrInvalidConfig)
	}

	if c.Chat.MaxRetries < 0 {
		return fmt.Errorf("%w: chat.max_retries must be >= 0, got %d", ErrInvalidConfig, c.Chat.MaxRetries)
	}

	if t := c.Chat.Temperature; t != nil && (*t < 0 || *t > 2) {
		return fmt.Errorf("%w: chat.temperature must be in [0, 2], got %v", ErrInvalidConfig, *t)
	}

	if c.Search.Listen == "" {
		return fmt.Errorf("%w: search.listen must not be empty", ErrInvalidConfig)
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

func ptr[T any](v T) *T {
	return &v
}
