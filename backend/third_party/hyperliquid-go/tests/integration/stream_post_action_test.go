//go:build integration

package integration

import (
	"testing"
	"time"


	"github.com/Simon-Busch/hyperliquid-go/types"
	"github.com/Simon-Busch/hyperliquid-go/trade"
	"github.com/Simon-Busch/hyperliquid-go/signing"
)

// TestStream_PostAction places a far-from-mid ALO over the WS PostAction
// channel and cancels it via REST. The action is built using the public
// NewCreateOrderActionWithGrouping helper, signed with SignL1Action,
// and dispatched via Stream.PostAction.
func TestStream_PostAction(t *testing.T) {
	c := newStreamingClient(t)
	skipIfNoBalance(t, c)

	coin := testCoin(t)
	m := mid(t, c, coin)
	px := snapPrice(m*0.5, c, coin)
	size := testSizeForLimit(t, c, coin, px)

	req := trade.CreateOrderRequest{
		Coin:       coin,
		IsBuy:      true,
		Price:      px,
		Size:       size,
		ReduceOnly: false,
		OrderType:  types.OrderType{Limit: &types.LimitOrderType{Tif: "Alo"}},
	}
	action, err := c.Trade.NewCreateOrderActionWithGrouping(
		[]trade.CreateOrderRequest{req}, nil, types.GroupingNA,
	)
	if err != nil {
		t.Fatalf("NewCreateOrderActionWithGrouping: %v", err)
	}

	cfg, _ := loadConfig()
	nonce := time.Now().UnixMilli()
	sig, err := signing.SignL1Action(
		cfg.privateKey,
		action,
		"", // no vault
		nonce,
		nil,
		cfg.BaseURL == types.MainnetAPIURL,
	)
	if err != nil {
		t.Fatalf("SignL1Action: %v", err)
	}

	resp, err := c.Stream.PostAction(action, sig, nonce, "", 15*time.Second)
	if err != nil {
		t.Fatalf("Stream.PostAction: %v", err)
	}
	t.Logf("WS PostAction response: %s", string(resp))

	// Clean up via REST CancelAll for the coin.
	if _, err := c.Trade.CancelAll(coin); err != nil {
		t.Logf("CancelAll cleanup: %v", err)
	}
}
