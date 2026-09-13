package trade

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/Simon-Busch/hyperliquid-go/types"
)

// SlippagePrice computes the worst-case fill price for a market order on
// name using the supplied slippage fraction. When px is non-nil it
// substitutes for the live mid price.
func (c *Client) SlippagePrice(
	name string,
	isBuy bool,
	slippage float64,
	px *float64,
) (float64, error) {
	var price float64

	if px != nil {
		price = *px
	} else {
		// HIP-3 coins are prefixed "<dex>:<coin>" and only appear in
		// the mid table when AllMids is called with that dex.
		var midsDex []string
		if idx := strings.Index(name, ":"); idx > 0 {
			midsDex = []string{name[:idx]}
		}
		mids, err := c.info.AllMids(midsDex...)
		if err != nil {
			return 0, err
		}
		if midPriceStr, ok := mids[name]; ok {
			price, err = strconv.ParseFloat(midPriceStr, 64)
			if err != nil {
				return 0, fmt.Errorf("failed to parse midprice for %s: %w", name, err)
			}
		} else {
			return 0, fmt.Errorf("no midprice found for %s", name)
		}
	}

	if isBuy {
		price = price * (1 + slippage)
	} else {
		price = price * (1 - slippage)
	}

	asset := c.info.AssetID(name)
	szDecimals := c.info.SzDecimals(asset)
	class := types.ClassifyAsset(asset)

	// HIP-4 outcome prices are bounded by the venue to (0, 1]; an
	// aggressive slippage multiplier on a near-1.0 buy mid will produce
	// a price the venue rejects with "Price too large". Clamp to the
	// tick-aligned bounds (0.0001 .. 0.9999) before tick rounding so a
	// post-rounding tick step can't push it back out of range.
	if class == types.AssetClassOutcome {
		const (
			outcomeMaxPrice = 0.9999
			outcomeMinPrice = 0.0001
		)
		if price > outcomeMaxPrice {
			price = outcomeMaxPrice
		}
		if price < outcomeMinPrice {
			price = outcomeMinPrice
		}
	}

	price = formatPriceToTickSize(price, szDecimals, class)

	adjustedPrice, err := validateAndAdjustPrice(price, asset)
	if err != nil {
		return 0, fmt.Errorf("failed to validate price for tick size: %w", err)
	}

	return adjustedPrice, nil
}

// PlaceMany packages multiple OrderSpec legs into a single signed action.
// Use the ALO/IOC/GTC/Market/Trigger constructors to build the specs.
func (c *Client) PlaceMany(orders ...types.OrderSpec) (types.BatchResult, error) {
	return c.placeMany(orders)
}

// PlaceALO places an add-liquidity-only (post-only) limit order. Required
// args are positional; everything else is supplied via options.
func (c *Client) PlaceALO(coin string, side types.Side, size, px float64, opts ...PlaceOpt) (types.Result, error) {
	spec := ALO(coin, side, size, px, opts...)
	return c.place(&spec)
}

// PlaceALOWS is PlaceALO submitted over the websocket action transport.
func (c *Client) PlaceALOWS(ctx context.Context, coin string, side types.Side, size, px float64, opts ...PlaceOpt) (types.Result, error) {
	spec := ALO(coin, side, size, px, opts...)
	return c.placeWS(ctx, &spec)
}

// PlaceIOC places an immediate-or-cancel limit order.
func (c *Client) PlaceIOC(coin string, side types.Side, size, px float64, opts ...PlaceOpt) (types.Result, error) {
	spec := IOC(coin, side, size, px, opts...)
	return c.place(&spec)
}

// PlaceIOCWS is PlaceIOC submitted over the websocket action transport.
func (c *Client) PlaceIOCWS(ctx context.Context, coin string, side types.Side, size, px float64, opts ...PlaceOpt) (types.Result, error) {
	spec := IOC(coin, side, size, px, opts...)
	return c.placeWS(ctx, &spec)
}

// PlaceGTC places a good-til-cancel limit order.
func (c *Client) PlaceGTC(coin string, side types.Side, size, px float64, opts ...PlaceOpt) (types.Result, error) {
	spec := GTC(coin, side, size, px, opts...)
	return c.place(&spec)
}

// PlaceGTCWS is PlaceGTC submitted over the websocket action transport.
func (c *Client) PlaceGTCWS(ctx context.Context, coin string, side types.Side, size, px float64, opts ...PlaceOpt) (types.Result, error) {
	spec := GTC(coin, side, size, px, opts...)
	return c.placeWS(ctx, &spec)
}

// ALO returns an OrderSpec describing a post-only limit order. Pass it to
// Client.PlaceMany to batch multiple legs into one signed action.
func ALO(coin string, side types.Side, size, px float64, opts ...PlaceOpt) types.OrderSpec {
	s := types.OrderSpec{Method: "alo", Coin: coin, Side: side, Size: size, Price: px, TIF: types.TifAlo}
	for _, o := range opts {
		o(&s)
	}
	return s
}

// IOC returns an OrderSpec describing an immediate-or-cancel limit order.
func IOC(coin string, side types.Side, size, px float64, opts ...PlaceOpt) types.OrderSpec {
	s := types.OrderSpec{Method: "ioc", Coin: coin, Side: side, Size: size, Price: px, TIF: types.TifIoc}
	for _, o := range opts {
		o(&s)
	}
	return s
}

// GTC returns an OrderSpec describing a good-til-cancel limit order.
func GTC(coin string, side types.Side, size, px float64, opts ...PlaceOpt) types.OrderSpec {
	s := types.OrderSpec{Method: "gtc", Coin: coin, Side: side, Size: size, Price: px, TIF: types.TifGtc}
	for _, o := range opts {
		o(&s)
	}
	return s
}

// PlaceMarket places a market order, implemented as an IOC limit at the
// current mid price adjusted by the requested slippage fraction (default
// 5% if WithSlippage is not supplied).
func (c *Client) PlaceMarket(coin string, side types.Side, size float64, opts ...PlaceOpt) (types.Result, error) {
	spec := Market(coin, side, size, opts...)
	slippage := spec.Slippage
	if slippage == 0 {
		slippage = 0.05
	}
	px, err := c.SlippagePrice(coin, side.IsBuy(), slippage, nil)
	if err != nil {
		return types.Result{}, err
	}
	spec.Price = px
	return c.place(&spec)
}

// PlaceMarketWS is PlaceMarket submitted over the websocket action transport.
func (c *Client) PlaceMarketWS(ctx context.Context, coin string, side types.Side, size float64, opts ...PlaceOpt) (types.Result, error) {
	spec := Market(coin, side, size, opts...)
	slippage := spec.Slippage
	if slippage == 0 {
		slippage = 0.05
	}
	px, err := c.SlippagePrice(coin, side.IsBuy(), slippage, nil)
	if err != nil {
		return types.Result{}, err
	}
	spec.Price = px
	return c.placeWS(ctx, &spec)
}

// Market returns an OrderSpec describing a market order. The Price field is
// resolved later (against mid) when the spec is consumed by PlaceMany or
// PlaceMarket; callers do not need to supply px.
func Market(coin string, side types.Side, size float64, opts ...PlaceOpt) types.OrderSpec {
	s := types.OrderSpec{Method: "market", Coin: coin, Side: side, Size: size, TIF: types.TifIoc}
	for _, o := range opts {
		o(&s)
	}
	return s
}

// PlaceTrigger places a trigger order (stop-market by default, or
// stop-limit when AsLimit(px) is supplied).
func (c *Client) PlaceTrigger(coin string, side types.Side, size, triggerPx float64, opts ...PlaceOpt) (types.Result, error) {
	spec := Trigger(coin, side, size, triggerPx, opts...)
	return c.place(&spec)
}

// ClosePosition flattens the caller's open position on coin. The order
// side is inferred from the cached UserState: long positions exit with a
// sell, short with a buy. By default this is a market close; supply
// WithLimit(px) to close at a specific price and WithSize(x) to close
// partially. If position state cannot be fetched (e.g. agent address
// mismatch) the call returns a ValidationError{Code:"no_position"} via
// validate().
func (c *Client) ClosePosition(coin string, opts ...PlaceOpt) (types.Result, error) {
	if err := c.RefreshState(context.Background()); err != nil {
		return types.Result{}, err
	}
	state := c.cachedUserState()
	var szi float64
	if state != nil {
		_, szi = positionFor(state, coin)
	}

	isBuy := szi < 0
	size := math.Abs(szi)

	// Apply options to a placeholder spec to surface WithSize/WithLimit/etc.
	tmp := types.OrderSpec{Method: "close", Coin: coin, Size: size, Side: sideFromIsBuy(isBuy), TIF: types.TifIoc}
	for _, o := range opts {
		o(&tmp)
	}
	if tmp.OverrideSize > 0 {
		tmp.Size = tmp.OverrideSize
	}

	var price float64
	if tmp.LimitPrice > 0 {
		price = tmp.LimitPrice
	} else {
		slip := tmp.Slippage
		if slip == 0 {
			slip = 0.05
		}
		p, err := c.SlippagePrice(coin, isBuy, slip, nil)
		if err != nil {
			return types.Result{}, err
		}
		price = p
	}
	tmp.Price = price
	tmp.ReduceOnly = true
	return c.place(&tmp)
}

// sideFromIsBuy is a tiny adapter used while we still convert between the
// boolean wire encoding and the typed Side enum.
func sideFromIsBuy(isBuy bool) types.Side {
	if isBuy {
		return types.Buy
	}
	return types.Sell
}

// Trigger returns an OrderSpec describing a trigger order. Default fills
// as a market; combine with AsLimit(px) to fill as a limit.
func Trigger(coin string, side types.Side, size, triggerPx float64, opts ...PlaceOpt) types.OrderSpec {
	s := types.OrderSpec{
		Method:    "trigger",
		Coin:      coin,
		Side:      side,
		Size:      size,
		TriggerPx: triggerPx,
		Price:     triggerPx,
		IsMarket:  true,
	}
	for _, o := range opts {
		o(&s)
	}
	return s
}
