package exchange

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

const (
	entropyExchange = "entropy"
	entropyDex      = "io"
	entropyInfoURL  = "https://api.hyperliquid.xyz"
)

type Entropy struct{ client client }

func NewEntropy(timeout time.Duration) *Entropy {
	return &Entropy{client: newClient(entropyInfoURL, timeout)}
}

func (e *Entropy) Name() string { return entropyExchange }

func (e *Entropy) SupportedContractTypes() []string {
	return []string{ContractTypePerpetual}
}

func (e *Entropy) SyncInstruments(ctx context.Context, contractType string) ([]Instrument, error) {
	if contractType != ContractTypePerpetual {
		return nil, fmt.Errorf("entropy: unsupported contract type %q", contractType)
	}
	meta, _, err := hyperliquidMetaAndContexts(e.client, ctx, entropyDex)
	if err != nil {
		return nil, err
	}
	result := stampEntropyInstruments(parseHyperliquidInstruments(meta, entropyDex))
	if len(result) == 0 {
		return nil, fmt.Errorf("entropy: empty perpetual catalog")
	}
	return result, nil
}

func (e *Entropy) FetchCurrent(ctx context.Context, _ []Instrument) ([]FundingRate, error) {
	meta, contexts, err := hyperliquidMetaAndContexts(e.client, ctx, entropyDex)
	if err != nil {
		return nil, err
	}
	rates, err := parseHyperliquidCurrent(meta, contexts, time.Now(), entropyDex)
	if err != nil {
		return nil, err
	}
	rates = stampEntropyRates(rates)
	if len(rates) == 0 {
		return nil, fmt.Errorf("entropy: empty perpetual catalog")
	}
	return rates, nil
}

func (e *Entropy) FetchHistory(ctx context.Context, instrument Instrument, since time.Time, limit int) ([]FundingRate, error) {
	wanted := clampLimit(limit, 10000)
	result := make([]FundingRate, 0, wanted)
	start := since.UnixMilli()
	if since.IsZero() {
		start = time.Now().AddDate(-1, 0, 0).UnixMilli()
	}
	for len(result) < wanted {
		body := fmt.Sprintf(
			`{"type":"fundingHistory","coin":%q,"startTime":%d,"endTime":%d}`,
			instrument.ExchangeSymbol, start, time.Now().UnixMilli(),
		)
		var page []hyperliquidHistory
		if err := e.client.post(ctx, "/info", body, &page); err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for _, item := range page {
			rate, err := strconv.ParseFloat(item.FundingRate, 64)
			if err != nil {
				return nil, err
			}
			result = append(result, FundingRate{
				Exchange: entropyExchange, ExchangeSymbol: instrument.ExchangeSymbol, Rate: rate,
				FundingTime: milliseconds(item.Time), Settled: true, IntervalHours: 1,
				SourceUpdatedAt: milliseconds(item.Time),
			})
			if len(result) == wanted {
				return result, nil
			}
		}
		if len(page) < 500 {
			break
		}
		nextStart := page[len(page)-1].Time + 1
		if nextStart <= start {
			return nil, fmt.Errorf("entropy fundingHistory: cursor did not advance")
		}
		start = nextStart
	}
	return result, nil
}

func stampEntropyInstruments(items []Instrument) []Instrument {
	for i := range items {
		items[i].Exchange = entropyExchange
	}
	return items
}

func stampEntropyRates(items []FundingRate) []FundingRate {
	for i := range items {
		items[i].Exchange = entropyExchange
	}
	return items
}
