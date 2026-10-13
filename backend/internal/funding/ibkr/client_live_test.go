//go:build ibkr_live

package ibkr

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestLiveLookupAAPL(t *testing.T) {
	addr := strings.TrimSpace(os.Getenv("IBKR_GATEWAY_ADDR"))
	if addr == "" {
		t.Fatal("IBKR_GATEWAY_ADDR is required")
	}
	clientID, err := strconv.Atoi(strings.TrimSpace(os.Getenv("IBKR_DISCOVERY_TEST_CLIENT_ID")))
	if err != nil || clientID <= 0 || clientID == 30 || clientID == 31 {
		t.Fatal("IBKR_DISCOVERY_TEST_CLIENT_ID must be a positive id other than 30 or 31")
	}
	contracts, err := NewClient(addr, clientID).LookupUSStock(context.Background(), "AAPL")
	if err != nil {
		t.Fatal(err)
	}
	if len(contracts) == 0 {
		t.Fatal("expected at least one AAPL contract")
	}
	for _, contract := range contracts {
		if !strings.EqualFold(contract.SecType, "STK") || !strings.EqualFold(contract.Currency, "USD") || contract.ConID <= 0 {
			t.Fatalf("contract=%+v", contract)
		}
	}
}
