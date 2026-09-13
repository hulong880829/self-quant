//go:build integration

package integration

import "testing"

// TestHIP4_BookAndMid pulls a Book and Mid for the chosen outcome.
// HIP-4 outcomes are priced in (0, 1] (probability). The test asserts
// both succeed and that the mid sits in the expected range.
// requireOutcomeOrSkip already filters out outcomes without a live mid.
func TestHIP4_BookAndMid(t *testing.T) {
	c := newClient(t)
	canonical, _, midPx := requireOutcomeOrSkip(t, c)

	book, err := c.Info.Book(canonical)
	if err != nil {
		t.Fatalf("Info.Book(%q): %v", canonical, err)
	}
	if book == nil || len(book.Levels) < 2 {
		t.Skipf("Book(%q) has no levels — outcome may be inactive", canonical)
	}
	bids := len(book.Levels[0])
	asks := len(book.Levels[1])
	if bids == 0 && asks == 0 {
		t.Skipf("Book(%q) empty on both sides — outcome inactive", canonical)
	}

	if midPx <= 0 || midPx > 1 {
		t.Fatalf("HIP-4 mid out of range: %v (want (0, 1])", midPx)
	}
	t.Logf("HIP-4 %q: mid=%v bids=%d asks=%d", canonical, midPx, bids, asks)
}
