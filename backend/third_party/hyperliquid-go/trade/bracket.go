package trade

import "github.com/Simon-Busch/hyperliquid-go/types"

// bracketOrders builds the TP and SL trigger CreateOrderRequests that bracket
// a parent OrderSpec. Returns an empty slice if no bracket fields are set.
// The trigger legs are always reduce-only and IsMarket=true.
func bracketOrders(spec *types.OrderSpec) []CreateOrderRequest {
	if spec.TakeProfit == 0 && spec.StopLoss == 0 {
		return nil
	}
	out := make([]CreateOrderRequest, 0, 2)
	exitSide := !spec.Side.IsBuy()
	if spec.TakeProfit > 0 {
		sz := spec.Size
		if spec.TPSize > 0 {
			sz = spec.TPSize
		}
		req := CreateOrderRequest{
			Coin:       spec.Coin,
			IsBuy:      exitSide,
			Price:      spec.TakeProfit,
			Size:       sz,
			ReduceOnly: true,
			OrderType: types.OrderType{
				Trigger: &types.TriggerOrderType{
					TriggerPx: spec.TakeProfit,
					IsMarket:  true,
					Tpsl:      "tp",
				},
			},
		}
		if spec.TPCloid != "" {
			cloid := spec.TPCloid
			req.ClientOrderID = &cloid
		}
		out = append(out, req)
	}
	if spec.StopLoss > 0 {
		sz := spec.Size
		if spec.SLSize > 0 {
			sz = spec.SLSize
		}
		req := CreateOrderRequest{
			Coin:       spec.Coin,
			IsBuy:      exitSide,
			Price:      spec.StopLoss,
			Size:       sz,
			ReduceOnly: true,
			OrderType: types.OrderType{
				Trigger: &types.TriggerOrderType{
					TriggerPx: spec.StopLoss,
					IsMarket:  true,
					Tpsl:      "sl",
				},
			},
		}
		if spec.SLCloid != "" {
			cloid := spec.SLCloid
			req.ClientOrderID = &cloid
		}
		out = append(out, req)
	}
	return out
}
