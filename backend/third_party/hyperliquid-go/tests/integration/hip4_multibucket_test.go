//go:build integration

package integration

import (
	"math"
	"strconv"
	"testing"
	"time"


	"github.com/Simon-Busch/hyperliquid-go/types"
	"github.com/Simon-Busch/hyperliquid-go/info"
	"github.com/Simon-Busch/hyperliquid-go/trade"
)

// TestHIP4_MultiBucketDiagnostic dumps the full wire shape of every
// HIP-4 question that has more than one named outcome (i.e. a real
// multi-bucket market). Multi-bucket markets are exposed by the venue
// as separate binary outcomes grouped under a parent Question — the
// only way to find them is to walk OutcomeMeta.Questions, not the
// outcomes array.
//
// The test never places an order. Skips cleanly when no questions exist
// or no multi-bucket questions are live.
func TestHIP4_MultiBucketDiagnostic(t *testing.T) {
	c := newClient(t)

	meta, err := c.Info.OutcomeMeta()
	if err != nil {
		t.Skipf("OutcomeMeta failed: %v", err)
	}
	if meta == nil {
		t.Skip("no HIP-4 outcome metadata on this environment")
	}
	t.Logf("OutcomeMeta: %d outcomes, %d questions", len(meta.Outcomes), len(meta.Questions))

	multiBucket := 0
	for i := range meta.Questions {
		if len(meta.Questions[i].NamedOutcomes) >= 2 {
			multiBucket++
		}
	}
	if multiBucket == 0 {
		t.Skip("no multi-bucket questions on this environment")
	}
	t.Logf("  multi-bucket questions (>=2 named outcomes): %d", multiBucket)

	cfg, _ := loadConfig()
	want := cfg.HIP4Outcome

	dumped := 0
	for i := range meta.Questions {
		q := &meta.Questions[i]
		if len(q.NamedOutcomes) < 2 {
			continue
		}
		if want != "" && q.Name != want {
			continue
		}
		dumped++

		t.Logf("")
		t.Logf("== question %d: %q ==", q.Question, q.Name)
		t.Logf("  description: %s", q.Description)
		t.Logf("  fallbackOutcome=%d  namedOutcomes=%v  settled=%v",
			q.FallbackOutcome, q.NamedOutcomes, q.SettledNamedOutcomes)

		desc := info.ParseOutcomeDescription(q.Description)
		if len(desc) > 0 {
			t.Logf("  parsed: %+v", desc)
		}
		labels := meta.QuestionLabels(q)
		if labels != nil {
			t.Logf("  bucket labels: %v", labels)
		} else {
			t.Logf("  bucket labels: (could not derive — at least one named outcome missing from OutcomeMeta)")
		}

		t.Logf("  buckets:")
		totalYesMid := 0.0
		for _, b := range meta.QuestionBuckets(q) {
			yesMidStr := "—"
			noMidStr := "—"
			if yes, err := c.Info.Mid(b.YesCanonical); err == nil && yes > 0 {
				yesMidStr = strconv.FormatFloat(yes, 'f', 4, 64)
				totalYesMid += yes
			}
			if no, err := c.Info.Mid(b.NoCanonical); err == nil && no > 0 {
				noMidStr = strconv.FormatFloat(no, 'f', 4, 64)
			}
			yesAsset := c.Info.AssetID(b.YesCanonical)
			t.Logf("    outcome=%d  label=%q  yes=%s mid=%s  no=%s mid=%s  asset=%d",
				b.Outcome, b.Label, b.YesCanonical, yesMidStr, b.NoCanonical, noMidStr, yesAsset)
		}
		t.Logf("  sum of YES mids: %.4f  (perfect = 1.0; spread inflates above)", totalYesMid)

		// Friendly-name registration assertions: each bucket's YES side
		// should be addressable under the parent question name + bucket
		// label, not just under "<outcome name>:Yes".
		for _, b := range meta.QuestionBuckets(q) {
			friendly := q.Name + ":" + b.Label + ":Yes"
			id := c.Info.AssetID(friendly)
			if id == 0 {
				t.Logf("    WARN: friendly %q does not resolve via AssetID — bucket-aware registration missing", friendly)
			} else {
				t.Logf("    friendly OK: %q -> asset %d", friendly, id)
			}
		}

		// Top of book for each bucket's YES side.
		for _, b := range meta.QuestionBuckets(q) {
			book, err := c.Info.Book(b.YesCanonical)
			if err != nil || len(book.Levels) < 2 {
				t.Logf("    book %s: empty/err=%v", b.YesCanonical, err)
				continue
			}
			top := func(side []info.Level) string {
				if len(side) == 0 {
					return "—"
				}
				return strconv.FormatFloat(side[0].Px, 'f', 4, 64) + " x " +
					strconv.FormatFloat(side[0].Sz, 'f', 0, 64)
			}
			t.Logf("    book %s: bid=%s ask=%s", b.YesCanonical, top(book.Levels[0]), top(book.Levels[1]))
		}

		if want == "" {
			break // one dump is enough when not pinned
		}
	}

	if dumped == 0 {
		t.Skipf("no multi-bucket question matched HL_HIP4_OUTCOME=%q", want)
	}
}

// TestHIP4_MultiBucketRoundTrip exercises a buy-hold-sell cycle on ONE
// bucket of a multi-bucket question, proving the asset is fully
// tradable through the SDK. Picks the bucket with the highest YES mid
// (most likely outcome, deepest book on average).
func TestHIP4_MultiBucketRoundTrip(t *testing.T) {
	c := newClient(t)

	question, buckets := pickMultiBucketQuestionOrSkip(t, c, 2)
	t.Logf("multi-bucket question: %q (%d buckets)", question, len(buckets))

	var pick multiBucketSide
	for _, b := range buckets {
		t.Logf("  %s yes=%s mid=%.4f", b.Label, b.YesCanonical, b.Mid)
		if b.Mid > pick.Mid {
			pick = b
		}
	}
	if pick.Mid <= 0 {
		t.Skipf("no bucket of %q has a live YES mid", question)
	}
	t.Logf("picked bucket: %s yes=%s mid=%.4f", pick.Label, pick.YesCanonical, pick.Mid)

	meta, err := c.Info.Asset(pick.YesCanonical)
	if err != nil {
		t.Fatalf("Info.Asset(%s): %v", pick.YesCanonical, err)
	}
	if meta.MinSize <= 0 {
		t.Fatalf("Asset(%s) MinSize=%v (expected > 0)", pick.YesCanonical, meta.MinSize)
	}

	// Multi-bucket markets require the venue's $10 USDH minimum like
	// any other HIP-4 outcome. The default hip4MaxNotional cap is too
	// tight here because the order MUST clear $10; use a per-test
	// budget that's just above the venue floor.
	const multiBucketMaxUSDH = 11.0
	target := hip4VenueMinNotional / pick.Mid
	steps := math.Ceil(target / meta.MinSize)
	if steps < 1 {
		steps = 1
	}
	size := steps * meta.MinSize
	notional := size * pick.Mid
	if notional > multiBucketMaxUSDH {
		t.Skipf("bucket %q notional %.2f USDH exceeds per-test budget %.2f at mid %.4f",
			pick.Label, notional, multiBucketMaxUSDH, pick.Mid)
	}

	t.Logf("buying %v contracts of %s (%s) at ~%.4f (~$%.2f USDH)",
		size, pick.YesCanonical, pick.Label, pick.Mid, notional)
	buy, err := c.Trade.PlaceMarket(pick.YesCanonical, types.Buy, size, trade.WithSlippage(0.05))
	if err != nil {
		t.Fatalf("PlaceMarket buy: %v", err)
	}
	if buy.Error != "" {
		t.Fatalf("buy rejected: %s", buy.Error)
	}
	filled, _ := strconv.ParseFloat(buy.TotalSz, 64)
	if filled <= 0 {
		t.Skipf("buy did not fill on %s", pick.YesCanonical)
	}
	t.Logf("buy ack: oid=%d filled=%v avgPx=%s", buy.OID, filled, buy.AvgPx)

	flattened := false
	t.Cleanup(func() {
		if flattened {
			return
		}
		if _, err := c.Trade.PlaceMarket(pick.YesCanonical, types.Sell, filled, trade.WithSlippage(0.10)); err != nil {
			t.Logf("cleanup sell %v: %v (best-effort)", filled, err)
		}
	})

	t.Log("holding 10s...")
	time.Sleep(10 * time.Second)

	sell, err := c.Trade.PlaceMarket(pick.YesCanonical, types.Sell, filled, trade.WithSlippage(0.10))
	if err != nil {
		t.Fatalf("PlaceMarket sell: %v", err)
	}
	if sell.Error != "" {
		t.Fatalf("sell rejected: %s", sell.Error)
	}
	flattened = true
	sold, _ := strconv.ParseFloat(sell.TotalSz, 64)
	t.Logf("sell ack: oid=%d sold=%v avgPx=%s", sell.OID, sold, sell.AvgPx)
	if sold < filled {
		t.Logf("partial close: bought=%v sold=%v residual=%v", filled, sold, filled-sold)
	}
}
