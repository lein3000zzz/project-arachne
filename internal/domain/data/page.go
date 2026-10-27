package data

import (
	"encoding/json"
	"time"
)

type PageData struct {
	URL           string    `json:"url" bson:"url"`
	Status        int       `json:"status" bson:"status"`
	Title         string    `json:"title" bson:"title"`
	Links         []string  `json:"links" bson:"links"`
	PageLinks     []string  `json:"pageLinks" bson:"pageLinks"`
	Aliases       []string  `json:"aliases" bson:"aliases"`
	LastRunID     string    `json:"lastRunID" bson:"lastRunID"`
	LastUpdatedAt time.Time `json:"lastUpdatedAt" bson:"lastUpdatedAt,omitempty"`
	FoundAt       time.Time `json:"foundAt" bson:"foundAt,omitempty"`
	ContentType   string    `json:"contentType" bson:"contentType"`
}

func (p *PageData) MarshalBinary() ([]byte, error) {
	return json.Marshal(p)
}
