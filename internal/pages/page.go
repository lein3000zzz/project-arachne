package pages

import (
	"encoding/json"
	"time"
)

type PageData struct {
	URL           string    `json:"url"`
	Status        int       `json:"status"`
	Links         []string  `json:"links"`
	LastRunID     string    `json:"lastRunID"`
	LastUpdatedAt time.Time `json:"lastUpdatedAt"`
	FoundAt       time.Time `json:"foundAt"`
	ContentType   string    `json:"contentType"`
}

func (p PageData) toParams() (map[string]any, error) {
	var m map[string]any
	b, err := json.Marshal(p)

	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

type PageDataRepo interface {
	SavePage(page PageData) error
	EnsureConnectivity() error
}
