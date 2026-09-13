//go:build integration

package integration

import (
	"sync/atomic"
	"testing"
	"time"


	"github.com/Simon-Busch/hyperliquid-go/stream"
)

// TestHIP4_TradesSubscription subscribes to stream.Trades on the canonical
// "#<enc>" name of a live outcome. Loose contract: subscription
// succeeds, message count logged. Zero messages within the window is
// acceptable for thin outcomes; the test passes either way.
func TestHIP4_TradesSubscription(t *testing.T) {
	c := newClient(t)
	canonical, _, _ := requireOutcomeOrSkip(t, c)
	// requireOutcomeOrSkip used HTTP only; we need a streaming client now.
	cfg, _ := loadConfig()
	if cfg.SkipWS {
		t.Skip("HL_SKIP_WS=true; skipping HIP-4 WS scenario")
	}
	_ = c
	sc := newStreamingClient(t)

	var count atomic.Int32
	sub, err := sc.Stream.Subscribe(stream.Trades(canonical), func(stream.WSMessage) { count.Add(1) })
	if err != nil {
		t.Fatalf("Subscribe(Trades %q): %v", canonical, err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	time.Sleep(10 * time.Second)
	t.Logf("HIP-4 Trades(%s) received %d messages in 10s", canonical, count.Load())
}
