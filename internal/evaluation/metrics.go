// Package evaluation scores ranked page lists against graded relevance
// judgements. A page nobody judged counts as not relevant, as in TREC; Judged
// reports how much of the top k that assumption covers.
package evaluation

import "math"

// Grades follow the usual 0-3 scale: 0 not relevant, 1 marginally, 2 relevant,
// 3 the answer.
type Judgements map[string]int

type Scores struct {
	NDCG   float64 `json:"ndcg"`
	MRR    float64 `json:"mrr"`
	Recall float64 `json:"recall"`
	// Share of the top k that was judged at all. Low values mean the other
	// metrics are guesses: unjudged pages may well be relevant.
	Judged float64 `json:"judged"`
}

// Score evaluates the first k of ranked. A page counts as relevant for MRR and
// recall from grade minRelevant up; nDCG uses every grade with gain 2^g - 1.
func Score(ranked []string, judged Judgements, k, minRelevant int) Scores {
	top := ranked[:min(k, len(ranked))]

	var dcg float64

	var scores Scores

	relevantFound, judgedCount := 0, 0

	for i, url := range top {
		grade, ok := judged[url]
		if ok {
			judgedCount++
		}

		dcg += gain(grade) / math.Log2(float64(i+2))

		if grade >= minRelevant {
			relevantFound++

			if scores.MRR == 0 {
				scores.MRR = 1 / float64(i+1)
			}
		}
	}

	if ideal := idealDCG(judged, k); ideal > 0 {
		scores.NDCG = dcg / ideal
	}

	if relevant := countRelevant(judged, minRelevant); relevant > 0 {
		scores.Recall = float64(relevantFound) / float64(relevant)
	}

	if len(top) > 0 {
		scores.Judged = float64(judgedCount) / float64(len(top))
	}

	return scores
}

func gain(grade int) float64 {
	if grade <= 0 {
		return 0
	}

	return math.Exp2(float64(grade)) - 1
}

func idealDCG(judged Judgements, k int) float64 {
	grades := make([]int, 0, len(judged))
	for _, grade := range judged {
		if grade > 0 {
			grades = append(grades, grade)
		}
	}

	// Descending; the lists are a few dozen long.
	for i := 1; i < len(grades); i++ {
		for j := i; j > 0 && grades[j] > grades[j-1]; j-- {
			grades[j], grades[j-1] = grades[j-1], grades[j]
		}
	}

	var ideal float64
	for i, grade := range grades[:min(k, len(grades))] {
		ideal += gain(grade) / math.Log2(float64(i+2))
	}

	return ideal
}

func countRelevant(judged Judgements, minRelevant int) int {
	n := 0

	for _, grade := range judged {
		if grade >= minRelevant {
			n++
		}
	}

	return n
}

// Mean averages per-query scores; every query weighs the same.
func Mean(all []Scores) Scores {
	var sum Scores

	for _, s := range all {
		sum.NDCG += s.NDCG
		sum.MRR += s.MRR
		sum.Recall += s.Recall
		sum.Judged += s.Judged
	}

	if n := float64(len(all)); n > 0 {
		sum.NDCG /= n
		sum.MRR /= n
		sum.Recall /= n
		sum.Judged /= n
	}

	return sum
}
