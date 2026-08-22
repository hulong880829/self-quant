package aggdata

import (
	"encoding/json"
	"math"
	"testing"
)

func TestSnapshotJSONEncodesUint64MetadataAsStrings(t *testing.T) {
	market := &liveMarket{
		catalog: catalogMarket{
			Identity: Identity{Profile: "agg_binance", Symbol: "BTCUSDT"},
		},
	}
	market.book.value.Store(&Snapshot{
		RingSequence: math.MaxUint64,
		Generation:   math.MaxUint64 - 1,
		WallNS:       17_865_120_000_000_001,
		Ready:        true,
		Book:         &Book{},
	})
	market.bbo.value.Store(&Snapshot{
		RingSequence: math.MaxUint64 - 2,
		Generation:   math.MaxUint64 - 3,
		WallNS:       17_865_120_000_000_007,
		Ready:        true,
		BBO:          &BBO{},
	})

	body, err := json.Marshal(snapshotJSON(market, 50))
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		BBO struct {
			Sequence   string `json:"sequence"`
			Generation string `json:"generation"`
			WallNS     string `json:"wall_ns"`
		} `json:"bbo"`
		OrderBook struct {
			Sequence   string `json:"sequence"`
			Generation string `json:"generation"`
			WallNS     string `json:"wall_ns"`
		} `json:"orderbook"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response.OrderBook.Sequence != "18446744073709551615" ||
		response.OrderBook.Generation != "18446744073709551614" ||
		response.OrderBook.WallNS != "17865120000000001" {
		t.Fatalf("unsafe snapshot integers were not preserved: %s", body)
	}
	if response.BBO.Sequence != "18446744073709551613" ||
		response.BBO.Generation != "18446744073709551612" ||
		response.BBO.WallNS != "17865120000000007" {
		t.Fatalf("unsafe BBO integers were not preserved: %s", body)
	}
}

func TestHistoryJSONEncodesNanosecondsAsStrings(t *testing.T) {
	response := HistoryResponse{
		StartNS:           17_865_120_000_000_001,
		EndNS:             17_865_120_000_000_002,
		ResolutionNS:      60_000_000_000,
		CurrentHourFromNS: 17_865_120_000_000_003,
		Buckets: []HistoryBucket{{
			StartNS: 17_865_120_000_000_004,
		}},
		Gaps: []HistoryGap{{
			StartNS: 17_865_120_000_000_005,
			EndNS:   17_865_120_000_000_006,
		}},
	}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var encoded struct {
		StartNS           string `json:"start_ns"`
		EndNS             string `json:"end_ns"`
		ResolutionNS      string `json:"resolution_ns"`
		CurrentHourFromNS string `json:"current_hour_from_ns"`
		Buckets           []struct {
			StartNS string `json:"start_ns"`
		} `json:"buckets"`
		Gaps []struct {
			StartNS string `json:"start_ns"`
			EndNS   string `json:"end_ns"`
		} `json:"gaps"`
	}
	if err := json.Unmarshal(body, &encoded); err != nil {
		t.Fatal(err)
	}
	if encoded.StartNS != "17865120000000001" ||
		encoded.EndNS != "17865120000000002" ||
		encoded.ResolutionNS != "60000000000" ||
		encoded.CurrentHourFromNS != "17865120000000003" ||
		len(encoded.Buckets) != 1 ||
		encoded.Buckets[0].StartNS != "17865120000000004" ||
		len(encoded.Gaps) != 1 ||
		encoded.Gaps[0].StartNS != "17865120000000005" ||
		encoded.Gaps[0].EndNS != "17865120000000006" {
		t.Fatalf("unsafe history timestamps were not preserved: %s", body)
	}
}
