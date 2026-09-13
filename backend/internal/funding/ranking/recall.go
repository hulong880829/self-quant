package ranking

const MinimumRecallAt100 = 0.99

type RecallAudit struct {
	K          int
	Expected   int
	Recovered  int
	Recall     float64
	MissingIDs []string
}

func (a RecallAudit) Passed() bool {
	return a.K == 100 && a.Recall >= MinimumRecallAt100
}

// AuditRecall compares an isolated full-universe ground truth with the candidate
// ranking. It is intentionally side-effect free so callers can run it outside
// the production API and ranking refresh paths.
func AuditRecall(fullUniverse, candidateRanking []Opportunity, k int) RecallAudit {
	if k <= 0 || k > 100 {
		k = 100
	}
	expected := min(k, len(fullUniverse))
	actual := make(map[string]struct{}, min(k, len(candidateRanking)))
	for index := 0; index < len(candidateRanking) && index < k; index++ {
		actual[opportunityKey(candidateRanking[index])] = struct{}{}
	}
	result := RecallAudit{K: k, Expected: expected}
	for index := 0; index < expected; index++ {
		id := opportunityKey(fullUniverse[index])
		if _, ok := actual[id]; ok {
			result.Recovered++
		} else {
			result.MissingIDs = append(result.MissingIDs, id)
		}
	}
	if expected == 0 {
		result.Recall = 1
	} else {
		result.Recall = float64(result.Recovered) / float64(expected)
	}
	return result
}
