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
	// A document's references, offered to the labs that hold it and never
	// crawled. nil leaves the stored ones alone; empty removes them.
	References []Reference `json:"references,omitempty" bson:"references,omitempty"`
}

// Reference is a link or a cited work found in a document, with the text
// around it - in a paper's reference list, the citation.
type Reference struct {
	URL   string `json:"url"`
	Label string `json:"label"`
}

func (p *PageData) MarshalBinary() ([]byte, error) {
	return json.Marshal(p)
}
