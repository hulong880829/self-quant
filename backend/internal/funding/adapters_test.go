package funding

import (
	"context"
	"errors"
	"testing"
	"time"

	"selfquant/backend/internal/config"
	"selfquant/backend/internal/exchange"
)

type namedAdapter struct {
	name string
}

func (a namedAdapter) Name() string { return a.name }
func (a namedAdapter) SyncInstruments(context.Context, string) ([]exchange.Instrument, error) {
	return nil, nil
}
func (a namedAdapter) FetchCurrent(context.Context, []exchange.Instrument) ([]exchange.FundingRate, error) {
	return nil, nil
}
func (a namedAdapter) FetchHistory(context.Context, exchange.Instrument, time.Time, int) ([]exchange.FundingRate, error) {
	return nil, nil
}

func TestSelectEnabledAdaptersUsesRequestedNamesOnly(t *testing.T) {
	registry := map[string]exchange.Adapter{
		"venue-a": namedAdapter{name: "venue-a"},
		"venue-b": namedAdapter{name: "venue-b"},
	}
	enabled := map[string]bool{"venue-a": true, "venue-b": true}
	selected, err := SelectEnabledAdapters(registry, enabled, []string{"venue-b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].Name() != "venue-b" {
		t.Fatalf("selected=%v", selected)
	}
}

func TestSelectEnabledAdaptersRejectsUnknownAndDisabled(t *testing.T) {
	registry := map[string]exchange.Adapter{
		"venue-a": namedAdapter{name: "venue-a"},
		"venue-b": namedAdapter{name: "venue-b"},
	}
	enabled := map[string]bool{"venue-a": true}
	if _, err := SelectEnabledAdapters(registry, enabled, []string{"venue-c"}); !errors.Is(err, ErrAdapterNotRegistered) {
		t.Fatalf("unregistered error=%v", err)
	}
	if _, err := SelectEnabledAdapters(registry, enabled, []string{"venue-b"}); !errors.Is(err, ErrAdapterNotEnabled) {
		t.Fatalf("disabled error=%v", err)
	}
	if _, err := SelectEnabledAdapters(registry, enabled, nil); !errors.Is(err, ErrExchangesRequired) {
		t.Fatalf("empty error=%v", err)
	}
}

func TestSelectEnabledAdaptersValidatesAllNamesBeforeReturning(t *testing.T) {
	registry := map[string]exchange.Adapter{"venue-a": namedAdapter{name: "venue-a"}}
	enabled := map[string]bool{"venue-a": true}
	if _, err := SelectEnabledAdapters(registry, enabled, []string{"venue-a", "venue-missing"}); err == nil {
		t.Fatal("expected validation to fail before returning adapters")
	}
}

func TestFundingAdapterListRegistersEntropyWithoutEnablingIt(t *testing.T) {
	cfg := config.Funding{HTTPTimeout: time.Second}
	list := FundingAdapterList(cfg)
	var entropy exchange.Adapter
	for _, adapter := range list {
		if adapter.Name() == "entropy" {
			entropy = adapter
			break
		}
	}
	if entropy == nil {
		t.Fatal("entropy adapter must be registered")
	}
	got := exchange.AdapterContractTypes(entropy)
	if len(got) != 1 || got[0] != exchange.ContractTypePerpetual {
		t.Fatalf("entropy contract types=%v", got)
	}
	enabled := EnabledFundingAdapters(cfg)
	for _, adapter := range enabled {
		if adapter.Name() == "entropy" {
			t.Fatal("entropy must stay disabled until ENABLED_EXCHANGES includes it")
		}
	}
	if _, err := SelectEnabledAdapters(
		FundingAdapters(cfg),
		map[string]bool{"binance": true},
		[]string{"entropy"},
	); !errors.Is(err, ErrAdapterNotEnabled) {
		t.Fatalf("unenabled entropy error=%v", err)
	}
}

func TestQualifyNewPerpetualsSkipsFirstCatalogImport(t *testing.T) {
	inserted := []FundingInstrument{{ID: 1}, {ID: 2}}
	if got := qualifyNewPerpetuals(false, inserted); len(got) != 0 {
		t.Fatalf("first import should not backfill: %v", got)
	}
	if got := qualifyNewPerpetuals(true, inserted); len(got) != 2 || got[0].ID != 1 {
		t.Fatalf("incremental insert=%v", got)
	}
}

func TestDeepHistoryStartUsesCalendarYear(t *testing.T) {
	leap := time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC)
	got := deepHistoryStart(leap)
	want := time.Date(2023, time.March, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("leap window start=%v want=%v", got, want)
	}
}
