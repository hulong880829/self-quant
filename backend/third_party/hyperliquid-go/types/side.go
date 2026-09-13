package types

// Side represents the direction of an order.
type Side string

const (
	// Buy is the long side; corresponds to "B" on the wire.
	Buy Side = "B"
	// Sell is the short side; corresponds to "A" on the wire.
	Sell Side = "A"

	// SideBid is retained for compatibility with API responses that use the
	// wire encoding directly. Equivalent to Buy.
	SideBid Side = "B"
	// SideAsk is retained for compatibility with API responses that use the
	// wire encoding directly. Equivalent to Sell.
	SideAsk Side = "A"
)

// IsBuy reports whether s is the buy/long side.
func (s Side) IsBuy() bool { return s == Buy }

// TIF identifies the time-in-force of a limit order at wire level.
type TIF string

const (
	tifALO TIF = "Alo"
	tifIOC TIF = "Ioc"
	tifGTC TIF = "Gtc"
)

// MarginMode identifies cross vs isolated margin.
type MarginMode int

const (
	// Cross is the cross-margin mode (default).
	Cross MarginMode = iota
	// Isolated is the per-position isolated-margin mode.
	Isolated
)
