package aggdata

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHistoryQueryKeepsLiveDataWhenShardIsUnavailable(t *testing.T) {
	root := t.TempDir()
	relative := "2026-08-15/05/agg_test/btcusdt/aggbbo.sqrec.zst"
	full := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("not a SQREC shard"), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 8, 15, 6, 30, 0, 0, time.UTC)
	market := &liveMarket{
		catalog: catalogMarket{
			Identity: Identity{Profile: "agg_test", Symbol: "BTCUSDT"},
			Shards:   []string{relative},
		},
		ring: newSampleRing(10),
	}
	market.ring.add(LiveSample{
		WallNS:        uint64(now.Add(-30 * time.Second).UnixNano()),
		GatedBPS:      1.25,
		GatedMin:      1,
		GatedMax:      1.5,
		WindowSamples: 1,
	})
	catalog := &catalogSnapshot{
		SampleIntervalMS: 200,
		Markets:          []catalogMarket{market.catalog},
	}

	response, err := NewHistory(root, 1).Query(
		t.Context(), catalog, market, time.Minute, "gated", now,
	)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(response.UnavailableShards) != 1 ||
		response.UnavailableShards[0] != relative {
		t.Fatalf("unavailable shards=%v", response.UnavailableShards)
	}
	if len(response.Buckets) != 1 || response.Buckets[0].Close != 1.25 {
		t.Fatalf("live bucket was not preserved: %+v", response.Buckets)
	}
	if len(response.Gaps) == 0 {
		t.Fatal("missing historical interval was not reported as a gap")
	}
}

func TestHistoryDeduplicatesShardAndLiveOverlap(t *testing.T) {
	wall := uint64(time.Date(2026, 8, 15, 6, 10, 0, 0, time.UTC).UnixNano())
	samples := []LiveSample{
		{WallNS: wall, GatedBPS: 1},
		{WallNS: wall, GatedBPS: 2},
	}

	result := deduplicateSamples(samples)
	if len(result) != 1 || result[0].GatedBPS != 2 {
		t.Fatalf("deduplicated samples=%+v", result)
	}
}

func TestAggregateHistoryMarksEmptyMinutesAsGaps(t *testing.T) {
	start := time.Date(2026, 8, 15, 6, 0, 0, 0, time.UTC)
	end := start.Add(4 * time.Minute)
	samples := []LiveSample{
		{WallNS: uint64(start.Add(10 * time.Second).UnixNano()), GatedBPS: 1},
		{WallNS: uint64(start.Add(3*time.Minute + 10*time.Second).UnixNano()), GatedBPS: 2},
	}

	buckets, gaps, _ := aggregateHistory(
		samples,
		uint64(start.UnixNano()),
		uint64(end.UnixNano()),
		time.Minute,
		200*time.Millisecond,
		"gated",
	)
	if len(buckets) != 2 {
		t.Fatalf("buckets=%d want=2", len(buckets))
	}
	if len(gaps) != 1 ||
		gaps[0].StartNS != uint64(start.Add(time.Minute).UnixNano()) ||
		gaps[0].EndNS != uint64(start.Add(3*time.Minute).UnixNano()) {
		t.Fatalf("unexpected gaps: %+v", gaps)
	}
}
