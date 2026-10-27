package config

import (
	"strings"
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
	// URL prefix every followed page must start with; empty follows links anywhere.
	// Links out of scope are still recorded in the graph, only not fetched.
	Scope string `json:"scope,omitempty"`

	ack func()
}

func (r *Run) InScope(url string) bool {
	return r.Scope == "" || strings.HasPrefix(url, r.Scope)
}

func (r *Run) SetAck(ack func()) {
	r.ack = ack
}

// Ack: see Task.Ack.
func (r *Run) Ack() {
	if ack := r.ack; ack != nil {
		r.ack = nil
		ack()
	}
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
