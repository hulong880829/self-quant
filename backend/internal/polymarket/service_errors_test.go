package polymarket

import "testing"

func TestInsufficientFundsAndPositionErrorsAreDistinct(t *testing.T) {
	if ErrInsufficientFunds.Error() != "insufficient balance" {
		t.Fatalf("funds error = %q", ErrInsufficientFunds)
	}
	if ErrInsufficientPosition.Error() != "insufficient position" {
		t.Fatalf("position error = %q", ErrInsufficientPosition)
	}
	if ErrInsufficientFunds == ErrInsufficientPosition {
		t.Fatal("cash and position errors must be distinct")
	}
}
