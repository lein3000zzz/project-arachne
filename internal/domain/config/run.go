package config

import (
	"web-crawler/internal/utils"
)

// Run - конфиг "забега"
// State в редисе (дб 2) для координации между нодами
type Run struct {
	ID string `json:"id"`

	UseCacheFlag bool            `json:"use_cache_flag"`
	MaxDepth     int             `json:"max_depth"`
	MaxLinks     int             `json:"max_links"`
	ExtraFlags   *ExtraTaskFlags `json:"extra_flags,omitempty"`

	StartURL string `json:"start_url"`
}

func NewRun(URL string, maxDepth, maxLinks int, flags *ExtraTaskFlags) *Run {
	id, _ := utils.GenerateID()

	return &Run{
		ID:           id,
		UseCacheFlag: true,
		MaxDepth:     maxDepth,
		MaxLinks:     maxLinks,
		ExtraFlags:   flags,
		StartURL:     URL,
	}
}
