package trade

import (
	"encoding/json"
	"fmt"

	"github.com/Simon-Busch/hyperliquid-go/signing"
	"github.com/Simon-Busch/hyperliquid-go/types"
)

type (
	// CancelOrderRequest identifies a single order to cancel by exchange oid.
	CancelOrderRequest struct {
		Coin    string
		OrderID int64
	}

	// CancelOrderResponse holds the per-cancel statuses returned by the
	// /exchange cancel action.
	CancelOrderResponse struct {
		Statuses types.MixedArray
	}
)

// Cancel cancels the resting order with the given exchange oid. A
// venue-side per-order error (already cancelled, filled, never placed)
// surfaces as a typed Go error rather than being buried in the result
// Status field.
func (c *Client) Cancel(coin string, oid int64) (types.CancelResult, error) {
	resp, err := c.bulkCancel([]CancelOrderRequest{{Coin: coin, OrderID: oid}})
	if err != nil {
		return types.CancelResult{}, err
	}
	return cancelResultOrError(resp)
}

// bulkCancel signs and submits a cancel action for every request.
func (c *Client) bulkCancel(requests []CancelOrderRequest) (*types.APIResponse[CancelOrderResponse], error) {
	cancels := make([]signing.CancelOrderWire, len(requests))
	for i, req := range requests {
		cancels[i] = signing.CancelOrderWire{
			Asset:   c.info.AssetID(req.Coin),
			OrderID: req.OrderID,
		}
	}

	action := signing.CancelAction{
		Type:    "cancel",
		Cancels: cancels,
	}

	var res *types.APIResponse[CancelOrderResponse]
	if err := c.executeAction(action, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// CancelOrderRequestByCloid identifies a single order to cancel by client id.
type CancelOrderRequestByCloid struct {
	Coin  string
	Cloid string
}

// CancelByCloid cancels the resting order with the given client order id.
// Per-order errors are surfaced as Go errors, same as Cancel.
func (c *Client) CancelByCloid(coin, cloid string) (types.CancelResult, error) {
	cancels := []signing.CancelByCloidWire{{Asset: c.info.AssetID(coin), ClientID: cloid}}
	action := signing.CancelByCloidAction{
		Type:    "cancelByCloid",
		Cancels: cancels,
	}
	var res *types.APIResponse[CancelOrderResponse]
	if err := c.executeAction(action, &res); err != nil {
		return types.CancelResult{}, err
	}
	return cancelResultOrError(res)
}

// cancelResultOrError inspects the first per-order status from a cancel
// response. A bare string status ("success") becomes a CancelResult with
// no error; an object status with an "error" field becomes a Go error
// carrying the venue's message verbatim, so callers can distinguish
// idempotent re-cancels from genuine successes.
func cancelResultOrError(resp *types.APIResponse[CancelOrderResponse]) (types.CancelResult, error) {
	if resp == nil {
		return types.CancelResult{}, fmt.Errorf("cancel: empty response")
	}
	if !resp.Ok {
		return types.CancelResult{Error: resp.Err}, fmt.Errorf("cancel: %s", resp.Err)
	}
	if len(resp.Data.Statuses) == 0 {
		return types.CancelResult{}, nil
	}
	first := resp.Data.Statuses[0]
	// Try to decode as an error object first; on failure treat as a
	// plain string status.
	var errObj struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(first, &errObj); err == nil && errObj.Error != "" {
		return types.CancelResult{Status: string(first), Error: errObj.Error},
			fmt.Errorf("cancel: %s", errObj.Error)
	}
	return types.CancelResult{Status: string(first)}, nil
}
