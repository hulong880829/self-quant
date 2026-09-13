package types

// OrderType discriminates a limit order from a trigger order. Exactly
// one of Limit or Trigger should be populated.
type OrderType struct {
	Limit   *LimitOrderType   `json:"limit,omitempty"`
	Trigger *TriggerOrderType `json:"trigger,omitempty"`
}

// LimitOrderType holds the time-in-force tag for a limit order.
type LimitOrderType struct {
	Tif string `json:"tif"` // TifAlo, TifIoc, TifGtc
}

// TriggerOrderType describes a trigger (stop) order.
type TriggerOrderType struct {
	TriggerPx float64 `json:"triggerPx"`
	IsMarket  bool    `json:"isMarket"`
	Tpsl      string  `json:"tpsl"` // "tp" or "sl"
}

// BuilderInfo carries the builder address and per-order fee (in basis
// points) used by HIP-3 builder-deployed perp markets.
type BuilderInfo struct {
	Builder string `json:"b" msgpack:"b"`
	Fee     int    `json:"f" msgpack:"f"`
}

// OrderTypeWire is the wire variant of OrderType.
type OrderTypeWire struct {
	Limit   *LimitOrderTypeWire   `json:"limit,omitempty" msgpack:"limit,omitempty"`
	Trigger *TriggerOrderTypeWire `json:"trigger,omitempty" msgpack:"trigger,omitempty"`
}

// LimitOrderTypeWire is the wire variant of LimitOrderType.
type LimitOrderTypeWire struct {
	Tif string `json:"tif" msgpack:"tif"`
}

// TriggerOrderTypeWire is the wire variant of TriggerOrderType.
// TriggerPx is encoded as a string for stable msgpack ordering.
type TriggerOrderTypeWire struct {
	IsMarket  bool   `json:"isMarket" msgpack:"isMarket"`
	TriggerPx string `json:"triggerPx" msgpack:"triggerPx"`
	Tpsl      string `json:"tpsl" msgpack:"tpsl"`
}

// Cloid wraps a client order id string in a typed value.
type Cloid struct {
	Value string
}

// ToRaw returns the underlying client order id string.
func (c Cloid) ToRaw() string {
	return c.Value
}
