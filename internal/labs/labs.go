// Package labs keeps users' labs: topics, the sources their material comes
// from, and which pages of the shared base those sources brought in. A lab
// holds no pages of its own.
package labs

import (
	"errors"
	"time"
)

var (
	// Also what another user's lab is: whether it exists is not theirs to know.
	ErrNotFound     = errors.New("no such lab")
	ErrNameTaken    = errors.New("you already have a lab with that name")
	ErrSourceExists = errors.New("the lab already has that source")
	ErrInvalid      = errors.New("invalid lab request")
)

type Lab struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Topic     string    `json:"topic"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Sources   int       `json:"sources"`
	// Fetched successfully; placeholders and failed fetches are not counted.
	Pages int `json:"pages"`
	// Of those, turned down on review before indexing.
	SetAside int `json:"set_aside"`
	// The external search engines gathering may use for this lab.
	Engines []string `json:"engines"`
}

const (
	KindLink = "link"
	// Found by the model searching the web while gathering.
	KindSearch = "search"
)

type Source struct {
	ID    int64  `json:"id"`
	LabID int64  `json:"lab_id"`
	Kind  string `json:"kind"`
	URL   string `json:"url"`
	// A preset (site, section, anywhere) or a URL prefix, as runs.Request takes it.
	Scope     string    `json:"scope"`
	MaxDepth  int       `json:"max_depth"`
	MaxLinks  int       `json:"max_links"`
	CreatedAt time.Time `json:"created_at"`
	Pages     int       `json:"pages"`
	// The source's latest crawl, when it has had one.
	LastRunID string `json:"last_run_id,omitempty"`
	// For a search source: the query that found it, the engines that returned
	// it, and the model's reason for taking it.
	Query       string   `json:"query,omitempty"`
	Engines     []string `json:"engines,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	GatheringID int64    `json:"gathering_id,omitempty"`
}

type Page struct {
	URL       string     `json:"url"`
	Title     string     `json:"title"`
	Status    int        `json:"status"`
	FetchedAt *time.Time `json:"fetched_at,omitempty"`
	// The review before indexing, when there was one.
	Review *PageReview `json:"review,omitempty"`
}

type PageReview struct {
	Decision   string   `json:"decision"`
	Reasons    []string `json:"reasons"`
	Summary    string   `json:"summary"`
	Unreviewed bool     `json:"unreviewed,omitempty"`
}
