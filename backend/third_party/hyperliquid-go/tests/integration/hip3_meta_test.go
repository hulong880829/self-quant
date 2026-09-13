//go:build integration

package integration

import "testing"

// TestHIP3_MetaAndPerpDexs reads Info.PerpDexs and Info.Meta for the
// configured HIP-3 builder dex (HL_HIP3_DEX). Skips when no dex is
// configured. Logs the first universe asset so the schema is captured
// in test output for the user docs.
func TestHIP3_MetaAndPerpDexs(t *testing.T) {
	dex, skip := pickHIP3Dex(t)
	if skip {
		t.Skip("HL_HIP3_DEX not set; skipping HIP-3 suite")
	}

	c := newClient(t)

	dexs, err := c.Info.PerpDexs()
	if err != nil {
		t.Fatalf("Info.PerpDexs: %v", err)
	}
	t.Logf("PerpDexs returned %d entries", len(dexs))

	meta, err := c.Info.Meta(dex)
	if err != nil {
		t.Fatalf("Info.Meta(%q): %v", dex, err)
	}
	if meta == nil || len(meta.Universe) == 0 {
		t.Skipf("HIP-3 dex %q has an empty universe", dex)
	}
	t.Logf("HIP-3 dex=%q universe size=%d first asset=%+v", dex, len(meta.Universe), meta.Universe[0])
}
