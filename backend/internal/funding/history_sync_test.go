package funding

import (
	"context"
	"errors"
	"testing"
	"time"

	"selfquant/backend/internal/exchange"
)

type recordingHistoryAdapter struct {
	namedAdapter
	since []time.Time
	rates []exchange.FundingRate
	err   error
}

func (a *recordingHistoryAdapter) FetchHistory(
	_ context.Context, _ exchange.Instrument, since time.Time, _ int,
) ([]exchange.FundingRate, error) {
	a.since = append(a.since, since)
	return a.rates, a.err
}

func TestSyncInstrumentHistoryRequestsRecentWindow(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	from := now.Add(-recentHistoryWindow)
	adapter := &recordingHistoryAdapter{
		namedAdapter: namedAdapter{name: "venue-a"},
		rates: []exchange.FundingRate{{
			Exchange: "venue-a", ExchangeSymbol: "BTC", Settled: true,
			FundingTime: now.Add(-3 * 24 * time.Hour), Rate: 0.0001, IntervalHours: 8,
		}},
	}
	syncer := NewSynchronizer(nil, []exchange.Adapter{adapter}, nil, nil)
	result, err := syncer.syncInstrumentHistory(
		context.Background(), adapter,
		FundingInstrument{ID: 1, Instrument: exchange.Instrument{
			Exchange: "venue-a", ExchangeSymbol: "BTC", IntervalHours: 8,
		}},
		from, now, historyModeRecent,
	)
	if err != nil || result.Outcome != HistoryOutcomeSourceLimited {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(adapter.since) != 1 || !adapter.since[0].Equal(from) {
		t.Fatalf("requested since=%v want=%v", adapter.since, from)
	}
}

func TestSyncInstrumentHistoryPaginationErrorIsFailed(t *testing.T) {
	adapter := &recordingHistoryAdapter{
		namedAdapter: namedAdapter{name: "venue-a"},
		err:          errors.New("page interrupted"),
	}
	syncer := NewSynchronizer(nil, []exchange.Adapter{adapter}, nil, nil)
	result, err := syncer.syncInstrumentHistory(
		context.Background(), adapter,
		FundingInstrument{ID: 2, Instrument: exchange.Instrument{
			Exchange: "venue-a", ExchangeSymbol: "BTC",
		}},
		time.Now().Add(-recentHistoryWindow), time.Now(), historyModeRecent,
	)
	if err == nil || result.Outcome != HistoryOutcomeFailed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestSyncInstrumentHistoryNoSettlementIsSuccess(t *testing.T) {
	adapter := &recordingHistoryAdapter{
		namedAdapter: namedAdapter{name: "venue-a"},
		err:          exchange.ErrNoSettledHistory,
	}
	syncer := NewSynchronizer(nil, []exchange.Adapter{adapter}, nil, nil)
	result, err := syncer.syncInstrumentHistory(
		context.Background(), adapter,
		FundingInstrument{ID: 3, Instrument: exchange.Instrument{
			Exchange: "venue-a", ExchangeSymbol: "BTC",
		}},
		time.Now().Add(-recentHistoryWindow), time.Now(), historyModeRecent,
	)
	if err != nil || result.Outcome != HistoryOutcomeNoSettlementYet {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
