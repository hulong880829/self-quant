package ranking

import (
	"math"
	"testing"
	"time"
)

func TestSnapshotStoreRestoresCompatibleLastGoodAsStale(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	store := NewSnapshotStore()
	snapshot := Snapshot{
		Period: Period8h,
		Items: []Opportunity{{
			Rank: 1, GlobalSymbol: "BTCUSDT", Period: Period8h,
			Long: Leg{Exchange: "binance"}, Short: Leg{Exchange: "okx"},
			ModelState: CurrentModelVersion,
		}},
		Generation: 42, Status: SnapshotReady, CalculatedAt: now,
		LastSuccessfulAt: now, DataThrough: now,
	}
	if err := store.Restore(snapshot); err != nil {
		t.Fatal(err)
	}
	restored := store.Get(Period8h)
	if restored.Status != SnapshotStale || !restored.Stale ||
		restored.Generation != 42 || len(restored.Items) != 1 {
		t.Fatalf("restored=%+v", restored)
	}
	store.Replace(Period8h, snapshot.Items, now.Add(time.Minute))
	if got := store.Get(Period8h).Generation; got <= 42 {
		t.Fatalf("generation did not advance after restore: %d", got)
	}
}

func TestPersistedSnapshotValidationRejectsWrongPeriodAndNonFiniteValues(t *testing.T) {
	snapshot := Snapshot{
		Period: Period8h, Generation: 1,
		Items: []Opportunity{{
			GlobalSymbol: "BTCUSDT", Period: Period24h,
			Long: Leg{Exchange: "binance"}, Short: Leg{Exchange: "okx"},
		}},
	}
	if err := validatePersistedSnapshot(snapshot, CurrentModelVersion); err == nil {
		t.Fatal("expected item period mismatch")
	}
	snapshot.Items[0].Period = Period8h
	snapshot.Items[0].P5Return = math.Inf(1)
	if err := validatePersistedSnapshot(snapshot, CurrentModelVersion); err == nil {
		t.Fatal("expected non-finite value rejection")
	}
}
