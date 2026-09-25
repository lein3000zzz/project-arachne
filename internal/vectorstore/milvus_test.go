package vectorstore

import "testing"

func TestCollectionName(t *testing.T) {
	cases := []struct {
		prefix, model string
		dim           int
		want          string
	}{
		{"chunks", "qwen3-embedding:0.6b", 1024, "chunks_v1_qwen3_embedding_0_6b_1024"},
		{"chunks", "text-embedding-3-large", 256, "chunks_v1_text_embedding_3_large_256"},
		{"exp", "BAAI/bge-m3", 1024, "exp_v1_baai_bge_m3_1024"},
		{"chunks", "модель", 8, "chunks_v1__8"},
	}

	for _, tc := range cases {
		if got := CollectionName(tc.prefix, tc.model, tc.dim); got != tc.want {
			t.Errorf("CollectionName(%q, %q, %d) = %q, want %q", tc.prefix, tc.model, tc.dim, got, tc.want)
		}
	}
}

func TestIsHexID(t *testing.T) {
	for _, ok := range []string{"0123456789abcdef", "a", "ff00ff00ff00ff00ff00ff00ff00ff00"} {
		if !isHexID(ok) {
			t.Errorf("isHexID(%q) = false", ok)
		}
	}

	for _, bad := range []string{"", "ABCDEF", "g1", `a"b`, "a b", string(make([]byte, 65))} {
		if isHexID(bad) {
			t.Errorf("isHexID(%q) = true", bad)
		}
	}
}
