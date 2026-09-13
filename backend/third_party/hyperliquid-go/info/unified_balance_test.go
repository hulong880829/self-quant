package info

import (
	"encoding/json"
	"testing"
)

// unifiedSpotStateFixture is a captured spotClearinghouseState for a
// unified account (mainnet shape): USDC collateral plus the
// tokenToAvailableAfterMaintenance map that only unified accounts emit.
// Token 0 (USDC) has availability; HYPE (150) has a balance row but no
// availability entry.
const unifiedSpotStateFixture = `{
  "balances": [
    {"coin": "USDC", "token": 0, "total": "405.44030466", "hold": "0.0", "entryNtl": "0.0"},
    {"coin": "HYPE", "token": 150, "total": "0.0", "hold": "0.0", "entryNtl": "0.0"}
  ],
  "tokenToAvailableAfterMaintenance": [[0, "405.44030466"]]
}`

func TestSpotClearinghouseState_UnifiedAvailability(t *testing.T) {
	var s SpotClearinghouseState
	if err := json.Unmarshal([]byte(unifiedSpotStateFixture), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// The positional [token, amount] tuple decodes into named fields.
	if len(s.TokenToAvailableAfterMaintenance) != 1 {
		t.Fatalf("TokenToAvailableAfterMaintenance len = %d, want 1", len(s.TokenToAvailableAfterMaintenance))
	}
	got := s.TokenToAvailableAfterMaintenance[0]
	if got.Token != 0 || got.Available != "405.44030466" {
		t.Errorf("entry = %+v, want {Token:0 Available:405.44030466}", got)
	}

	// Accessor by token index.
	if amt, ok := s.AvailableAfterMaintenance(0); !ok || amt != "405.44030466" {
		t.Errorf("AvailableAfterMaintenance(0) = (%q, %v), want (405.44030466, true)", amt, ok)
	}
	if amt, ok := s.AvailableAfterMaintenance(999); ok {
		t.Errorf("AvailableAfterMaintenance(999) = (%q, %v), want (\"\", false)", amt, ok)
	}

	// Accessor by coin (resolves coin -> token via Balances).
	if amt, ok := s.Available("USDC"); !ok || amt != "405.44030466" {
		t.Errorf("Available(USDC) = (%q, %v), want (405.44030466, true)", amt, ok)
	}
	if amt, ok := s.Available("usdc"); !ok || amt != "405.44030466" {
		t.Errorf("Available is not case-insensitive: (%q, %v)", amt, ok)
	}
	// HYPE has a balance row but no availability entry.
	if amt, ok := s.Available("HYPE"); ok {
		t.Errorf("Available(HYPE) = (%q, %v), want (\"\", false)", amt, ok)
	}
	// Unknown coin.
	if _, ok := s.Available("DOGE"); ok {
		t.Error("Available(DOGE) ok = true, want false")
	}
}

// TestSpotClearinghouseState_ClassicAccount confirms a classic-account
// response (no tokenToAvailableAfterMaintenance field) decodes cleanly
// and the accessors report "not available" rather than panicking — the
// signal for callers to fall back to Total minus Hold.
func TestSpotClearinghouseState_ClassicAccount(t *testing.T) {
	const classic = `{"balances": [{"coin": "USDC", "token": 0, "total": "100.0", "hold": "0.0", "entryNtl": "0.0"}]}`
	var s SpotClearinghouseState
	if err := json.Unmarshal([]byte(classic), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.TokenToAvailableAfterMaintenance != nil {
		t.Errorf("expected nil availability for classic account, got %+v", s.TokenToAvailableAfterMaintenance)
	}
	if _, ok := s.Available("USDC"); ok {
		t.Error("Available(USDC) ok = true for classic account, want false (caller falls back to total-hold)")
	}
}

// TestTokenAvailable_MalformedTuple ensures a wrong-shaped tuple surfaces
// an error rather than silently producing a zero entry.
func TestTokenAvailable_MalformedTuple(t *testing.T) {
	var s SpotClearinghouseState
	const bad = `{"balances": [], "tokenToAvailableAfterMaintenance": [[0]]}`
	if err := json.Unmarshal([]byte(bad), &s); err == nil {
		t.Fatal("expected error on malformed [token] tuple, got nil")
	}
}
