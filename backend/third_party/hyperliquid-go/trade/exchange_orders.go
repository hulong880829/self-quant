package trade

import (
	"fmt"

	"github.com/Simon-Busch/hyperliquid-go/info"
	"github.com/Simon-Busch/hyperliquid-go/signing"
	"github.com/Simon-Busch/hyperliquid-go/types"
)

// CreateOrderRequest is the wire-adjacent order request consumed by the
// internal order action builders.
type CreateOrderRequest struct {
	Coin          string
	IsBuy         bool
	Price         float64
	Size          float64
	ReduceOnly    bool
	OrderType     types.OrderType
	ClientOrderID *string
}

// OrderStatusResting describes a resting (post-only or unfilled) order entry
// returned in an /exchange response.
type OrderStatusResting struct {
	Oid      int64  `json:"oid"`
	ClientID string `json:"cid"`
	Status   string `json:"status"`
}

// OrderStatusFilled describes a filled order entry returned in an /exchange
// response.
type OrderStatusFilled struct {
	TotalSz string `json:"totalSz"`
	AvgPx   string `json:"avgPx"`
	Oid     int    `json:"oid"`
}

// OrderStatus is one element of an /exchange response's statuses array.
type OrderStatus struct {
	Resting *OrderStatusResting `json:"resting,omitempty"`
	Filled  *OrderStatusFilled  `json:"filled,omitempty"`
	Error   *string             `json:"error,omitempty"`
}

// OrderResponse is the data shape returned by /exchange for an order action.
type OrderResponse struct {
	Statuses types.MixedArray `json:"statuses"`
}

// BulkOrderResponse is the legacy response shape for a bulk order action.
type BulkOrderResponse struct {
	Status string        `json:"status"`
	Data   []OrderStatus `json:"data,omitempty"`
	Error  string        `json:"error,omitempty"`
}

// CancelResponse is the wire response for a single-cancel action.
type CancelResponse struct {
	Status string          `json:"status"`
	Data   *info.OpenOrder `json:"data,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// BulkCancelResponse is the wire response for a bulk-cancel action.
type BulkCancelResponse struct {
	Status string           `json:"status"`
	Data   []info.OpenOrder `json:"data,omitempty"`
	Error  string           `json:"error,omitempty"`
}

// ModifyResponse is the legacy response shape for an order modify action.
type ModifyResponse struct {
	Status string        `json:"status"`
	Data   []OrderStatus `json:"data,omitempty"`
	Error  string        `json:"error,omitempty"`
}

// NewCreateOrderActionWithGrouping is the public wrapper for creating order
// actions. Use when you need the signed action bytes before dispatching --
// for example to forward through Stream.PostAction.
func (c *Client) NewCreateOrderActionWithGrouping(
	orders []CreateOrderRequest,
	info *types.BuilderInfo,
	grouping types.Grouping,
) (signing.OrderAction, error) {
	return newCreateOrderActionWithGrouping(c, orders, info, grouping)
}

// newCreateOrderActionWithGrouping builds an order action allowing a specific grouping.
func newCreateOrderActionWithGrouping(
	c *Client,
	orders []CreateOrderRequest,
	info *types.BuilderInfo,
	grouping types.Grouping,
) (signing.OrderAction, error) {
	orderRequests := make([]signing.OrderWire, len(orders))
	for i, order := range orders {
		asset := c.info.AssetID(order.Coin)
		class := types.ClassifyAsset(asset)

		priceWire, err := PriceToWire(order.Price, asset, c.info, class)
		if err != nil {
			return signing.OrderAction{}, fmt.Errorf("failed to wire price for order %d: %w", i, err)
		}

		sizeWire, err := sizeToWireWithAsset(order.Size, asset, c.info)
		if err != nil {
			return signing.OrderAction{}, fmt.Errorf("failed to wire size for order %d: %w", i, err)
		}

		var orderTypeWire types.OrderTypeWire
		if order.OrderType.Limit != nil {
			orderTypeWire.Limit = &types.LimitOrderTypeWire{Tif: order.OrderType.Limit.Tif}
		} else if order.OrderType.Trigger != nil {
			triggerPxWire, err := PriceToWire(order.OrderType.Trigger.TriggerPx, asset, c.info, class)
			if err != nil {
				return signing.OrderAction{}, fmt.Errorf("failed to wire trigger price for order %d: %w", i, err)
			}
			orderTypeWire.Trigger = &types.TriggerOrderTypeWire{
				TriggerPx: triggerPxWire,
				IsMarket:  order.OrderType.Trigger.IsMarket,
				Tpsl:      order.OrderType.Trigger.Tpsl,
			}
		}

		orderRequests[i] = signing.OrderWire{
			Asset:      asset,
			IsBuy:      order.IsBuy,
			LimitPx:    priceWire,
			Size:       sizeWire,
			ReduceOnly: order.ReduceOnly,
			OrderType:  orderTypeWire,
			Cloid:      order.ClientOrderID,
		}
	}

	// The order action does NOT carry the builder dex name on the wire —
	// HIP-3 routing is encoded in the asset id itself. Including a `dex`
	// field makes Hyperliquid reject the action with a JSON-shape error.
	return signing.OrderAction{
		Type:     "order",
		Orders:   orderRequests,
		Grouping: string(grouping),
		Builder:  info,
	}, nil
}

// ModifyOrderRequest pairs an existing order identifier with the
// replacement order definition.
type ModifyOrderRequest struct {
	Oid   any // can be int64 or Cloid
	Order CreateOrderRequest
}

// newModifyOrderAction builds a ModifyAction from a single ModifyOrderRequest.
func newModifyOrderAction(
	c *Client,
	modifyRequest ModifyOrderRequest,
) (signing.ModifyAction, error) {
	asset := c.info.AssetID(modifyRequest.Order.Coin)
	class := types.ClassifyAsset(asset)

	priceWire, err := PriceToWire(modifyRequest.Order.Price, asset, c.info, class)
	if err != nil {
		return signing.ModifyAction{}, fmt.Errorf("failed to wire price: %w", err)
	}

	sizeWire, err := sizeToWireWithAsset(modifyRequest.Order.Size, asset, c.info)
	if err != nil {
		return signing.ModifyAction{}, fmt.Errorf("failed to wire size: %w", err)
	}

	var orderTypeWire types.OrderTypeWire
	if modifyRequest.Order.OrderType.Limit != nil {
		orderTypeWire.Limit = &types.LimitOrderTypeWire{Tif: modifyRequest.Order.OrderType.Limit.Tif}
	} else if modifyRequest.Order.OrderType.Trigger != nil {
		triggerPxWire, err := PriceToWire(modifyRequest.Order.OrderType.Trigger.TriggerPx, asset, c.info, class)
		if err != nil {
			return signing.ModifyAction{}, fmt.Errorf("failed to wire trigger price: %w", err)
		}
		orderTypeWire.Trigger = &types.TriggerOrderTypeWire{
			TriggerPx: triggerPxWire,
			IsMarket:  modifyRequest.Order.OrderType.Trigger.IsMarket,
			Tpsl:      modifyRequest.Order.OrderType.Trigger.Tpsl,
		}
	}

	return signing.ModifyAction{
		Type: "modify",
		Oid:  modifyRequest.Oid,
		Order: signing.OrderWire{
			Asset:      asset,
			IsBuy:      modifyRequest.Order.IsBuy,
			LimitPx:    priceWire,
			Size:       sizeWire,
			ReduceOnly: modifyRequest.Order.ReduceOnly,
			OrderType:  orderTypeWire,
			Cloid:      modifyRequest.Order.ClientOrderID,
		},
	}, nil
}
