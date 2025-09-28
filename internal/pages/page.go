package pages

import "time"

type PageData struct {
	URL           string    `json:"url"`
	Status        string    `json:"status"`
	Links         []string  `json:"links"`
	LastRunID     string    `json:"lastRunID"`
	LastUpdatedAt time.Time `json:"lastUpdatedAt"`
	ContentType   string    `json:"contentType"`
}

type PageDataRepo interface {
	s()
}
