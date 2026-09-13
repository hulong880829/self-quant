package types

// OrderSpec is the internal-shape value type produced by the public
// placement constructors (hl.ALO, hl.IOC, hl.GTC, hl.Market, hl.Trigger)
// and consumed by Trader.PlaceMany. All fields are populated by option
// functions; nothing here is meant to be set by callers directly.
type OrderSpec struct {
	// Method records which placement verb produced this spec ("alo", "ioc",
	// "gtc", "market", "trigger", "close", "modify"). validate() uses it to
	// detect options applied to incompatible methods.
	Method string

	Coin       string
	Side       Side
	Size       float64
	Price      float64
	ReduceOnly bool
	Cloid      string

	// Limit-style fields.
	TIF TIF

	// Market-style fields.
	Slippage float64

	// Trigger-style fields.
	TriggerPx float64
	IsMarket  bool

	// Bracket fields.
	TakeProfit float64
	StopLoss   float64
	TPSize     float64
	SLSize     float64
	TPCloid    string
	SLCloid    string

	// Builder pass-through.
	BuilderAddr   string
	BuilderFeeBps int

	// Modify/Close hint.
	OverrideSize float64
	LimitPrice   float64

	// Modify target — set by Modify / ModifyByCloid.
	ModifyOID   int64
	ModifyCloid string

	// SkipValidate suppresses validate() for this spec.
	SkipValidate bool
}
