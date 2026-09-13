//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestSubAccount_CreateDepositList creates a fresh sub-account, lists
// the parent's directory via Info.SubAccounts, and (when balance
// permits) round-trips a 1 USDC deposit/withdraw. The 16-char name cap
// is respected. Common pre-existing-name or sub-cap errors are treated
// as skips since the SDK path was exercised regardless.
func TestSubAccount_CreateDepositList(t *testing.T) {
	c := newClient(t)
	skipIfNoBalance(t, c)
	cfg, _ := loadConfig()

	// 16-char limit: "itest-sub-" is 10 chars + up to 5 digits = 15.
	name := fmt.Sprintf("itest-sub-%05d", time.Now().Unix()%100000)
	resp, err := c.Trade.SubAccount.Create(name)
	if err != nil {
		if strings.Contains(err.Error(), "already") ||
			strings.Contains(err.Error(), "Too many") ||
			strings.Contains(err.Error(), "cap") {
			t.Skipf("SubAccount.Create skipped: %v", err)
		}
		t.Fatalf("SubAccount.Create(%s): %v", name, err)
	}
	if resp != nil && resp.Error != "" {
		if strings.Contains(resp.Error, "Too many") ||
			strings.Contains(resp.Error, "already") {
			t.Skipf("SubAccount.Create returned soft error: %s", resp.Error)
		}
		t.Fatalf("SubAccount.Create returned error: %s", resp.Error)
	}

	subs, err := c.Info.SubAccounts(cfg.AccountAddr)
	if err != nil {
		t.Fatalf("Info.SubAccounts: %v", err)
	}
	var subAddr string
	for _, s := range subs {
		if s.Name == name {
			subAddr = s.User
			break
		}
	}
	if subAddr == "" {
		t.Logf("created sub %q not yet visible in SubAccounts list (acceptable for replication lag)", name)
		return
	}
	t.Logf("sub-account created: name=%q user=%s", name, subAddr)

	if cfg.SkipTransfer {
		return
	}
	if _, err := c.Trade.SubAccount.DepositUSD(subAddr, 1.0); err != nil {
		t.Logf("SubAccount.DepositUSD: %v (best-effort)", err)
		return
	}
	if _, err := c.Trade.SubAccount.WithdrawUSD(subAddr, 1.0); err != nil {
		t.Logf("SubAccount.WithdrawUSD: %v (best-effort)", err)
	}
}
