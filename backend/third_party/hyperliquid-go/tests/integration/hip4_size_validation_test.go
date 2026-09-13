//go:build integration

package integration

import (
	"errors"
	"testing"


	"github.com/Simon-Busch/hyperliquid-go/types"
)

// TestHIP4_FractionalSizeRejected feeds size=0.5 (fractional) to an
// outcome whose MinSize is 1. The SDK validator must reject before
// signing. The order of validate() rules is: size_below_min triggers
// before size_step_violation, so size=0.5 < MinSize=1 surfaces as
// ValidationError{Code:"size_below_min"}. Either code is acceptable as
// evidence the validator caught the violation pre-flight.
func TestHIP4_FractionalSizeRejected(t *testing.T) {
	c := newClient(t)
	canonical, _, midPx := requireOutcomeOrSkip(t, c)

	px := snapPrice(midPx*0.5, c, canonical)
	if px <= 0 {
		t.Skipf("snapped price <= 0 for %s", canonical)
	}

	_, err := c.Trade.PlaceALO(canonical, types.Buy, 0.5, px)
	if err == nil {
		t.Fatalf("PlaceALO with size=0.5 on HIP-4 outcome was accepted; expected ValidationError")
	}
	var verr *types.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("PlaceALO error not a ValidationError: %v", err)
	}
	if verr.Code != "size_below_min" && verr.Code != "size_step_violation" {
		t.Fatalf("ValidationError.Code = %q, want size_below_min or size_step_violation", verr.Code)
	}
	t.Logf("HIP-4 fractional size rejected: field=%s code=%s msg=%s", verr.Field, verr.Code, verr.Message)
}
