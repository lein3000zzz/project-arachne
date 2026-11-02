package main

import (
	"testing"
	"web-crawler/internal/vectorstore"
)

func TestTheOldCollectionIsChosenOrNamed(t *testing.T) {
	const current = "chunks_v1_text_embedding_3_small_1536"

	cols := []vectorstore.Collection{
		{Name: "chunks_v1_qwen3_embedding_0_6b_1024", Dimension: 1024, Rows: 5000},
		{Name: current, Dimension: 1536, Rows: 12},
		{Name: "chunks_v1_nomic_embed_text_768", Dimension: 768, Rows: 0},
	}

	if c, err := choose(cols, current, ""); err != nil || c.Dimension != 1024 {
		t.Errorf("the only other one with chunks: %+v, %v", c, err)
	}

	if c, err := choose(cols, current, "chunks_v1_nomic_embed_text_768"); err != nil || c.Dimension != 768 {
		t.Errorf("named: %+v, %v", c, err)
	}

	for _, name := range []string{current, "someone_elses"} {
		if _, err := choose(cols, current, name); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}

	cols[2].Rows = 3
	if _, err := choose(cols, current, ""); err == nil {
		t.Error("two with chunks: one must be named")
	}

	if _, err := choose(cols[1:2], current, ""); err == nil {
		t.Error("nothing to copy from")
	}
}
