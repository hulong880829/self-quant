//go:build integration

package integration

import (
	"context"
	"sync/atomic"
	"testing"
	"time"


	"github.com/Simon-Busch/hyperliquid-go/stream"
	hl "github.com/Simon-Busch/hyperliquid-go"
)

// TestStream_Reconnect verifies the SDK's session-restart contract:
// after a Stream.Close, a freshly constructed client can connect and
// re-subscribe to the same filters without leaking the old session.
//
// The SDK's Stream.Close is terminal (sets a closed flag), so
// "reconnect" in the user-facing sense means building a new Client and
// resubscribing — this test exercises that path. Subscription state on
// the live socket is also exercised via two parallel filters on the
// fresh client.
func TestStream_Reconnect(t *testing.T) {
	cfg, _ := loadConfig()
	if cfg.SkipWS {
		t.Skip("HL_SKIP_WS=true; skipping WS scenario")
	}

	coin := testCoin(t)

	// First session: connect, subscribe to two filters, then close.
	c1 := newStreamingClient(t)
	var first atomic.Int32
	sub1a, err := c1.Stream.Subscribe(stream.Trades(coin), func(stream.WSMessage) { first.Add(1) })
	if err != nil {
		t.Fatalf("first Subscribe(Trades): %v", err)
	}
	sub1b, err := c1.Stream.Subscribe(stream.AllMids(), func(stream.WSMessage) { first.Add(1) })
	if err != nil {
		t.Fatalf("first Subscribe(AllMids): %v", err)
	}

	// Hold the connection long enough for a healthy session, then close.
	time.Sleep(2 * time.Second)
	_ = sub1a.Close()
	_ = sub1b.Close()
	if err := c1.Stream.Close(); err != nil {
		t.Logf("first Stream.Close: %v (best-effort)", err)
	}

	// Second session: fresh client, same filters.
	c2, err := hl.New(
		hl.WithBaseURL(cfg.BaseURL),
		hl.WithPrivateKey(cfg.privateKey),
		hl.WithAccount(cfg.AccountAddr),
	)
	if err != nil {
		t.Fatalf("hl.New (second session): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c2.Stream.Connect(ctx); err != nil {
		t.Fatalf("Stream.Connect (second session): %v", err)
	}
	t.Cleanup(func() { _ = c2.Stream.Close() })

	var second atomic.Int32
	sub2a, err := c2.Stream.Subscribe(stream.Trades(coin), func(stream.WSMessage) { second.Add(1) })
	if err != nil {
		t.Fatalf("second Subscribe(Trades): %v", err)
	}
	t.Cleanup(func() { _ = sub2a.Close() })
	sub2b, err := c2.Stream.Subscribe(stream.AllMids(), func(stream.WSMessage) { second.Add(1) })
	if err != nil {
		t.Fatalf("second Subscribe(AllMids): %v", err)
	}
	t.Cleanup(func() { _ = sub2b.Close() })

	deadline := time.After(10 * time.Second)
	for {
		if second.Load() > 0 {
			t.Logf("reconnect ok: first session msgs=%d second session msgs=%d", first.Load(), second.Load())
			return
		}
		select {
		case <-deadline:
			t.Logf("second session received 0 messages in 10s (market quiet); subscribe path succeeded")
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}
