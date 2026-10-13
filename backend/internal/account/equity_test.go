package account

import (
	"testing"
	"time"
)

func TestVenueIdentity(t *testing.T) {
	index := int64(3)
	cases := []struct {
		name   string
		record TradingAccountRecord
		key    string
		want   string
	}{
		{
			name:   "cex uses account name",
			record: TradingAccountRecord{Exchange: "Binance", AccountName: "Main"},
			want:   "binance:main",
		},
		{
			name:   "polymarket uses account name",
			record: TradingAccountRecord{Exchange: "polymarket", AccountName: "Poly-1"},
			want:   "polymarket:poly-1",
		},
		{
			name:   "hyperliquid prefers vault",
			record: TradingAccountRecord{Exchange: "hyperliquid", VaultAddress: "0xABC", SigningAddress: "0xDEF"},
			key:    "0x1111111111111111111111111111111111111111",
			want:   "hyperliquid:0xabc",
		},
		{
			name:   "aster falls back to decrypted wallet",
			record: TradingAccountRecord{Exchange: "aster"},
			key:    "0x1111111111111111111111111111111111111111",
			want:   "aster:0x1111111111111111111111111111111111111111",
		},
		{
			name:   "lighter includes index",
			record: TradingAccountRecord{Exchange: "lighter", VaultAddress: "0xAAA", AccountIndex: &index},
			want:   "lighter:0xaaa:3",
		},
	}
	for _, item := range cases {
		if got := VenueIdentity(item.record, item.key); got != item.want {
			t.Fatalf("%s: got %q want %q", item.name, got, item.want)
		}
	}
}

func TestAggregateEquitySnapshotsDedupesOwnerVenue(t *testing.T) {
	now := time.Date(2026, 9, 20, 4, 0, 0, 0, time.UTC)
	rows := []EquitySnapshotView{
		{OwnerUsername: "alice", BrokerID: "brk_1", VenueIdentity: "binance:main", EquityUSD: "100", Status: "ok", ObservedAt: now.Add(-time.Minute)},
		{OwnerUsername: "alice", BrokerID: "brk_1", VenueIdentity: "binance:main", EquityUSD: "80", Status: "ok", ObservedAt: now.Add(-2 * time.Minute)},
		{OwnerUsername: "alice", BrokerID: "brk_1", VenueIdentity: "okx:main", EquityUSD: "50", Status: "failed", ObservedAt: now.Add(-time.Minute)},
		{OwnerUsername: "bob", BrokerID: "", VenueIdentity: "binance:main", EquityUSD: "10", Status: "ok", ObservedAt: now.Add(-20 * time.Minute)},
	}
	got := AggregateEquitySnapshots(rows, 15*time.Minute, now)
	if got["brk_1"].CoveredAccounts != 1 || got["brk_1"].FailedSnapshots != 1 || got["brk_1"].EquityUSD.String() != "100" {
		t.Fatalf("broker aggregate: %+v", got["brk_1"])
	}
	if got[""].StaleSnapshots != 1 || !got[""].EquityUSD.IsZero() {
		t.Fatalf("direct account stale must not count as zero equity: %+v", got[""])
	}
}
