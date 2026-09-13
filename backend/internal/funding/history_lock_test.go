package funding

import "testing"

func TestHistoryLockKey(t *testing.T) {
	if got := historyLockKey(966983); got != "funding-history:966983" {
		t.Fatalf("historyLockKey(966983)=%q", got)
	}
}
