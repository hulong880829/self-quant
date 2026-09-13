package ranking

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestHistoryGenerationLargeMemoryProfile(t *testing.T) {
	if os.Getenv("FUNDING_RANKING_LARGE_MEMORY_TEST") != "1" {
		t.Skip("set FUNDING_RANKING_LARGE_MEMORY_TEST=1 for isolated memory profile")
	}
	const (
		replayLegs = 1074
		tailLegs   = 1953
	)
	replay := make(map[HistoryPair]*LegSeries, replayLegs)
	tail := make(map[HistoryPair]*LegSeries, tailLegs)
	minute := int64(30_000_000)
	for index := 0; index < replayLegs; index++ {
		values := make([]MinuteQuote, 10_080)
		for offset := range values {
			values[offset] = MinuteQuote{
				Minute: minute + int64(offset), Bid: 100, Ask: 101,
			}
		}
		pair := HistoryPair{
			Venue: "binance", SourceSymbol: fmt.Sprintf("R%04dUSDT", index),
			CanonicalSymbol: fmt.Sprintf("R%04dUSDT", index),
		}
		replay[pair] = newLegSeries(appendBlocks(nil, values, replayQuoteBlockSize))
	}
	for index := 0; index < tailLegs; index++ {
		pair := HistoryPair{
			Venue: "okx", SourceSymbol: fmt.Sprintf("T%04dUSDT", index),
			CanonicalSymbol: fmt.Sprintf("T%04dUSDT", index),
		}
		tail[pair] = newLegSeries(appendBlocks(nil, []MinuteQuote{{
			Minute: minute, Bid: 100, Ask: 101,
		}}, tailQuoteBlockSize))
	}
	slots := tierSlots(replay) + tierSlots(tail)
	if slots > defaultHistorySlots || slots < defaultSoftSlots {
		t.Fatalf("profile slots=%d", slots)
	}
	var group sync.WaitGroup
	launched := 0
	for _, series := range replay {
		view := MinuteSeriesView{
			series: series, fromMinute: minute, toMinute: minute + 10_080,
		}
		group.Add(1)
		go func() {
			defer group.Done()
			count := 0
			view.ForEach(func(MinuteQuote) bool {
				count++
				return true
			})
			if count != 10_080 {
				t.Errorf("view rows=%d", count)
			}
		}()
		launched++
		if launched == 8 {
			break
		}
	}
	group.Wait()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	rss := testRSSBytes()
	t.Logf("slots=%d heap_inuse=%d rss=%d", slots, stats.HeapInuse, rss)
	if rss > 1200<<20 {
		t.Fatalf("rss=%d exceeds 1200MiB target", rss)
	}
	runtime.KeepAlive(replay)
	runtime.KeepAlive(tail)
}

func testRSSBytes() uint64 {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * uint64(os.Getpagesize())
}
