package evaluation

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestScorePerfectRanking(t *testing.T) {
	judged := Judgements{"a": 3, "b": 2, "c": 1}

	s := Score([]string{"a", "b", "c", "x"}, judged, 10, 1)
	if !near(s.NDCG, 1) || s.MRR != 1 || s.Recall != 1 || !near(s.Judged, 0.75) {
		t.Errorf("perfect ranking scored %+v", s)
	}
}

func TestScoreByHand(t *testing.T) {
	judged := Judgements{"a": 3, "b": 1, "z": 0}

	// x (unjudged), b (1), a (3), z (0)
	s := Score([]string{"x", "b", "a", "z"}, judged, 3, 1)

	dcg := 1/math.Log2(3) + 7/math.Log2(4)
	ideal := 7/math.Log2(2) + 1/math.Log2(3)

	if !near(s.NDCG, dcg/ideal) {
		t.Errorf("nDCG = %v, want %v", s.NDCG, dcg/ideal)
	}

	if !near(s.MRR, 0.5) {
		t.Errorf("MRR = %v, want 1/2: b at rank 2 is the first relevant page", s.MRR)
	}

	if s.Recall != 1 {
		t.Errorf("recall = %v, want 1", s.Recall)
	}

	if !near(s.Judged, 2.0/3) {
		t.Errorf("judged = %v, want 2/3", s.Judged)
	}
}

func TestScoreCutsAtK(t *testing.T) {
	judged := Judgements{"a": 2}

	s := Score([]string{"x", "y", "a"}, judged, 2, 1)
	if s.NDCG != 0 || s.MRR != 0 || s.Recall != 0 {
		t.Errorf("a relevant page below k must not count: %+v", s)
	}
}

func TestScoreMinRelevantRaisesTheBar(t *testing.T) {
	judged := Judgements{"a": 1, "b": 3}

	s := Score([]string{"a", "b"}, judged, 10, 2)
	if !near(s.MRR, 0.5) || s.Recall != 1 {
		t.Errorf("with minRelevant 2 only b counts: %+v", s)
	}
}

func TestScoreEdgeCases(t *testing.T) {
	if s := Score(nil, Judgements{"a": 3}, 10, 1); s != (Scores{}) {
		t.Errorf("no results: %+v", s)
	}

	if s := Score([]string{"a"}, Judgements{"a": 0}, 10, 1); s.NDCG != 0 || s.Recall != 0 || s.Judged != 1 {
		t.Errorf("nothing relevant exists: %+v", s)
	}
}

func TestMean(t *testing.T) {
	m := Mean([]Scores{{NDCG: 1, MRR: 1, Recall: 1, Judged: 1}, {}})
	if m != (Scores{NDCG: 0.5, MRR: 0.5, Recall: 0.5, Judged: 0.5}) {
		t.Errorf("mean = %+v", m)
	}

	if Mean(nil) != (Scores{}) {
		t.Error("mean of nothing must be zero, not NaN")
	}
}
