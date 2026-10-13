package funding

import (
	"fmt"
	"strings"
)

// IBCanonicalAction is one canonical migration step. Crypto not_found rows are omitted.
type IBCanonicalAction struct {
	Kind       string
	Symbol     string
	Target     string
	ConID      int64
	Resolution IBResolution
}

func PlanIBCanonical(rows []IBResolution) []IBCanonicalAction {
	grouped := map[int64][]IBResolution{}
	var actions []IBCanonicalAction
	for _, row := range rows {
		if cryptoNotFound(row) {
			continue
		}
		if row.ConID != nil && *row.ConID > 0 {
			grouped[*row.ConID] = append(grouped[*row.ConID], row)
			continue
		}
		actions = append(actions, IBCanonicalAction{Kind: "clear", Symbol: row.Symbol})
	}
	for conID, group := range grouped {
		target := fmt.Sprintf("conid:%d", conID)
		survivor := group[0]
		for _, row := range group {
			if row.Symbol == target {
				survivor = row
				break
			}
		}
		merged := survivor
		merged.Symbol = target
		merged.PairMatches = mergePairMatches(nil, survivor.PairMatches)
		merged.Aliases = append([]string{}, survivor.Aliases...)
		for _, row := range group {
			merged.PairMatches = mergePairMatches(merged.PairMatches, row.PairMatches)
			merged.Aliases = mergeAliases(merged.Aliases, append(row.Aliases, row.Symbol))
		}
		actions = append(actions, IBCanonicalAction{
			Kind: "save", Symbol: survivor.Symbol, Target: target, ConID: conID, Resolution: merged,
		})
		for _, row := range group {
			if row.Symbol == survivor.Symbol {
				continue
			}
			actions = append(actions, IBCanonicalAction{Kind: "clear", Symbol: row.Symbol, ConID: conID})
		}
	}
	return actions
}

func cryptoNotFound(row IBResolution) bool {
	return strings.HasPrefix(row.Symbol, "crypto|") && row.Status == MatchNotFound
}
