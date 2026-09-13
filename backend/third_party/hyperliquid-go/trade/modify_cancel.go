package trade

import (
	"fmt"

	"github.com/Simon-Busch/hyperliquid-go/types"
)

// CancelRequest names a single order to cancel by exchange oid.
type CancelRequest struct {
	Coin string `json:"coin"`
	Oid  int64  `json:"oid"`
}

// CancelByCloidRequest names a single order to cancel by client order id.
type CancelByCloidRequest struct {
	Coin  string `json:"coin"`
	Cloid string `json:"cloid"`
}

// Modify changes the price (or size, or both) of a resting order identified
// by oid. The coin is preserved on the existing order — only the supplied
// fields change. Required: WithLimit(newPx) for a new price, WithSize(x)
// for a new size, or both.
func (c *Client) Modify(oid int64, opts ...PlaceOpt) (types.Result, error) {
	spec := types.OrderSpec{Method: "modify", ModifyOID: oid, TIF: types.TifGtc}
	for _, o := range opts {
		o(&spec)
	}
	return c.doModify(&spec)
}

// ModifyByCloid changes a resting order identified by its client order id.
func (c *Client) ModifyByCloid(cloid string, opts ...PlaceOpt) (types.Result, error) {
	spec := types.OrderSpec{Method: "modify", ModifyCloid: cloid, TIF: types.TifGtc}
	for _, o := range opts {
		o(&spec)
	}
	return c.doModify(&spec)
}

func (c *Client) doModify(spec *types.OrderSpec) (types.Result, error) {
	if err := c.validate(spec); err != nil {
		return types.Result{}, err
	}
	if spec.LimitPrice > 0 {
		spec.Price = spec.LimitPrice
	}
	if spec.OverrideSize > 0 {
		spec.Size = spec.OverrideSize
	}
	// Modify is a cancel + replace under the hood; default the
	// replacement TIF to ALO (post-only) so a far-from-mid resting order
	// stays inside Hyperliquid's price-band rules. A future WithTIF
	// option can let callers override when modifying a GTC/IOC order.
	req := CreateOrderRequest{
		Coin:       spec.Coin,
		IsBuy:      spec.Side.IsBuy(),
		Price:      spec.Price,
		Size:       spec.Size,
		ReduceOnly: spec.ReduceOnly,
		OrderType:  types.OrderType{Limit: &types.LimitOrderType{Tif: string(types.TifAlo)}},
	}
	if spec.Cloid != "" {
		ci := spec.Cloid
		req.ClientOrderID = &ci
	}
	var oidAny any
	if spec.ModifyOID != 0 {
		oidAny = spec.ModifyOID
	} else {
		oidAny = spec.ModifyCloid
	}
	action, err := newModifyOrderAction(c, ModifyOrderRequest{Oid: oidAny, Order: req})
	if err != nil {
		return types.Result{}, fmt.Errorf("failed to create modify action: %w", err)
	}
	resp := types.APIResponse[OrderResponse]{}
	if err := c.executeAction(action, &resp); err != nil {
		return types.Result{}, fmt.Errorf("failed to modify order: %w", err)
	}
	if !resp.Ok {
		return types.Result{}, fmt.Errorf("failed to modify order: %s", resp.Err)
	}
	if len(resp.Data.Statuses) == 0 {
		return types.Result{}, fmt.Errorf("no status for modified order: %s", resp.Err)
	}
	first := resp.Data.Statuses[0]
	if first.Type() != "object" {
		return types.Result{}, fmt.Errorf("unexpected status type: %s", first.Type())
	}
	var status OrderStatus
	if err := first.Parse(&status); err != nil {
		return types.Result{}, fmt.Errorf("failed to parse modified order status: %w", err)
	}
	r := types.Result{}
	if status.Resting != nil {
		r.OID = status.Resting.Oid
		r.Cloid = status.Resting.ClientID
		r.Status = status.Resting.Status
	}
	if status.Filled != nil {
		r.OID = int64(status.Filled.Oid)
		r.AvgPx = status.Filled.AvgPx
		r.TotalSz = status.Filled.TotalSz
		r.Status = "filled"
	}
	if status.Error != nil {
		r.Error = *status.Error
	}
	return r, nil
}

// CancelAll cancels every open order across the supplied coins. With no
// coins supplied it cancels everything across every asset.
func (c *Client) CancelAll(coins ...string) (types.BatchCancelResult, error) {
	addr := c.accountAddr
	if addr == "" {
		addr = c.vault
	}
	orders, err := c.info.OpenOrders(addr)
	if err != nil {
		return types.BatchCancelResult{}, err
	}
	keep := func(coin string) bool {
		if len(coins) == 0 {
			return true
		}
		for _, k := range coins {
			if k == coin {
				return true
			}
		}
		return false
	}
	reqs := make([]CancelOrderRequest, 0, len(orders))
	for _, o := range orders {
		if !keep(o.Coin) {
			continue
		}
		reqs = append(reqs, CancelOrderRequest{Coin: o.Coin, OrderID: o.Oid})
	}
	if len(reqs) == 0 {
		return types.BatchCancelResult{}, nil
	}
	resp, err := c.bulkCancel(reqs)
	if err != nil {
		return types.BatchCancelResult{}, err
	}
	return cancelBatchFromResponse(resp), nil
}

// cancelBatchFromResponse maps a bulk-cancel APIResponse into a
// BatchCancelResult, one entry per status returned by the server.
func cancelBatchFromResponse(resp *types.APIResponse[CancelOrderResponse]) types.BatchCancelResult {
	br := types.BatchCancelResult{}
	if resp == nil {
		return br
	}
	if !resp.Ok {
		br.Error = resp.Err
		return br
	}
	for _, s := range resp.Data.Statuses {
		br.Results = append(br.Results, types.CancelResult{Status: string(s)})
	}
	return br
}
