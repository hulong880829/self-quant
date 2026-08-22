package trader

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestMergeOrderStatusMonotonic(t *testing.T) {
	tests := []struct {
		name     string
		current  string
		incoming string
		filled   string
		quantity string
		want     string
	}{
		{"old snapshot does not reopen partial", "partially_filled", "open", "0.4", "1", "partially_filled"},
		{"fill makes open partial", "open", "open", "0.1", "1", "partially_filled"},
		{"cumulative fill infers partial", "open", "", "0.1", "1", "partially_filled"},
		{"late partial fill preserves canceled", "canceled", "partially_filled", "0.8", "1", "canceled"},
		{"late fill converges canceled to filled", "canceled", "canceled", "1", "1", "filled"},
		{"filled never regresses", "filled", "canceled", "1", "1", "filled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := mergeOrderStatus(
				test.current,
				test.incoming,
				decimal.RequireFromString(test.filled),
				decimal.RequireFromString(test.quantity),
			)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("status=%q want=%q", got, test.want)
			}
		})
	}
}
