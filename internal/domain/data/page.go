package data

import (
	"encoding/json"
	"time"
)

type PageData struct {
	URL           string    `json:"url" bson:"url"`
	Status        int       `json:"status" bson:"status"`
	Links         []string  `json:"links" bson:"links"`
	LastRunID     string    `json:"lastRunID" bson:"lastRunID"`
	LastUpdatedAt time.Time `json:"lastUpdatedAt" bson:"lastUpdatedAt,omitempty"`
	FoundAt       time.Time `json:"foundAt" bson:"foundAt,omitempty"`
	ContentType   string    `json:"contentType" bson:"contentType"`
}

func (p *PageData) MarshalBinary() ([]byte, error) {
	return json.Marshal(p)
}

func (p *PageData) ToParams() (map[string]any, error) {
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
