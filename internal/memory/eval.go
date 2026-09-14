package memory

import "fmt"

// RecallCase is one labeled offline retrieval query. RelevantKeys should use
// stable memory keys so content rewrites do not invalidate the benchmark.
type RecallCase struct {
	Query        string
	RelevantKeys []string
}

// RecallMetrics are macro-averaged retrieval metrics plus duplicate pressure.
// HitRateAtK answers "did we find anything useful?"; RecallAtK measures how
// much of the labeled set was found; MRR rewards placing the first hit early.
type RecallMetrics struct {
	Cases         int
	HitRateAtK    float64
	RecallAtK     float64
	MRR           float64
	DuplicateRate float64
}

// EvaluateRecall runs a deterministic labeled benchmark against any Recaller.
// It intentionally reuses production Recall, so it also detects regressions in
// query sanitization, cross-store merge, ranking and duplicate suppression.
func EvaluateRecall(r Recaller, cases []RecallCase, topK int) (RecallMetrics, error) {
	if r == nil {
		return RecallMetrics{}, fmt.Errorf("recaller is nil")
	}
	if topK <= 0 {
		topK = 5
	}
	metrics := RecallMetrics{Cases: len(cases)}
	if len(cases) == 0 {
		return metrics, nil
	}
	var hits, recallSum, reciprocalRank float64
	totalReturned, duplicates := 0, 0
	for _, c := range cases {
		got, err := r.Recall(c.Query, topK)
		if err != nil {
			return RecallMetrics{}, fmt.Errorf("recall %q: %w", c.Query, err)
		}
		relevant := make(map[string]bool, len(c.RelevantKeys))
		for _, key := range c.RelevantKeys {
			relevant[key] = true
		}
		found := map[string]bool{}
		for rank, m := range got {
			isDuplicate := nearDuplicateOfAny(m.Content, got[:rank])
			if isDuplicate {
				duplicates++
			}
			totalReturned++
			if relevant[m.Key] && !found[m.Key] {
				found[m.Key] = true
				if len(found) == 1 {
					hits++
					reciprocalRank += 1 / float64(rank+1)
				}
			}
		}
		if len(relevant) > 0 {
			recallSum += float64(len(found)) / float64(len(relevant))
		}
	}
	n := float64(len(cases))
	metrics.HitRateAtK = hits / n
	metrics.RecallAtK = recallSum / n
	metrics.MRR = reciprocalRank / n
	if totalReturned > 0 {
		metrics.DuplicateRate = float64(duplicates) / float64(totalReturned)
	}
	return metrics, nil
}
