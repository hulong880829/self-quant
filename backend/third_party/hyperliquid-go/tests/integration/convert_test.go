//go:build integration

package integration

import (
	"strconv"
	"strings"
	"testing"

	hl "github.com/Simon-Busch/hyperliquid-go"
)

// usdcUsdhConvertMinUSDC is the floor used by these scenarios. The
// USDH/USDC spot pair on Hyperliquid enforces the same $10 minimum
// order value as outcome markets; sized to land just over that.
const usdcUsdhConvertMinUSDC = 11.0

// TestConvert_USDCToUSDH spends a small amount of spot USDC to buy USDH
// via Trader.Convert.USDCToUSDH. The scenario skips when the account
// lacks the spot-USDC headroom, when the USDH/USDC pair is not present
// in the spot universe, or when the order is below the venue minimum.
func TestConvert_USDCToUSDH(t *testing.T) {
	c := newClient(t)
	skipIfNoSpotBalance(t, c)

	cfg, _ := loadConfig()

	usdcBefore := spotBalance(t, c, cfg.AccountAddr, "USDC")
	usdhBefore := spotBalance(t, c, cfg.AccountAddr, "USDH")
	t.Logf("before: USDC=%.6f USDH=%.6f", usdcBefore, usdhBefore)

	if usdcBefore < usdcUsdhConvertMinUSDC {
		t.Skipf("spot USDC balance %.2f below %.2f threshold; top up the spot wallet to run this scenario",
			usdcBefore, usdcUsdhConvertMinUSDC)
	}

	res, err := c.Trade.Convert.USDCToUSDH(usdcUsdhConvertMinUSDC)
	if err != nil {
		t.Fatalf("Convert.USDCToUSDH: %v", err)
	}
	if res.Error != "" {
		if strings.Contains(res.Error, "minimum value") || strings.Contains(res.Error, "Insufficient") {
			t.Skipf("Convert.USDCToUSDH rejected by venue: %s", res.Error)
		}
		t.Fatalf("Convert.USDCToUSDH rejected: %s", res.Error)
	}
	t.Logf("buy ack: oid=%d status=%s avgPx=%s totalSz=%s",
		res.OID, res.Status, res.AvgPx, res.TotalSz)
}

// TestConvert_USDHToUSDC sells a small USDH position back into spot
// USDC via Trader.Convert.USDHToUSDC. Skips when the account has no
// USDH to liquidate.
func TestConvert_USDHToUSDC(t *testing.T) {
	c := newClient(t)
	cfg, _ := loadConfig()

	usdhBefore := spotBalance(t, c, cfg.AccountAddr, "USDH")
	if usdhBefore <= 0 {
		t.Skipf("no USDH balance to convert back; run TestConvert_USDCToUSDH first or top up USDH")
	}

	// Cap the sell to a sane upper bound so a fat-fingered config does
	// not drain the wallet.
	amount := usdhBefore
	if amount > 20.0 {
		amount = 20.0
	}
	t.Logf("converting %.6f USDH back to USDC", amount)

	res, err := c.Trade.Convert.USDHToUSDC(amount)
	if err != nil {
		t.Fatalf("Convert.USDHToUSDC: %v", err)
	}
	if res.Error != "" {
		if strings.Contains(res.Error, "minimum value") || strings.Contains(res.Error, "Insufficient") {
			t.Skipf("Convert.USDHToUSDC rejected by venue: %s", res.Error)
		}
		t.Fatalf("Convert.USDHToUSDC rejected: %s", res.Error)
	}
	t.Logf("sell ack: oid=%d status=%s avgPx=%s totalSz=%s",
		res.OID, res.Status, res.AvgPx, res.TotalSz)
}

// spotBalance returns the total balance of one spot token for addr.
// Returns 0 when the token is absent; fatals only on a transport error.
func spotBalance(t *testing.T, c *hl.Client, addr, symbol string) float64 {
	t.Helper()
	spot, err := c.Info.SpotBalances(addr)
	if err != nil {
		t.Fatalf("SpotBalances: %v", err)
	}
	for _, b := range spot.Balances {
		if strings.EqualFold(b.Coin, symbol) {
			v, _ := strconv.ParseFloat(b.Total, 64)
			return v
		}
	}
	return 0
}
