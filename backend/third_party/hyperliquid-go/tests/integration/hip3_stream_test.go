//go:build integration

package integration

import (
	"sync/atomic"
	"testing"
	"time"


	"github.com/Simon-Busch/hyperliquid-go/stream"
)

// TestHIP3_AllMidsOn subscribes to the dex-pinned all-mids feed via
// stream.AllMidsOn. Loose contract — receiving zero messages within the
// window is acceptable when the dex is quiet; the test only proves the
// subscribe path succeeds.
func TestHIP3_AllMidsOn(t *testing.T) {
	dex, skip := pickHIP3Dex(t)
	if skip {
		t.Skip("HL_HIP3_DEX not set; skipping HIP-3 suite")
	}

	c := newStreamingClient(t)

	var count atomic.Int32
	sub, err := c.Stream.Subscribe(stream.AllMidsOn(dex), func(stream.WSMessage) { count.Add(1) })
	if err != nil {
		t.Fatalf("Subscribe(AllMidsOn %q): %v", dex, err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	time.Sleep(10 * time.Second)
	t.Logf("AllMidsOn(%q) received %d messages", dex, count.Load())
}
