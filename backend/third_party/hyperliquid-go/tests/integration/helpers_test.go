//go:build integration

package integration

import (
	"context"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"


	"github.com/Simon-Busch/hyperliquid-go/info"
	hl "github.com/Simon-Busch/hyperliquid-go"
)

// requireEnv fails the test if name is unset in the environment.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		t.Skipf("env %s not set; skipping", name)
	}
	return v
}

// newClient builds a fresh hl.Client wired to the integration config. The
// client is not connected to the websocket by default; tests that need
// streaming call Stream.Connect explicitly.
func newClient(t *testing.T, opts ...hl.Option) *hl.Client {
	t.Helper()
	c, err := loadConfig()
	if err != nil {
		t.Skipf("integration env not configured: %v", err)
	}
	full := append([]hl.Option{
		hl.WithBaseURL(c.BaseURL),
		hl.WithPrivateKey(c.privateKey),
		hl.WithAccount(c.AccountAddr),
		hl.WithSkipStream(true),
	}, opts...)
	client, err := hl.New(full...)
	if err != nil {
		t.Fatalf("hl.New: %v", err)
	}
	return client
}

// newStreamingClient constructs a Client with the Stream enabled and
// connected, returning both the client and a deferred-close hook for
// t.Cleanup.
func newStreamingClient(t *testing.T) *hl.Client {
	t.Helper()
	c, err := loadConfig()
	if err != nil {
		t.Skipf("integration env not configured: %v", err)
	}
	if c.SkipWS {
		t.Skip("HL_SKIP_WS=true; skipping WS scenario")
	}
	client, err := hl.New(
		hl.WithBaseURL(c.BaseURL),
		hl.WithPrivateKey(c.privateKey),
		hl.WithAccount(c.AccountAddr),
	)
	if err != nil {
		t.Fatalf("hl.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Stream.Connect(ctx); err != nil {
		t.Fatalf("Stream.Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Stream.Close() })
	return client
}

// testCoin returns the integration coin under test.
func testCoin(t *testing.T) string {
	t.Helper()
	c, err := loadConfig()
	if err != nil {
		t.Skipf("integration env not configured: %v", err)
	}
	return c.TestCoin
}

// testSize returns the integration order size in coin units. When
// HL_TEST_NOTIONAL is set (default $10), the size is derived from the
// current mid so mainnet and testnet stay within the same USD budget.
// HL_TEST_SIZE is a fixed-size fallback used only when HL_TEST_NOTIONAL=0.
// The result is always snapped to the asset's MinSize step and clamped
// to at least one step.
func testSize(t *testing.T, client *hl.Client, coin string) float64 {
	t.Helper()
	c, _ := loadConfig()

	target := c.TestSize
	if c.TestNotional > 0 {
		px := mid(t, client, coin)
		if px > 0 {
			target = c.TestNotional / px
		}
	}

	meta, err := client.Info.Asset(coin)
	if err != nil || meta.MinSize == 0 {
		return target
	}
	if target < meta.MinSize {
		return meta.MinSize
	}
	// Snap UP to the size step. Rounding down could put us under the
	// venue's $10 minimum order value when the target sits between two
	// steps; the small overshoot is acceptable for an integration budget.
	steps := math.Ceil(target / meta.MinSize)
	if steps < 1 {
		steps = 1
	}
	return steps * meta.MinSize
}

// testSizeForLimit returns a coin-unit size such that size * limitPx is
// at least HL_TEST_NOTIONAL — used by tests that place orders far from
// mid (resting ALO/GTC, brackets, triggers) where sizing at mid would
// produce a sub-$10 notional and get rejected by the venue's minimum
// order-value rule. Snaps UP to the size step to ensure the threshold
// is cleared.
func testSizeForLimit(t *testing.T, client *hl.Client, coin string, limitPx float64) float64 {
	t.Helper()
	cfg, _ := loadConfig()
	if cfg.TestNotional <= 0 || limitPx <= 0 {
		return testSize(t, client, coin)
	}
	target := cfg.TestNotional / limitPx
	meta, err := client.Info.Asset(coin)
	if err != nil || meta.MinSize == 0 {
		return target
	}
	if target < meta.MinSize {
		return meta.MinSize
	}
	steps := math.Ceil(target / meta.MinSize)
	return steps * meta.MinSize
}

// mid returns the current mid price for coin. Delegates to Info.Mid
// which auto-routes by the "<dex>:" prefix so HIP-3 coins resolve
// without the caller having to pass a dex argument.
func mid(t *testing.T, client *hl.Client, coin string) float64 {
	t.Helper()
	px, err := client.Info.Mid(coin)
	if err != nil {
		t.Skipf("no mid for %s — skipping: %v", coin, err)
	}
	return px
}

// skipIfNoBalance skips the test when the account has no perp margin
// to trade with. Withdrawable is the wrong field here: it is zero
// whenever margin is locked by an open position, even if the account
// has plenty of equity. AccountValue is the right semantic check for
// "can this account place perp orders".
func skipIfNoBalance(t *testing.T, client *hl.Client) {
	t.Helper()
	c, _ := loadConfig()
	state, err := client.Info.UserState(c.AccountAddr)
	if err != nil {
		t.Skipf("UserState failed: %v", err)
	}
	v, err := strconv.ParseFloat(state.MarginSummary.AccountValue, 64)
	if err != nil || v <= 0 {
		t.Skipf("account %s has no perp account value (%q); fund the perp wallet to run trading scenarios",
			c.AccountAddr, state.MarginSummary.AccountValue)
	}
}

// skipIfNoSpotBalance skips when the account has no spot USDC, used by
// scenarios that transfer or convert between spot and perp classes.
func skipIfNoSpotBalance(t *testing.T, client *hl.Client) {
	t.Helper()
	c, _ := loadConfig()
	spot, err := client.Info.SpotBalances(c.AccountAddr)
	if err != nil {
		t.Skipf("SpotBalances failed: %v", err)
	}
	for _, b := range spot.Balances {
		if v, err := strconv.ParseFloat(b.Total, 64); err == nil && v > 0 {
			return
		}
	}
	t.Skipf("account %s has no spot balance", c.AccountAddr)
}

// skipIfSpotTokenBelow skips when the account's available (Total - Hold)
// balance of token is below min. HIP-4 outcome scenarios pay in USDH and
// the venue rejects orders when free balance is insufficient — so a non-
// zero total isn't enough; the live amount has to actually cover the
// order's notional. Pass min=0 to require only that some balance exists.
func skipIfSpotTokenBelow(t *testing.T, client *hl.Client, token string, min float64) {
	t.Helper()
	c, _ := loadConfig()
	spot, err := client.Info.SpotBalances(c.AccountAddr)
	if err != nil {
		t.Skipf("SpotBalances failed: %v", err)
	}
	for _, b := range spot.Balances {
		if !strings.EqualFold(b.Coin, token) {
			continue
		}
		total, terr := strconv.ParseFloat(b.Total, 64)
		hold, herr := strconv.ParseFloat(b.Hold, 64)
		if terr != nil {
			continue
		}
		if herr != nil {
			hold = 0
		}
		avail := total - hold
		if avail >= min {
			return
		}
		t.Skipf("account %s has %s available=%.6f (total=%.6f hold=%.6f), need >= %.6f",
			c.AccountAddr, token, avail, total, hold, min)
	}
	t.Skipf("account %s holds no %s; fund the testnet account to run this scenario",
		c.AccountAddr, token)
}

// awaitPosition polls Info.Position until coin has a non-zero size or
// the timeout elapses. Returns the position or nil. Used by scenarios
// that need to act on a freshly-opened position when testnet/mainnet
// state propagation lags behind a market order ack.
func awaitPosition(t *testing.T, client *hl.Client, coin string, timeout time.Duration) *info.Position {
	t.Helper()
	cfg, _ := loadConfig()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pos, err := client.Info.Position(cfg.AccountAddr, coin)
		if err == nil && pos != nil {
			if szi, perr := strconv.ParseFloat(pos.Szi, 64); perr == nil && szi != 0 {
				return pos
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil
}

// awaitFill polls Info.Fill for up to timeout, returning the first fill
// matching oid or nil if none seen.
func awaitFill(t *testing.T, client *hl.Client, oid int64, timeout time.Duration) bool {
	t.Helper()
	c, _ := loadConfig()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		fill, err := client.Info.Fill(c.AccountAddr, oid)
		if err == nil && fill != nil {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// snapPrice rounds px to a wire-valid Hyperliquid price: at most five
// significant figures AND a multiple of the asset's tick size. The
// significant-figure cap is the protocol rule; tick alignment is the
// per-asset constraint. Order matters: round to 5 sig figs first, then
// snap to tick — otherwise a tick-aligned price like 1250.25 (6 sig
// figs) sails through the tick check but fails the wire validator.
func snapPrice(px float64, client *hl.Client, coin string) float64 {
	if px <= 0 {
		return px
	}
	// 1. Round to 5 significant figures.
	digits := math.Ceil(math.Log10(math.Abs(px)))
	scale := math.Pow(10, 5-digits)
	px = math.Round(px*scale) / scale

	// 2. Snap to the asset's tick size.
	meta, err := client.Info.Asset(coin)
	if err != nil || meta.TickSize == 0 {
		return px
	}
	return math.Round(px/meta.TickSize) * meta.TickSize
}

// cancelIfResting attempts to cancel oid on coin, ignoring "order not
// found" errors so test cleanup is idempotent.
func cancelIfResting(t *testing.T, client *hl.Client, coin string, oid int64) {
	t.Helper()
	if oid == 0 {
		return
	}
	if _, err := client.Trade.Cancel(coin, oid); err != nil {
		t.Logf("Cancel(%s, %d): %v (best-effort)", coin, oid, err)
	}
}

// cleanupCloid attempts to cancel a resting order identified by its
// client order id, ignoring "order not found" errors. Symmetric to
// cancelIfResting for cloid-based tests.
func cleanupCloid(t *testing.T, client *hl.Client, coin, cloid string) {
	t.Helper()
	if cloid == "" {
		return
	}
	if _, err := client.Trade.CancelByCloid(coin, cloid); err != nil {
		t.Logf("CancelByCloid(%s, %s): %v (best-effort)", coin, cloid, err)
	}
}

// pickHIP3Dex returns the HIP-3 dex configured via HL_HIP3_DEX. The
// second return value signals whether the suite should skip — true means
// no dex configured, run a clean t.Skip in the caller.
func pickHIP3Dex(t *testing.T) (string, bool) {
	t.Helper()
	cfg, err := loadConfig()
	if err != nil {
		t.Skipf("integration env not configured: %v", err)
	}
	if cfg.HIP3Dex == "" {
		return "", true
	}
	return cfg.HIP3Dex, false
}

// pickHIP3Coin picks a coin on the HIP-3 dex. Resolves against the
// dex's universe so that HL_HIP3_COIN can be supplied either bare
// ("COPPER") or fully qualified ("xyz:COPPER"); HIP-3 dexes namespace
// their coin names with the dex prefix on the wire. Returns ("", true)
// to signal skip when the dex has no usable assets.
func pickHIP3Coin(t *testing.T, client *hl.Client, dex string) (string, bool) {
	t.Helper()
	cfg, _ := loadConfig()
	meta, err := client.Info.Meta(dex)
	if err != nil {
		t.Logf("Info.Meta(%q) failed: %v", dex, err)
		return "", true
	}
	if meta == nil || len(meta.Universe) == 0 {
		return "", true
	}
	want := cfg.HIP3Coin
	if want == "" {
		return meta.Universe[0].Name, false
	}
	// Accept the user's name as-is, with the dex prefix, or as a suffix
	// match (canonical form). Universe entries are like "xyz:COPPER".
	prefixed := dex + ":" + want
	for _, a := range meta.Universe {
		if a.Name == want || a.Name == prefixed || strings.EqualFold(strings.TrimPrefix(a.Name, dex+":"), want) {
			return a.Name, false
		}
	}
	t.Logf("HL_HIP3_COIN=%q not found in dex %q universe (size=%d, first=%q)",
		want, dex, len(meta.Universe), meta.Universe[0].Name)
	return "", true
}

// multiBucketSide is one bucket of a multi-bucket HIP-4 question
// (e.g. one of the three price ranges of a BTC range market).
type multiBucketSide struct {
	YesCanonical string  // "#<10*outcome+0>"
	NoCanonical  string  // "#<10*outcome+1>"
	Label        string  // human-readable bucket label, e.g. "75348 to 78423"
	Outcome      int     // outcome id of the bucket
	Mid          float64 // YES-side mid in (0, 1); 0 when no live book
}

// pickMultiBucketQuestionOrSkip walks OutcomeMeta.Questions looking for
// a price-bucket question whose NamedOutcomes count meets minBuckets.
// HL_HIP4_OUTCOME pins the search to a specific question name when set.
// Returns the question name and every bucket, ready for trading.
func pickMultiBucketQuestionOrSkip(t *testing.T, client *hl.Client, minBuckets int) (question string, buckets []multiBucketSide) {
	t.Helper()
	meta, err := client.Info.OutcomeMeta()
	if err != nil {
		t.Skipf("OutcomeMeta failed: %v", err)
	}
	if meta == nil {
		t.Skip("no HIP-4 outcome metadata on this environment")
	}
	cfg, _ := loadConfig()
	want := cfg.HIP4Outcome

	for i := range meta.Questions {
		q := &meta.Questions[i]
		if len(q.NamedOutcomes) < minBuckets {
			continue
		}
		if want != "" && q.Name != want {
			continue
		}
		raw := meta.QuestionBuckets(q)
		if raw == nil {
			continue
		}
		out := make([]multiBucketSide, 0, len(raw))
		for _, b := range raw {
			px, err := client.Info.Mid(b.YesCanonical)
			if err != nil || px <= 0 || px >= 1 {
				px = 0
			}
			out = append(out, multiBucketSide{
				YesCanonical: b.YesCanonical,
				NoCanonical:  b.NoCanonical,
				Label:        b.Label,
				Outcome:      b.Outcome,
				Mid:          px,
			})
		}
		return q.Name, out
	}
	t.Skipf("no HIP-4 question with >= %d named outcomes found", minBuckets)
	return "", nil
}

// requireYesOutcomeOrSkip restricts requireOutcomeOrSkip to the YES side
// (sideIdx == 0; SideSpecs is documented as [YES, NO] in that order).
// Use this when the test deliberately wants to long an outcome rather
// than whichever side happens to have a live mid first.
func requireYesOutcomeOrSkip(t *testing.T, client *hl.Client) (canonical, friendly string, midPx float64) {
	t.Helper()
	meta, err := client.Info.OutcomeMeta()
	if err != nil {
		t.Skipf("OutcomeMeta failed: %v", err)
	}
	if meta == nil || len(meta.Outcomes) == 0 {
		t.Skip("no HIP-4 outcomes available on this environment")
	}
	cfg, _ := loadConfig()
	want := cfg.HIP4Outcome
	for _, oc := range meta.Outcomes {
		if len(oc.SideSpecs) == 0 {
			continue
		}
		spec := oc.SideSpecs[0] // YES
		f := oc.Name + ":" + spec.Name
		c := "#" + strconv.Itoa(10*oc.Outcome+0)
		if want != "" && want != f && want != c {
			continue
		}
		px, err := client.Info.Mid(c)
		if err != nil || px <= 0 || px >= 1 {
			continue
		}
		return c, f, px
	}
	t.Skip("no HIP-4 outcome with a live YES-side mid found")
	return "", "", 0
}

// requireQuoteTokenOutcomeOrSkip restricts requireYesOutcomeOrSkip to a
// YES-side outcome whose collateral/settlement token equals quoteToken
// (case-insensitive). Use it to prove a flow works against a specific
// stablecoin — the USDH→USDC migration means a generic "first live
// outcome" could be quoted in either token. Returns the canonical name,
// friendly name and current YES mid, or skips when no live outcome with
// that quote token exists on the environment.
func requireQuoteTokenOutcomeOrSkip(t *testing.T, client *hl.Client, quoteToken string) (canonical, friendly string, midPx float64) {
	t.Helper()
	meta, err := client.Info.OutcomeMeta()
	if err != nil {
		t.Skipf("OutcomeMeta failed: %v", err)
	}
	if meta == nil || len(meta.Outcomes) == 0 {
		t.Skip("no HIP-4 outcomes available on this environment")
	}
	cfg, _ := loadConfig()
	want := cfg.HIP4Outcome
	for _, oc := range meta.Outcomes {
		if !strings.EqualFold(oc.QuoteToken, quoteToken) {
			continue
		}
		if len(oc.SideSpecs) == 0 {
			continue
		}
		spec := oc.SideSpecs[0] // YES
		f := oc.Name + ":" + spec.Name
		c := "#" + strconv.Itoa(10*oc.Outcome+0)
		if want != "" && want != f && want != c {
			continue
		}
		px, err := client.Info.Mid(c)
		if err != nil || px <= 0 || px >= 1 {
			continue
		}
		return c, f, px
	}
	t.Skipf("no HIP-4 outcome quoted in %s with a live YES-side mid found", quoteToken)
	return "", "", 0
}

// requireOutcomeOrSkip looks up an active HIP-4 outcome and returns the
// canonical name ("#<enc>"), friendly name ("<question>:<side>") and the
// current mid price. The caller can place / cancel against the canonical
// name. The test is skipped cleanly when:
//   - OutcomeMeta fails (network or feature unavailable),
//   - no outcomes are returned (env without HIP-4 support),
//   - HL_HIP4_OUTCOME is set but does not match any side.
func requireOutcomeOrSkip(t *testing.T, client *hl.Client) (canonical, friendly string, midPx float64) {
	t.Helper()
	meta, err := client.Info.OutcomeMeta()
	if err != nil {
		t.Skipf("OutcomeMeta failed: %v", err)
	}
	if meta == nil || len(meta.Outcomes) == 0 {
		t.Skip("no HIP-4 outcomes available on this environment")
	}
	cfg, _ := loadConfig()
	want := cfg.HIP4Outcome
	for _, oc := range meta.Outcomes {
		for sideIdx, spec := range oc.SideSpecs {
			f := oc.Name + ":" + spec.Name
			c := "#" + strconv.Itoa(10*oc.Outcome+sideIdx)
			if want != "" && want != f && want != c {
				continue
			}
			// Skip outcomes with no live mid: a Mid() failure here means the
			// outcome is delisted or its book is empty.
			px, err := client.Info.Mid(c)
			if err != nil || px <= 0 || px >= 1 {
				continue
			}
			return c, f, px
		}
	}
	t.Skip("no usable HIP-4 outcome with a live mid found")
	return "", "", 0
}
