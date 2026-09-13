//go:build integration

package integration

import (
	"testing"


	"github.com/Simon-Busch/hyperliquid-go/types"
)

// TestHIP4_AssetLookupBothNames verifies Info.AssetID and Info.Asset
// agree on the same underlying asset id when queried via either the
// canonical "#<enc>" form or the friendly "<question>:<side>" form, and
// that the resulting AssetMeta.Class is AssetClassOutcome.
func TestHIP4_AssetLookupBothNames(t *testing.T) {
	c := newClient(t)
	canonical, friendly, _ := requireOutcomeOrSkip(t, c)

	idC := c.Info.AssetID(canonical)
	idF := c.Info.AssetID(friendly)
	if idC == 0 || idF == 0 {
		t.Fatalf("AssetID returned 0 for HIP-4 names: canonical=%q (%d) friendly=%q (%d)", canonical, idC, friendly, idF)
	}
	if idC != idF {
		t.Fatalf("AssetID mismatch: canonical %q -> %d, friendly %q -> %d", canonical, idC, friendly, idF)
	}
	if idC < 100_000_000 {
		t.Fatalf("HIP-4 asset id %d below the 100_000_000 base", idC)
	}

	meta, err := c.Info.Asset(canonical)
	if err != nil {
		t.Fatalf("Info.Asset(%q): %v", canonical, err)
	}
	if meta.Class != types.AssetClassOutcome {
		t.Fatalf("Asset(%q).Class = %d, want AssetClassOutcome (%d)", canonical, meta.Class, types.AssetClassOutcome)
	}
	if meta.SzDecimals != 0 {
		t.Fatalf("HIP-4 SzDecimals = %d, want 0 (integer contracts)", meta.SzDecimals)
	}
	t.Logf("HIP-4 asset: canonical=%q friendly=%q id=%d szDecimals=%d minSize=%v tick=%v class=outcome",
		canonical, friendly, meta.ID, meta.SzDecimals, meta.MinSize, meta.TickSize)
}
