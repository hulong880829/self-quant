package trade

import (
	"fmt"
	"math"
	"strings"

	"github.com/Simon-Busch/hyperliquid-go/info"
	"github.com/Simon-Busch/hyperliquid-go/types"
)

// ConvertGroup is the spot-conversion subgroup on Client. Conversions
// are implemented as IOC trades on the relevant spot pair — there is
// no dedicated venue endpoint for token-to-token swaps.
type ConvertGroup struct {
	t *Client
}

// USDCToUSDH converts spot USDC into USDH by IOC-buying the USDH/USDC
// spot pair. usdcAmount is the USDC notional to spend; the realised
// USDH amount is approximately usdcAmount / mid minus slippage.
//
// The default slippage is 5%. Fails with a typed error when the
// USDH/USDC pair is missing from the spot universe, when the mid is
// unavailable, or when the venue rejects the order.
func (g *ConvertGroup) USDCToUSDH(usdcAmount float64) (types.Result, error) {
	return g.t.convertSpot("USDH", "USDC", types.Buy, usdcAmount)
}

// USDHToUSDC converts USDH back into spot USDC by IOC-selling the
// USDH/USDC spot pair. usdhAmount is the USDH amount to sell; the
// realised USDC is approximately usdhAmount * mid minus slippage.
//
// Pass usdhAmount in USDH units (not USDC) to make the intent explicit
// at the call site.
func (g *ConvertGroup) USDHToUSDC(usdhAmount float64) (types.Result, error) {
	return g.t.convertSpotSize("USDH", "USDC", types.Sell, usdhAmount)
}

// convertSpot finds the base/quote spot pair and submits an IOC market
// order. amount is the QUOTE-side notional (the user spends this much
// of `quote` to receive `base` on a Buy, or receives this much of
// `quote` from selling `base` on a Sell). Size is derived from the
// current mid and snapped down to the pair's size step so the validator
// does not reject for size_step_violation.
func (c *Client) convertSpot(base, quote string, side types.Side, quoteAmount float64) (types.Result, error) {
	if quoteAmount <= 0 {
		return types.Result{}, fmt.Errorf("convertSpot: amount must be > 0, got %v", quoteAmount)
	}
	pair, err := c.findSpotPair(base, quote)
	if err != nil {
		return types.Result{}, err
	}
	mid, err := c.info.Mid(pair)
	if err != nil {
		return types.Result{}, fmt.Errorf("convertSpot: mid for %s: %w", pair, err)
	}
	if mid <= 0 {
		return types.Result{}, fmt.Errorf("convertSpot: non-positive mid %v for %s", mid, pair)
	}
	size, err := snapSpotSize(c.info, pair, quoteAmount/mid)
	if err != nil {
		return types.Result{}, err
	}
	return c.PlaceMarket(pair, side, size, WithSlippage(0.05))
}

// convertSpotSize is the BASE-quantity twin of convertSpot. baseAmount is
// already in base-token units (no mid lookup needed for size), which is
// the natural shape when the caller knows the holding they want to
// liquidate (USDH -> USDC, where they hold X USDH).
func (c *Client) convertSpotSize(base, quote string, side types.Side, baseAmount float64) (types.Result, error) {
	if baseAmount <= 0 {
		return types.Result{}, fmt.Errorf("convertSpot: amount must be > 0, got %v", baseAmount)
	}
	pair, err := c.findSpotPair(base, quote)
	if err != nil {
		return types.Result{}, err
	}
	size, err := snapSpotSize(c.info, pair, baseAmount)
	if err != nil {
		return types.Result{}, err
	}
	return c.PlaceMarket(pair, side, size, WithSlippage(0.05))
}

// snapSpotSize rounds size DOWN to the pair's MinSize step. Rounding
// down rather than up: the caller's amount is the maximum they want to
// spend or sell, and overshooting could drain a thin wallet. Returns
// an error when the pair has unknown metadata or when the snapped size
// is below one step.
func snapSpotSize(infoC *info.Client, pair string, size float64) (float64, error) {
	meta, err := infoC.Asset(pair)
	if err != nil {
		return 0, fmt.Errorf("snapSpotSize: asset meta for %s: %w", pair, err)
	}
	if meta.MinSize <= 0 {
		return size, nil
	}
	steps := math.Floor(size / meta.MinSize)
	if steps < 1 {
		return 0, fmt.Errorf("snapSpotSize: %v is below the %s step %v", size, pair, meta.MinSize)
	}
	return steps * meta.MinSize, nil
}

// findSpotPair returns the venue-side name of the spot pair whose base
// token is `base` and whose quote token is `quote`. Lookup goes via the
// spot universe so it works for any token combination the venue
// exposes; the test for USDH/USDC was the motivator.
func (c *Client) findSpotPair(base, quote string) (string, error) {
	sm, err := c.info.SpotMeta()
	if err != nil {
		return "", fmt.Errorf("findSpotPair: %w", err)
	}
	baseIdx, quoteIdx := -1, -1
	for _, tok := range sm.Tokens {
		if strings.EqualFold(tok.Name, base) {
			baseIdx = tok.Index
		}
		if strings.EqualFold(tok.Name, quote) {
			quoteIdx = tok.Index
		}
	}
	if baseIdx < 0 {
		return "", fmt.Errorf("findSpotPair: %q not in spot tokens", base)
	}
	if quoteIdx < 0 {
		return "", fmt.Errorf("findSpotPair: %q not in spot tokens", quote)
	}
	for _, p := range sm.Universe {
		if len(p.Tokens) < 2 {
			continue
		}
		if p.Tokens[0] == baseIdx && p.Tokens[1] == quoteIdx {
			return p.Name, nil
		}
	}
	return "", fmt.Errorf("findSpotPair: no spot pair with base=%s quote=%s", base, quote)
}
