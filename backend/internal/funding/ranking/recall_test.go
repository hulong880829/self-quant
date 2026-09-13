package ranking

import "testing"

func TestAuditRecallUsesDirectedCandidateIdentity(t *testing.T) {
	item := func(symbol, long, short string) Opportunity {
		return Opportunity{
			GlobalSymbol: symbol,
			Long:         Leg{Exchange: long, ExchangeSymbol: symbol},
			Short:        Leg{Exchange: short, ExchangeSymbol: symbol},
		}
	}
	full := []Opportunity{
		item("BTCUSDT", "binance", "okx"),
		item("ETHUSDT", "okx", "binance"),
	}
	candidate := []Opportunity{
		item("BTCUSDT", "binance", "okx"),
		item("ETHUSDT", "binance", "okx"),
	}
	audit := AuditRecall(full, candidate, 100)
	if audit.Recall != 0.5 || audit.Recovered != 1 ||
		len(audit.MissingIDs) != 1 || audit.Passed() {
		t.Fatalf("audit=%+v", audit)
	}
}

func TestAuditRecallEmptyGroundTruthIsComplete(t *testing.T) {
	if audit := AuditRecall(nil, nil, 100); audit.Recall != 1 || !audit.Passed() {
		t.Fatalf("audit=%+v", audit)
	}
}
