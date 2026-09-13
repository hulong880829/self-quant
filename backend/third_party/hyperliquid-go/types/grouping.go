package types

// Grouping is the order-grouping discriminator used by /exchange order
// actions. Distinct groupings allow TP/SL trigger legs to attach to a
// parent or to an existing position.
type Grouping string

const (
	// GroupingNA is the default (no grouping).
	GroupingNA Grouping = "na"
	// GroupingNormalTpsl groups TP/SL legs with their parent order.
	GroupingNormalTpsl Grouping = "normalTpsl"
	// GroupingPositionTpls binds TP/SL legs to an existing position.
	GroupingPositionTpls Grouping = "positionTpsl"
)

// DefaultSlippage is the default worst-case fill slippage for PlaceMarket (5%).
const DefaultSlippage = 0.05

// Order Time-in-Force constants (exported, string-valued).
const (
	TifAlo = "Alo" // Add Liquidity Only
	TifIoc = "Ioc" // Immediate or Cancel
	TifGtc = "Gtc" // Good Till Cancel
)
