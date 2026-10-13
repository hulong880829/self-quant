package funding

import "testing"

func TestPlanIBCanonicalMergesDuplicateConIDsAndSkipsCrypto(t *testing.T) {
	conID := int64(265598)
	actions := PlanIBCanonical([]IBResolution{
		{Symbol: "stock|AAPL|USD|US", ConID: &conID, Status: "active", PairMatches: map[string]PairMatch{"binance|AAPLUSDT": {MatchClass: MatchVerifiedListing}}},
		{Symbol: "etf|AAPL|USD|US", ConID: &conID, Status: "active"},
		{Symbol: "AAPL", ConID: &conID, Status: "active"},
		{Symbol: "crypto|BTC|USD|", Status: MatchNotFound},
		{Symbol: "tradfi_unclassified|ZHIPU|USD|", Status: MatchNotFound},
	})
	saves, clears := 0, 0
	var saved IBCanonicalAction
	for _, action := range actions {
		switch action.Kind {
		case "save":
			saves++
			saved = action
		case "clear":
			clears++
			if action.Symbol == "crypto|BTC|USD|" {
				t.Fatal("crypto not_found must stay untouched")
			}
		}
	}
	if saves != 1 || saved.Target != "conid:265598" || clears != 3 {
		t.Fatalf("saves=%d clears=%d action=%+v", saves, clears, saved)
	}
	if _, ok := saved.Resolution.PairMatches["binance|AAPLUSDT"]; !ok {
		t.Fatalf("pairs=%+v", saved.Resolution.PairMatches)
	}
}

func TestCanonicalLockSQLUsesAdvisoryLock(t *testing.T) {
	if canonicalIBLockSQL != "SELECT pg_advisory_xact_lock($1)" {
		t.Fatal(canonicalIBLockSQL)
	}
}
