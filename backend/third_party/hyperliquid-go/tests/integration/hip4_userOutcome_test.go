//go:build integration

package integration

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// hip4SplitAmountUSDH is the amount we split / merge per test. Sized
// above any plausible venue floor while staying within the small USDH
// budget that the rest of the HIP-4 tests assume.
const hip4SplitAmountUSDH = 1.0

// TestHIP4_SplitMergeRoundTrip exercises Trader.Outcome.Split followed
// by Trader.Outcome.Merge on a single outcome, proving the userOutcome
// wire path is correctly signed and accepted by the venue.
//
// The split mints N USDH worth of YES + N USDH worth of NO; merge burns
// them back into N USDH. Net P&L is zero modulo venue fees, so the test
// asserts the USDH balance returns to its starting value within a small
// tolerance.
//
// Skips cleanly when no HIP-4 outcomes are live or the wallet lacks
// the budget.
func TestHIP4_SplitMergeRoundTrip(t *testing.T) {
	c := newClient(t)
	cfg, _ := loadConfig()

	usdh := spotBalance(t, c, cfg.AccountAddr, "USDH")
	if usdh < hip4SplitAmountUSDH*1.1 {
		t.Skipf("USDH balance %.4f below required %.4f for split+merge", usdh, hip4SplitAmountUSDH*1.1)
	}

	canonical, friendly, _ := requireYesOutcomeOrSkip(t, c)
	outcome, err := outcomeIDFromCanonical(t, canonical)
	if err != nil {
		t.Fatalf("parse outcome id from %s: %v", canonical, err)
	}
	t.Logf("outcome %d (%s / %s); starting USDH=%.4f", outcome, friendly, canonical, usdh)

	t.Logf("step 1: Split %.2f USDH into Yes + No shares of outcome %d", hip4SplitAmountUSDH, outcome)
	splitResp, err := c.Trade.Outcome.Split(outcome, hip4SplitAmountUSDH)
	if err != nil {
		if strings.Contains(err.Error(), "minimum") || strings.Contains(err.Error(), "Insufficient") {
			t.Skipf("Split rejected by venue (likely below per-outcome minimum): %v", err)
		}
		t.Fatalf("Outcome.Split: %v", err)
	}
	t.Logf("  split ack: status=%q txHash=%s", splitResp.Status, splitResp.TxHash)

	// Give the venue a moment to settle the share credit before the
	// merge attempts to burn them.
	time.Sleep(2 * time.Second)

	t.Logf("step 2: Merge same %.2f back into USDH", hip4SplitAmountUSDH)
	amt := hip4SplitAmountUSDH
	mergeResp, err := c.Trade.Outcome.Merge(outcome, &amt)
	if err != nil {
		t.Fatalf("Outcome.Merge: %v", err)
	}
	t.Logf("  merge ack: status=%q txHash=%s", mergeResp.Status, mergeResp.TxHash)

	time.Sleep(2 * time.Second)
	usdhAfter := spotBalance(t, c, cfg.AccountAddr, "USDH")
	delta := usdhAfter - usdh
	t.Logf("USDH delta after split+merge: %+.6f (start=%.4f end=%.4f)", delta, usdh, usdhAfter)
	if delta < -0.1 {
		t.Errorf("USDH lost more than 0.1 across split+merge (delta=%v) — fees may be wrong or shares lost", delta)
	}
}

// TestHIP4_MergeQuestionMax exercises the maximum-balance MergeQuestion
// shape on a multi-bucket question. The call passes amount=nil, which
// the venue interprets as "burn all you have"; the test just verifies
// the wire path is accepted (most accounts hold no shares of every
// bucket simultaneously, so the actual burned amount will be zero).
func TestHIP4_MergeQuestionMax(t *testing.T) {
	c := newClient(t)

	meta, err := c.Info.OutcomeMeta()
	if err != nil {
		t.Skipf("OutcomeMeta failed: %v", err)
	}
	if meta == nil || len(meta.Questions) == 0 {
		t.Skip("no HIP-4 questions on this environment")
	}
	q := meta.Questions[0]
	if len(q.NamedOutcomes) < 2 {
		t.Skipf("first question has only %d named outcomes; need a multi-bucket question", len(q.NamedOutcomes))
	}
	t.Logf("MergeQuestion(max) on question %d (%s, %d buckets)", q.Question, q.Name, len(q.NamedOutcomes))

	resp, err := c.Trade.Outcome.MergeQuestion(uint64(q.Question), nil)
	if err != nil {
		// "No shares to merge" or similar is an acceptable outcome — the
		// wire path was constructed correctly even if the wallet holds
		// nothing to burn.
		if strings.Contains(err.Error(), "no shares") || strings.Contains(err.Error(), "insufficient") ||
			strings.Contains(err.Error(), "zero") {
			t.Logf("venue had nothing to merge (expected for accounts with no Yes-of-every-bucket): %v", err)
			return
		}
		t.Fatalf("Outcome.MergeQuestion: %v", err)
	}
	t.Logf("merge ack: status=%q", resp.Status)
}

// TestHIP4_NegateAndCollapse exercises the full HIP-4 user-outcome
// lifecycle on a multi-bucket question. The flow:
//
//   1. Split N USDH on one bucket B of a multi-bucket question — wallet
//      now holds N Yes + N No of bucket B.
//   2. Negate N No of bucket B — protocol converts those N No into N Yes
//      of every OTHER bucket in the same question. Wallet now holds
//      N Yes of every bucket and zero No anywhere.
//   3. MergeQuestion(question, N) — burns N Yes of each bucket back into
//      N USDH (since exactly one bucket resolves, the held YES portfolio
//      already guarantees a 1:1 payout; Merge realises it early).
//
// Net USDH change is zero modulo venue fees. Skips when no multi-bucket
// question is live or the wallet lacks the USDH budget. Defensive
// cleanup attempts MergeQuestion(max) to flatten any residual shares.
func TestHIP4_NegateAndCollapse(t *testing.T) {
	c := newClient(t)
	cfg, _ := loadConfig()

	usdh := spotBalance(t, c, cfg.AccountAddr, "USDH")
	if usdh < hip4SplitAmountUSDH*1.5 {
		t.Skipf("USDH balance %.4f below required %.4f for split+negate+merge",
			usdh, hip4SplitAmountUSDH*1.5)
	}

	question, buckets := pickMultiBucketQuestionOrSkip(t, c, 3)
	pick := buckets[0]
	for _, b := range buckets {
		if b.Mid > pick.Mid {
			pick = b
		}
	}
	t.Logf("question %q (%d buckets); picked bucket %q outcome=%d",
		question, len(buckets), pick.Label, pick.Outcome)

	// Defensive cleanup: even if a step fails midway, try to flatten
	// any held shares so the wallet returns to a known state.
	meta, _ := c.Info.OutcomeMeta()
	q := meta.QuestionByName(question)
	if q == nil {
		t.Fatalf("could not re-find question %q in OutcomeMetaCached", question)
	}
	t.Cleanup(func() {
		if _, err := c.Trade.Outcome.MergeQuestion(uint64(q.Question), nil); err != nil {
			t.Logf("cleanup MergeQuestion(max): %v (best-effort)", err)
		}
	})

	t.Logf("step 1: Split %.2f USDH on outcome %d", hip4SplitAmountUSDH, pick.Outcome)
	if _, err := c.Trade.Outcome.Split(uint64(pick.Outcome), hip4SplitAmountUSDH); err != nil {
		if strings.Contains(err.Error(), "minimum") || strings.Contains(err.Error(), "Insufficient") {
			t.Skipf("Split rejected by venue: %v", err)
		}
		t.Fatalf("Split: %v", err)
	}
	time.Sleep(2 * time.Second)

	t.Logf("step 2: Negate %.2f No of outcome %d into Yes of other buckets",
		hip4SplitAmountUSDH, pick.Outcome)
	if _, err := c.Trade.Outcome.Negate(uint64(q.Question), uint64(pick.Outcome), hip4SplitAmountUSDH); err != nil {
		t.Fatalf("Negate: %v", err)
	}
	time.Sleep(2 * time.Second)

	t.Logf("step 3: MergeQuestion %.2f Yes from every bucket -> USDH", hip4SplitAmountUSDH)
	amt := hip4SplitAmountUSDH
	if _, err := c.Trade.Outcome.MergeQuestion(uint64(q.Question), &amt); err != nil {
		t.Fatalf("MergeQuestion: %v", err)
	}
	time.Sleep(2 * time.Second)

	usdhAfter := spotBalance(t, c, cfg.AccountAddr, "USDH")
	delta := usdhAfter - usdh
	t.Logf("USDH delta across split+negate+merge: %+.6f (start=%.4f end=%.4f)",
		delta, usdh, usdhAfter)
	if delta < -0.1 {
		t.Errorf("USDH lost more than 0.1 across the cycle (delta=%v)", delta)
	}
}

// outcomeIDFromCanonical decodes "#<enc>" -> outcome id. Test-side
// wrapper so the integration suite doesn't reach into unexported helpers.
func outcomeIDFromCanonical(t *testing.T, canonical string) (uint64, error) {
	t.Helper()
	if len(canonical) < 2 || canonical[0] != '#' {
		t.Fatalf("expected canonical \"#<enc>\", got %q", canonical)
	}
	enc, err := strconv.ParseUint(canonical[1:], 10, 64)
	if err != nil {
		return 0, err
	}
	return enc / 10, nil
}
