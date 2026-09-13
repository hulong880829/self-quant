package exchange

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestRegistryDefaultsIncludeDEXPerpetualAdapters(t *testing.T) {
	registry := NewRegistry(&http.Client{}, map[string]string{})
	for _, venue := range []string{"binance", "okx", "bybit", "bitget", "gate", "hyperliquid", "lighter", "aster"} {
		adapter, ok := registry.Adapter(venue)
		if !ok || adapter == nil {
			t.Fatalf("adapter %s was not registered", venue)
		}
		if venue == "hyperliquid" || venue == "lighter" || venue == "aster" {
			provider, ok := adapter.(CapabilityProvider)
			if !ok {
				t.Fatalf("adapter %s has no capability provider", venue)
			}
			capabilities, err := provider.Capabilities(t.Context(), Credentials{})
			if err != nil || len(capabilities.Products) != 1 ||
				capabilities.Products[0] != "perpetual" || !capabilities.ReduceOnly ||
				!capabilities.Arbitrage {
				t.Fatalf("%s capabilities=%+v err=%v", venue, capabilities, err)
			}
			if _, ok := adapter.(FillReader); !ok {
				t.Fatalf("adapter %s has no fill reader", venue)
			}
			if _, ok := adapter.(PositionReader); !ok {
				t.Fatalf("adapter %s has no position reader", venue)
			}
			if _, ok := adapter.(BalanceReader); !ok {
				t.Fatalf("adapter %s has no balance reader", venue)
			}
			if _, ok := adapter.(HealthReader); !ok {
				t.Fatalf("adapter %s has no health reader", venue)
			}
		}
	}
}

func TestAsterOfficialOrderFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/aster_order.json")
	if err != nil {
		t.Fatal(err)
	}
	result, err := parseAsterOrder(raw)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "filled" || result.VenueOrderID != "22542179" ||
		result.FilledQuantity != "0.001" || result.Reference.ClientOrderID != "sq_order_fixture" {
		t.Fatalf("result=%+v", result)
	}
}

func TestHyperliquidOfficialOrderFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/hyperliquid_order_status.json")
	if err != nil {
		t.Fatal(err)
	}
	var payload hyperliquidOrderStatus
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	result := payload.result(raw)
	if result.Status != "open" || result.VenueOrderID != "123456789" ||
		result.Reference.Cloid != "0x00112233445566778899aabbccddeeff" {
		t.Fatalf("result=%+v", result)
	}
}

func TestLighterOfficialOrderFixtureAndStableClientIndex(t *testing.T) {
	raw, err := os.ReadFile("testdata/lighter_order.json")
	if err != nil {
		t.Fatal(err)
	}
	var payload lighterOrdersResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	accountIndex, apiKeyIndex := int64(1), int32(2)
	result := payload.Orders[0].result(raw, Credentials{AccountIndex: &accountIndex, APIKeyIndex: &apiKeyIndex})
	if result.Status != "filled" || result.AveragePrice != "3024.66" ||
		result.Reference.Nonce != 722 {
		t.Fatalf("result=%+v", result)
	}
	first := lighterClientOrderIndex("twap:stable:1")
	if first == 0 || first != lighterClientOrderIndex("twap:stable:1") || first >= 1<<47 {
		t.Fatalf("client index=%d", first)
	}
}

func TestDEXClientIdentifiersAreDeterministic(t *testing.T) {
	if hyperliquidCloid("same") != hyperliquidCloid("same") ||
		len(hyperliquidCloid("same")) != 34 {
		t.Fatal("invalid Hyperliquid cloid")
	}
	if asterClientOrderID("abcdefghijklmnopqrstuvwxyz-0123456789-long") !=
		asterClientOrderID("abcdefghijklmnopqrstuvwxyz-0123456789-long") {
		t.Fatal("Aster client order id is not deterministic")
	}
}

func TestVenueCircuitIsAccountScopedAndRecovers(t *testing.T) {
	circuit := newVenueCircuit()
	now := time.Unix(1_700_000_000, 0)
	firstAccount, firstKey, secondAccount, secondKey := int64(1), int32(2), int64(2), int32(2)
	first := Credentials{CredentialKind: "lighter_api", AccountIndex: &firstAccount, APIKeyIndex: &firstKey}
	second := Credentials{CredentialKind: "lighter_api", AccountIndex: &secondAccount, APIKeyIndex: &secondKey}
	for range 5 {
		circuit.record(first, ErrUncertain, now)
	}
	if !errors.Is(circuit.before(first, now), ErrRateLimited) {
		t.Fatal("expected the failing account circuit to open")
	}
	if err := circuit.before(second, now); err != nil {
		t.Fatalf("second account was affected: %v", err)
	}
	if err := circuit.before(first, now.Add(6*time.Second)); err != nil {
		t.Fatalf("circuit did not recover after cooldown: %v", err)
	}
	circuit.record(first, nil, now.Add(6*time.Second))
	if circuit.open(first, now.Add(6*time.Second)) {
		t.Fatal("successful request did not reset circuit")
	}
}
