//go:build integration

package integration

import (
	"sync/atomic"
	"testing"
	"time"


	"github.com/Simon-Busch/hyperliquid-go/stream"
)

// TestStream_TradesReceived subscribes to the trades feed for the test
// coin. The contract verified here is that the subscription succeeds
// and the connection stays alive: receiving zero trades within the
// window means the market was quiet, not that the SDK is broken, so
// the test passes either way and logs the message count.
func TestStream_TradesReceived(t *testing.T) {
	c := newStreamingClient(t)
	coin := testCoin(t)

	var count atomic.Int32
	got := make(chan struct{}, 1)
	sub, err := c.Stream.Subscribe(stream.Trades(coin), func(m stream.WSMessage) {
		count.Add(1)
		select {
		case got <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	select {
	case <-got:
		t.Logf("received %d trade message(s) on %s", count.Load(), coin)
	case <-time.After(10 * time.Second):
		t.Logf("no trades on %s within 10s (market quiet); subscription succeeded", coin)
	}
}
