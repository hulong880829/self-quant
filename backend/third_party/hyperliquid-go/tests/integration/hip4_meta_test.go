//go:build integration

package integration

import "testing"

// TestHIP4_OutcomeMeta exercises Info.OutcomeMeta. Skips cleanly when
// the environment has no outcomes registered. Logs the shape of the
// first outcome so the user-doc captures the on-wire schema.
func TestHIP4_OutcomeMeta(t *testing.T) {
	c := newClient(t)

	meta, err := c.Info.OutcomeMeta()
	if err != nil {
		t.Skipf("OutcomeMeta failed (HIP-4 not supported here?): %v", err)
	}
	if meta == nil || len(meta.Outcomes) == 0 {
		t.Skip("no HIP-4 outcomes available on this environment")
	}

	t.Logf("HIP-4 outcomes=%d questions=%d", len(meta.Outcomes), len(meta.Questions))
	first := meta.Outcomes[0]
	t.Logf("first outcome: id=%d name=%q desc=%q sides=%d", first.Outcome, first.Name, first.Description, len(first.SideSpecs))
	for i, s := range first.SideSpecs {
		t.Logf("  side[%d]: %q", i, s.Name)
	}
}
