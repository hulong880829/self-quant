package trade

import "github.com/Simon-Busch/hyperliquid-go/types"

// PlaceOpt is the option function type consumed by the placement verbs
// (PlaceALO/IOC/GTC/Market/Trigger, ClosePosition, Modify, the ALO etc.
// constructors). Each option mutates one or more fields on the supplied
// OrderSpec; options never report errors directly — invalid combinations
// surface from validate() at place() time.
type PlaceOpt func(*types.OrderSpec)

// WithTakeProfit attaches a reduce-only take-profit trigger order at price.
func WithTakeProfit(price float64) PlaceOpt {
	return func(s *types.OrderSpec) { s.TakeProfit = price }
}

// WithStopLoss attaches a reduce-only stop-loss trigger order at price.
func WithStopLoss(price float64) PlaceOpt {
	return func(s *types.OrderSpec) { s.StopLoss = price }
}

// WithBracket is a shortcut for WithTakeProfit(tp) plus WithStopLoss(sl).
func WithBracket(tp, sl float64) PlaceOpt {
	return func(s *types.OrderSpec) {
		s.TakeProfit = tp
		s.StopLoss = sl
	}
}

// WithReduceOnly marks the order as reduce-only.
func WithReduceOnly() PlaceOpt {
	return func(s *types.OrderSpec) { s.ReduceOnly = true }
}

// WithCloid pins a client-supplied order id. The value must be a 16-byte
// identifier encoded as 32 hex characters with a "0x" prefix.
func WithCloid(cloid string) PlaceOpt {
	return func(s *types.OrderSpec) { s.Cloid = cloid }
}

// WithBuilder attaches a HIP-1 builder fee referral.
func WithBuilder(addr string, feeBps int) PlaceOpt {
	return func(s *types.OrderSpec) {
		s.BuilderAddr = addr
		s.BuilderFeeBps = feeBps
	}
}

// WithSlippage bounds the worst-case fill price for PlaceMarket as a
// fraction of mid (0.05 = 5%).
func WithSlippage(frac float64) PlaceOpt {
	return func(s *types.OrderSpec) { s.Slippage = frac }
}

// WithSize overrides the order size — used by ClosePosition (partial close)
// and Modify (resize).
func WithSize(size float64) PlaceOpt {
	return func(s *types.OrderSpec) { s.OverrideSize = size }
}

// WithLimit turns ClosePosition into a limit close at the given price, or
// updates Modify's target price.
func WithLimit(price float64) PlaceOpt {
	return func(s *types.OrderSpec) { s.LimitPrice = price }
}

// AsMarket forces PlaceTrigger to fill as a market when triggered.
func AsMarket() PlaceOpt {
	return func(s *types.OrderSpec) { s.IsMarket = true }
}

// AsLimit forces PlaceTrigger to rest as a limit at px when triggered.
func AsLimit(px float64) PlaceOpt {
	return func(s *types.OrderSpec) {
		s.IsMarket = false
		s.Price = px
	}
}

// WithTPSize sets a partial-fill size for the bracket take-profit leg.
func WithTPSize(size float64) PlaceOpt {
	return func(s *types.OrderSpec) { s.TPSize = size }
}

// WithSLSize sets a partial-fill size for the bracket stop-loss leg.
func WithSLSize(size float64) PlaceOpt {
	return func(s *types.OrderSpec) { s.SLSize = size }
}

// WithTPCloid pins a client-supplied order id on the bracket take-profit leg.
func WithTPCloid(cloid string) PlaceOpt {
	return func(s *types.OrderSpec) { s.TPCloid = cloid }
}

// WithSLCloid pins a client-supplied order id on the bracket stop-loss leg.
func WithSLCloid(cloid string) PlaceOpt {
	return func(s *types.OrderSpec) { s.SLCloid = cloid }
}

// SkipValidation bypasses the validate() pre-flight; use only when the
// caller has its own checks or runs against a network with no metadata
// cache.
func SkipValidation() PlaceOpt {
	return func(s *types.OrderSpec) { s.SkipValidate = true }
}
