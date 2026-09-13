package info

import (
	"encoding/json"
	"testing"
)

// TestOutcomeMeta_QuoteTokenUSDC pins the post-USDH wire shape: the
// venue tags each HIP-4 outcome with a `quoteToken` naming the spot
// token the market is collateralized and settled in. Since the USDH
// sunset, live mainnet outcomes quote in USDC, so the SDK must surface
// that field rather than assume a hardcoded stablecoin.
//
// This is the deterministic (no-network) half of the "we can buy an
// outcome market with USDC" guarantee; the live buy lives in the
// integration suite (TestHIP4_BuyOutcomeWithUSDC).
func TestOutcomeMeta_QuoteTokenUSDC(t *testing.T) {
	// Trimmed copy of a real POST /info {"type":"outcomeMeta"} response:
	// the May CPI question (USDC-quoted) plus a legacy USDH outcome, so
	// the test proves the field is read verbatim, not defaulted.
	const blob = `{
	  "outcomes": [
	    {"outcome": 101, "name": "Below 4.3%", "description": "class:priceBucket",
	     "sideSpecs": [{"name": "Yes"}, {"name": "No"}], "quoteToken": "USDC"},
	    {"outcome": 7003, "name": "Akami", "description": "",
	     "sideSpecs": [{"name": "Yes"}, {"name": "No"}], "quoteToken": "USDH"}
	  ],
	  "questions": []
	}`

	var meta OutcomeMeta
	if err := json.Unmarshal([]byte(blob), &meta); err != nil {
		t.Fatalf("unmarshal outcomeMeta: %v", err)
	}
	if len(meta.Outcomes) != 2 {
		t.Fatalf("expected 2 outcomes, got %d", len(meta.Outcomes))
	}

	if got := meta.Outcomes[0].QuoteToken; got != "USDC" {
		t.Errorf("outcome 101 QuoteToken = %q, want %q — the SDK is dropping the venue's collateral token", got, "USDC")
	}
	if got := meta.Outcomes[1].QuoteToken; got != "USDH" {
		t.Errorf("outcome 7003 QuoteToken = %q, want %q", got, "USDH")
	}
}

// TestOutcomeMeta_QuoteTokenAbsent guards the older-snapshot path: when
// the venue omits quoteToken, the field decodes to "" rather than
// erroring, so callers can treat empty as "unknown / pre-migration".
func TestOutcomeMeta_QuoteTokenAbsent(t *testing.T) {
	const blob = `{"outcomes": [{"outcome": 1, "name": "x", "sideSpecs": [{"name": "Yes"}]}], "questions": []}`
	var meta OutcomeMeta
	if err := json.Unmarshal([]byte(blob), &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := meta.Outcomes[0].QuoteToken; got != "" {
		t.Errorf("absent quoteToken = %q, want empty string", got)
	}
}
