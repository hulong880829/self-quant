package funding

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"selfquant/backend/internal/exchange"
	"selfquant/backend/internal/funding/ibkr"
)

type scriptLookup struct {
	mu        sync.Mutex
	calls     []string
	queries   []ibkr.Query
	contracts []ibkr.Contract
	err       error
}

func (s *scriptLookup) Lookup(_ context.Context, query ibkr.Query) ([]ibkr.Contract, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, query.Symbol)
	s.queries = append(s.queries, query)
	if s.err != nil {
		return nil, s.err
	}
	return append([]ibkr.Contract(nil), s.contracts...), nil
}

func (s *scriptLookup) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

type memIBCatalog struct {
	mu    sync.Mutex
	items map[string]IBResolution
	saves int
}

func newMemIBCatalog(items ...IBResolution) *memIBCatalog {
	catalog := &memIBCatalog{items: map[string]IBResolution{}}
	for _, item := range items {
		catalog.items[item.Symbol] = item
	}
	return catalog
}

func (c *memIBCatalog) ListIBInstrumentResolutions(context.Context) ([]IBResolution, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]IBResolution, 0, len(c.items))
	for _, item := range c.items {
		result = append(result, item)
	}
	return result, nil
}

func (c *memIBCatalog) UpsertIBInstrumentResolution(_ context.Context, item IBResolution) (IBResolution, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saves++
	if item.ConID != nil && *item.ConID > 0 {
		item.Symbol = "conid:" + strconv.FormatInt(*item.ConID, 10)
		if existing, ok := c.items[item.Symbol]; ok {
			item.PairMatches = mergePairMatches(cloneIBResolution(existing).PairMatches, item.PairMatches)
			item.Aliases = mergeAliases(existing.Aliases, item.Aliases)
		}
	}
	stored := cloneIBResolution(item)
	c.items[stored.Symbol] = stored
	return stored, nil
}

func (c *memIBCatalog) get(symbol string) (IBResolution, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[symbol]
	return item, ok
}

func (c *memIBCatalog) saveCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saves
}

func testTradefi(lookup ContractLookup, catalog *memIBCatalog, rates []Rate) *TradefiService {
	funding := NewSnapshotStore()
	funding.Replace(rates, len(rates), time.Now().UTC())
	service := NewTradefiService(catalog, funding, lookup, NewTradefiSnapshotStore(), nil)
	service.interval = 0
	service.SetTradableVenues(map[string]bool{
		"binance": true, "okx": true, "bybit": true, "bitget": true,
		"gate": true, "hyperliquid": true, "aster": true, "lighter": true,
	})
	return service
}

func equityPerp(venue, symbol, base string) exchange.Instrument {
	return exchange.Instrument{
		Exchange: venue, ExchangeSymbol: symbol, BaseAsset: base, QuoteAsset: "USDT",
		ContractType: exchange.ContractTypePerpetual, Status: "active",
		Underlying: exchange.StockReference(base, "US", "USDT", "Apple Inc", "test"),
	}
}

func freshRate(venue, symbol, base string, annualized float64) Rate {
	return Rate{
		Exchange: venue, ExchangeSymbol: symbol, BaseAsset: base, QuoteAsset: "USDT",
		IndexPrice: 100, AnnualizedRate: annualized, SourceUpdatedAt: time.Now().UTC(),
	}
}

const aaplKey = "stock|AAPL|USD|US"

func TestReconcileQueriesNewTradifiOnceDespitePeerFailure(t *testing.T) {
	lookup := &scriptLookup{contracts: []ibkr.Contract{{
		ConID: 265598, Symbol: "AAPL", SecType: "STK", Currency: "USD", PrimaryExchange: "NASDAQ",
		LocalSymbol: "AAPL", TradingClass: "AAPL", LongName: "Apple Inc",
	}}}
	catalog := newMemIBCatalog()
	service := testTradefi(lookup, catalog, []Rate{freshRate("binance", "AAPLUSDT", "AAPL", 0.11)})
	binance := &catalogAdapter{name: "binance", instruments: []exchange.Instrument{
		equityPerp("binance", "AAPLUSDT", "AAPL"),
	}}
	okx := &catalogAdapter{name: "okx", err: errors.New("catalog down")}
	writer := &memInstrumentWriter{}
	synchronizer := NewSynchronizer(nil, []exchange.Adapter{binance, okx}, NewSnapshotStore(), nil)
	synchronizer.instrumentWrites = writer
	synchronizer.WithTradefi(service)
	if _, err := synchronizer.SyncInstruments(context.Background()); err == nil {
		t.Fatal("peer catalog failure must still be reported")
	}
	if lookup.count() != 1 || lookup.calls[0] != "AAPL" {
		t.Fatalf("lookups=%v", lookup.calls)
	}
	if len(writer.deactivated) != 1 || writer.deactivated[0] != "binance" {
		t.Fatalf("deactivated=%v", writer.deactivated)
	}
	rows := service.Snapshots().Get().Rows
	if len(rows) != 1 || rows[0].Exchange != "binance" || rows[0].ConID != 265598 {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestSameStockOnSeveralVenuesMakesSeveralRows(t *testing.T) {
	lookup := &scriptLookup{contracts: []ibkr.Contract{{
		ConID: 265598, Symbol: "AAPL", SecType: "STK", Currency: "USD", PrimaryExchange: "NASDAQ", LongName: "Apple Inc",
	}}}
	service := testTradefi(lookup, newMemIBCatalog(), []Rate{
		freshRate("binance", "AAPLUSDT", "AAPL", 0.1),
		freshRate("okx", "AAPL-USDT-SWAP", "AAPL", 0.2),
		freshRate("hyperliquid", "xyz:AAPL", "AAPL", 0.3),
	})
	err := service.Reconcile(context.Background(), []exchange.Instrument{
		equityPerp("binance", "AAPLUSDT", "AAPL"),
		equityPerp("okx", "AAPL-USDT-SWAP", "AAPL"),
		equityPerp("hyperliquid", "xyz:AAPL", "AAPL"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if lookup.count() != 1 {
		t.Fatalf("lookups=%d", lookup.count())
	}
	rows := service.Snapshots().Get().Rows
	if len(rows) != 3 {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestDuplicateConIDIsUnique(t *testing.T) {
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	resolution := resolutionFromContracts(exchange.StockReference("AAPL", "US", "USDT", "", "test"), []ibkr.Contract{
		{ConID: 265598, Symbol: "AAPL", SecType: "STK", Currency: "USD", PrimaryExchange: "NASDAQ", LocalSymbol: "AAPL"},
		{ConID: 265598, Symbol: "AAPL", SecType: "STK", Currency: "USD", PrimaryExchange: "NASDAQ", LocalSymbol: "AAPL"},
	}, now)
	if !resolution.Active || resolution.Status != "active" || resolution.ConID == nil || *resolution.ConID != 265598 {
		t.Fatalf("resolution=%+v", resolution)
	}
}

func TestLookupFailureDoesNotWriteNotFoundOrDropActive(t *testing.T) {
	conID := int64(265598)
	catalog := newMemIBCatalog(IBResolution{
		Symbol: aaplKey, UnderlyingSymbol: "AAPL", AssetClass: "stock", UnderlyingCurrency: "USD", Region: "US",
		Active: true, Status: "active", ConID: &conID,
		NextVerify: time.Now().Add(-time.Hour),
	})
	lookup := &scriptLookup{err: errors.New("gateway down")}
	service := testTradefi(lookup, catalog, nil)
	if err := service.Reconcile(context.Background(), []exchange.Instrument{
		equityPerp("binance", "AAPLUSDT", "AAPL"),
	}); err != nil {
		t.Fatal(err)
	}
	if lookup.count() != tradefiLookupAttempts {
		t.Fatalf("attempts=%d", lookup.count())
	}
	if catalog.saveCount() != 0 {
		t.Fatalf("saves=%d", catalog.saveCount())
	}
	item, ok := catalog.get(aaplKey)
	if !ok || !item.Active || item.Status != "active" || item.ConID == nil {
		t.Fatalf("cached=%+v ok=%v", item, ok)
	}
}

func TestRestartKeepsNegativeCache(t *testing.T) {
	catalog := newMemIBCatalog(
		IBResolution{Symbol: aaplKey, Status: "not_found", NextVerify: time.Now().Add(time.Hour)},
		IBResolution{Symbol: "stock|MSFT|USD|US", Status: "ambiguous", NextVerify: time.Now().Add(time.Hour)},
	)
	lookup := &scriptLookup{}
	service := testTradefi(lookup, catalog, nil)
	if err := service.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Reconcile(context.Background(), []exchange.Instrument{
		equityPerp("binance", "AAPLUSDT", "AAPL"),
		equityPerp("binance", "MSFTUSDT", "MSFT"),
	}); err != nil {
		t.Fatal(err)
	}
	if lookup.count() != 0 {
		t.Fatalf("lookups=%d", lookup.count())
	}
}

func TestExpiredConIDRecheckMarksTheCanonicalRowInactive(t *testing.T) {
	conID := int64(265598)
	catalog := newMemIBCatalog(IBResolution{
		Symbol: "conid:265598", UnderlyingSymbol: "AAPL", UnderlyingCurrency: "USD",
		Active: true, Status: "active", ConID: &conID, NextVerify: time.Now().Add(-time.Hour),
		Aliases: []string{aaplKey},
	})
	lookup := &scriptLookup{}
	service := testTradefi(lookup, catalog, nil)
	if err := service.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Reconcile(context.Background(), []exchange.Instrument{
		equityPerp("binance", "AAPLUSDT", "AAPL"),
	}); err != nil {
		t.Fatal(err)
	}
	item, ok := catalog.get("conid:265598")
	if !ok || item.Active || item.Status != "inactive" {
		t.Fatalf("canonical=%+v ok=%v", item, ok)
	}
	if _, exists := catalog.get(aaplKey); exists {
		t.Fatal("recheck must not create a second candidate row")
	}
	lookup.mu.Lock()
	defer lookup.mu.Unlock()
	if len(lookup.queries) == 0 || lookup.queries[0].ConID != 265598 || lookup.queries[0].Symbol != "" {
		t.Fatalf("queries=%+v", lookup.queries)
	}
}

func TestExpiredNegativeCacheIsQueriedAgain(t *testing.T) {
	catalog := newMemIBCatalog(IBResolution{
		Symbol: aaplKey, Status: "not_found", NextVerify: time.Now().Add(-time.Second),
	})
	lookup := &scriptLookup{contracts: []ibkr.Contract{{
		ConID: 265598, Symbol: "AAPL", SecType: "STK", Currency: "USD", PrimaryExchange: "NASDAQ", LongName: "Apple Inc",
	}}}
	service := testTradefi(lookup, catalog, nil)
	if err := service.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Reconcile(context.Background(), []exchange.Instrument{
		equityPerp("binance", "AAPLUSDT", "AAPL"),
	}); err != nil {
		t.Fatal(err)
	}
	if lookup.count() != 1 {
		t.Fatalf("lookups=%d", lookup.count())
	}
}

func TestBuildTradefiRowsKeepsSingleExchangeHistory(t *testing.T) {
	conID := int64(265598)
	next := 0.0002
	incomplete := false
	rows := BuildTradefiRows([]IBResolution{{
		Symbol: aaplKey, UnderlyingSymbol: "AAPL", Active: true, Status: "active", ConID: &conID, PrimaryExchange: "NASDAQ",
		PairMatches:        map[string]PairMatch{"binance|AAPLUSDT": {MatchClass: MatchVerifiedListing, PerpPriceCurrency: "USD", VerifiedAt: time.Now().UTC()}},
		UnderlyingCurrency: "USD", NextVerify: time.Now().Add(time.Hour),
	}}, []Rate{{
		Exchange: "binance", ExchangeSymbol: "AAPLUSDT", BaseAsset: "AAPL", QuoteAsset: "USDT",
		Rate: 0.0001, AnnualizedRate: 0.11, NextRate: &next,
		Cumulative24h: 0.001, Cumulative7d: 0.01, History24hComplete: &incomplete,
		History7dComplete: &incomplete, SourceUpdatedAt: time.Now().UTC(),
	}}, map[string]bool{"binance": true})
	if len(rows) != 1 || rows[0].NextRate == nil || *rows[0].NextRate != next ||
		rows[0].Cumulative24h != 0.001 || rows[0].Cumulative7d != 0.01 ||
		rows[0].History24hComplete == nil || *rows[0].History24hComplete ||
		rows[0].History7dComplete == nil || *rows[0].History7dComplete ||
		rows[0].SourceUpdatedAt.IsZero() || rows[0].AnnualizedRate != 0.11 {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestIsStaleMatchesSingleExchangeRule(t *testing.T) {
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	if !IsStale(time.Time{}, now, time.Minute) {
		t.Fatal("zero timestamp must be stale")
	}
	if IsStale(now.Add(-time.Second), now, time.Minute) {
		t.Fatal("fresh timestamp must not be stale")
	}
	if !IsStale(now.Add(-2*time.Minute), now, time.Minute) {
		t.Fatal("old timestamp must be stale")
	}
}

func TestDisabledTradefiDoesNotQueryIB(t *testing.T) {
	writer := &memInstrumentWriter{}
	synchronizer := NewSynchronizer(nil, []exchange.Adapter{
		&catalogAdapter{name: "binance", instruments: []exchange.Instrument{equityPerp("binance", "AAPLUSDT", "AAPL")}},
	}, NewSnapshotStore(), nil)
	synchronizer.instrumentWrites = writer
	if _, err := synchronizer.SyncInstruments(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(writer.saved) != 1 || writer.saved[0][0].Exchange != "binance" {
		t.Fatalf("saved=%+v", writer.saved)
	}
}

type catalogAdapter struct {
	name        string
	instruments []exchange.Instrument
	err         error
}

func (a *catalogAdapter) Name() string { return a.name }
func (a *catalogAdapter) SupportedContractTypes() []string {
	return []string{exchange.ContractTypePerpetual}
}
func (a *catalogAdapter) SyncInstruments(context.Context, string) ([]exchange.Instrument, error) {
	if a.err != nil {
		return nil, a.err
	}
	return append([]exchange.Instrument(nil), a.instruments...), nil
}
func (a *catalogAdapter) FetchCurrent(context.Context, []exchange.Instrument) ([]exchange.FundingRate, error) {
	return nil, nil
}
func (a *catalogAdapter) FetchHistory(context.Context, exchange.Instrument, time.Time, int) ([]exchange.FundingRate, error) {
	return nil, nil
}

type memInstrumentWriter struct {
	saved       [][]exchange.Instrument
	deactivated []string
}

func (w *memInstrumentWriter) HasExchangeInstruments(context.Context, string) (bool, error) {
	return true, nil
}
func (w *memInstrumentWriter) UpsertInstruments(_ context.Context, instruments []exchange.Instrument) ([]FundingInstrument, error) {
	w.saved = append(w.saved, instruments)
	return nil, nil
}
func (w *memInstrumentWriter) DeactivateMissingInstruments(_ context.Context, exchangeName, _ string, _ []exchange.Instrument) error {
	w.deactivated = append(w.deactivated, exchangeName)
	return nil
}

func TestUSDAndHKDPricesDoNotShareAPageRow(t *testing.T) {
	conID := int64(1211)
	rows := BuildTradefiRows([]IBResolution{{
		Symbol: "conid:1211", UnderlyingSymbol: "1211", UnderlyingCurrency: "HKD",
		Active: true, Status: "active", ConID: &conID, NextVerify: time.Now().Add(time.Hour),
		PairMatches: map[string]PairMatch{"bybit|BYDUSDT": {MatchClass: MatchVerifiedListing, PerpPriceCurrency: "USD"}},
	}}, []Rate{{Exchange: "bybit", ExchangeSymbol: "BYDUSDT", SourceUpdatedAt: time.Now().UTC()}},
		map[string]bool{"bybit": true})
	if len(rows) != 0 {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestUnclassifiedAndIndexAreNotQueried(t *testing.T) {
	lookup := &scriptLookup{}
	service := testTradefi(lookup, newMemIBCatalog(), nil)
	err := service.Reconcile(context.Background(), []exchange.Instrument{
		{Exchange: "binance", ExchangeSymbol: "XAUUSDT", BaseAsset: "XAU", ContractType: exchange.ContractTypePerpetual, Status: "active",
			Underlying: exchange.UnderlyingReference{AssetClass: exchange.AssetClassTradfiUnclassified, Symbol: "XAU", PerpQuoteCurrency: "USDT"}},
		{Exchange: "hyperliquid", ExchangeSymbol: "xyz:SP500", BaseAsset: "SP500", ContractType: exchange.ContractTypePerpetual, Status: "active",
			Underlying: exchange.UnderlyingReference{AssetClass: exchange.AssetClassIndex, Symbol: "SP500"}},
		{Exchange: "hyperliquid", ExchangeSymbol: "xyz:OPENAI", BaseAsset: "OPENAI", ContractType: exchange.ContractTypePerpetual, Status: "active",
			Underlying: exchange.UnderlyingReference{AssetClass: exchange.AssetClassPreIPO, Symbol: "OPENAI"}},
	})
	if err != nil || lookup.count() != 0 || len(service.Snapshots().Get().Rows) != 0 {
		t.Fatalf("lookups=%d rows=%d err=%v", lookup.count(), len(service.Snapshots().Get().Rows), err)
	}
}

func TestStockAndCryptoDoNotShareALookup(t *testing.T) {
	lookup := &scriptLookup{contracts: []ibkr.Contract{{
		ConID: 1, Symbol: "AAPL", SecType: "STK", Currency: "USD", PrimaryExchange: "NASDAQ", LongName: "Apple Inc",
	}}}
	service := testTradefi(lookup, newMemIBCatalog(), []Rate{
		freshRate("bybit", "AAPLUSDT", "AAPL", 0.1),
		freshRate("binance", "AAPLUSDT", "AAPL", 0.2),
	})
	crypto := exchange.Instrument{
		Exchange: "binance", ExchangeSymbol: "AAPLUSDT", BaseAsset: "AAPL",
		ContractType: exchange.ContractTypePerpetual, Status: "active",
		Underlying: exchange.CryptoReference("AAPL", "USDT", "test"),
	}
	if err := service.Reconcile(context.Background(), []exchange.Instrument{
		equityPerp("bybit", "AAPLUSDT", "AAPL"), crypto,
	}); err != nil {
		t.Fatal(err)
	}
	if lookup.count() != 1 || lookup.calls[0] != "AAPL" {
		t.Fatalf("lookups=%v", lookup.calls)
	}
	rows := service.Snapshots().Get().Rows
	if len(rows) != 1 || rows[0].Exchange != "bybit" {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestMultipleConIDsStayOffThePage(t *testing.T) {
	lookup := &scriptLookup{contracts: []ibkr.Contract{
		{ConID: 1, Symbol: "AAPL", SecType: "STK", Currency: "USD", PrimaryExchange: "NASDAQ", LongName: "Apple Inc"},
		{ConID: 2, Symbol: "AAPL", SecType: "STK", Currency: "USD", PrimaryExchange: "NYSE", LongName: "Apple Inc"},
	}}
	service := testTradefi(lookup, newMemIBCatalog(), []Rate{freshRate("bybit", "AAPLUSDT", "AAPL", 0.1)})
	if err := service.Reconcile(context.Background(), []exchange.Instrument{equityPerp("bybit", "AAPLUSDT", "AAPL")}); err != nil {
		t.Fatal(err)
	}
	item, ok := service.cached(aaplKey)
	if !ok || item.Status != MatchAmbiguous || len(service.Snapshots().Get().Rows) != 0 {
		t.Fatalf("resolution=%+v rows=%d", item, len(service.Snapshots().Get().Rows))
	}
}

func TestRegionMapAcceptsNASDAQForUS(t *testing.T) {
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	ref := exchange.StockReference("AAPL", "US", "USDT", "Apple Inc", "bybit.instruments.underlyingTicker")
	resolution := resolutionFromContracts(ref, []ibkr.Contract{{
		ConID: 265598, Symbol: "AAPL", SecType: "STK", Currency: "USD", PrimaryExchange: "NASDAQ", LongName: "Apple Inc",
	}}, now)
	if !resolution.Active || resolution.PrimaryExchange != "NASDAQ" {
		t.Fatalf("resolution=%+v", resolution)
	}
	miss := resolutionFromContracts(ref, []ibkr.Contract{{
		ConID: 9, Symbol: "AAPL", SecType: "STK", Currency: "HKD", PrimaryExchange: "SEHK", LongName: "Apple Inc",
	}}, now)
	if miss.Status != MatchNotFound || miss.Active {
		t.Fatalf("currency miss=%+v", miss)
	}
}

func TestForbidDoesNotVerifyAtParity(t *testing.T) {
	if pricesMatch(100, 1, 100, 0.02, exchange.PricePolicyForbid) {
		t.Fatal("forbid must not pass a 1:1 comparison")
	}
	if !pricesMatch(100, 1, 100, 0.02, exchange.PricePolicyStablecoinParity) {
		t.Fatal("stablecoin parity compares the raw price")
	}
	hk := exchange.StockReference("0700", "HK", "USDT", "Tencent", "test")
	if hk.PriceConversionPolicy != exchange.PricePolicyForbid || !hk.Routable() {
		t.Fatalf("hk=%+v", hk)
	}
	lookup := &scriptLookup{contracts: []ibkr.Contract{{
		ConID: 700, Symbol: "0700", SecType: "STK", Currency: "HKD", PrimaryExchange: "SEHK", LongName: "Tencent",
	}}}
	service := testTradefi(lookup, newMemIBCatalog(), []Rate{freshRate("bybit", "0700USDT", "0700", 0.1)})
	instrument := exchange.Instrument{
		Exchange: "bybit", ExchangeSymbol: "0700USDT", BaseAsset: "0700",
		ContractType: exchange.ContractTypePerpetual, Status: "active", Underlying: hk,
	}
	if err := service.Reconcile(context.Background(), []exchange.Instrument{instrument}); err != nil {
		t.Fatal(err)
	}
	if len(service.Snapshots().Get().Rows) != 0 {
		t.Fatalf("rows=%+v", service.Snapshots().Get().Rows)
	}
}

func TestGatewayErrorDoesNotWriteNotFound(t *testing.T) {
	lookup := &scriptLookup{err: errors.New("gateway down")}
	catalog := newMemIBCatalog()
	service := testTradefi(lookup, catalog, nil)
	if err := service.Reconcile(context.Background(), []exchange.Instrument{equityPerp("bybit", "AAPLUSDT", "AAPL")}); err != nil {
		t.Fatal(err)
	}
	if lookup.count() != tradefiLookupAttempts || catalog.saveCount() != 0 {
		t.Fatalf("lookups=%d saves=%d", lookup.count(), catalog.saveCount())
	}
}

func TestBuildDropsDiagnosticsAndStaleQuotes(t *testing.T) {
	conID := int64(1)
	now := time.Now().UTC()
	rows := BuildTradefiRows([]IBResolution{{
		Symbol: "conid:1", UnderlyingSymbol: "AAPL", UnderlyingCurrency: "USD",
		Active: true, Status: "active", ConID: &conID, NextVerify: now.Add(time.Hour),
		PairMatches: map[string]PairMatch{
			"binance|AAPLUSDT":   {MatchClass: MatchCandidate},
			"okx|AAPL-USDT-SWAP": {MatchClass: MatchVerifiedListing, PerpPriceCurrency: "USD"},
			"entropy|AAPL":       {MatchClass: MatchVerifiedListing, PerpPriceCurrency: "USD"},
		},
	}}, []Rate{
		{Exchange: "binance", ExchangeSymbol: "AAPLUSDT", SourceUpdatedAt: now},
		{Exchange: "okx", ExchangeSymbol: "AAPL-USDT-SWAP", SourceUpdatedAt: now},
		{Exchange: "entropy", ExchangeSymbol: "AAPL", SourceUpdatedAt: now},
		{Exchange: "okx", ExchangeSymbol: "STALE", SourceUpdatedAt: now.Add(-time.Hour)},
	}, map[string]bool{"binance": true, "okx": true, "entropy": true})
	if len(rows) != 1 || rows[0].Exchange != "okx" {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestEachVenueKeepsItsOwnPriceCurrency(t *testing.T) {
	conID := int64(265598)
	catalog := newMemIBCatalog(IBResolution{
		Symbol: "conid:265598", UnderlyingSymbol: "AAPL", UnderlyingCurrency: "USD", SecType: "STK",
		Active: true, Status: "active", ConID: &conID, NextVerify: time.Now().Add(time.Hour),
		Aliases: []string{aaplKey},
	})
	service := testTradefi(&scriptLookup{}, catalog, nil)
	if err := service.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	binance := equityPerp("binance", "AAPLUSDT", "AAPL")
	binance.Underlying.PerpPriceCurrency = "USDT"
	okx := equityPerp("okx", "AAPL-USDT-SWAP", "AAPL")
	okx.Underlying.PerpPriceCurrency = "USDC"
	if err := service.Reconcile(context.Background(), []exchange.Instrument{binance, okx}); err != nil {
		t.Fatal(err)
	}
	item, ok := catalog.get("conid:265598")
	if !ok {
		t.Fatal("missing canonical row")
	}
	if item.PairMatches["binance|AAPLUSDT"].PerpPriceCurrency != "USDT" || item.PairMatches["okx|AAPL-USDT-SWAP"].PerpPriceCurrency != "USDC" {
		t.Fatalf("matches=%+v", item.PairMatches)
	}
}

func TestRestoreUnderlyingRoundTrip(t *testing.T) {
	item := exchange.Instrument{Exchange: "binance", ExchangeSymbol: "AAPLUSDT", BaseAsset: "AAPL", QuoteAsset: "USDT"}
	item.SetUnderlying(exchange.USDStockReference("AAPL", "USDT", "Apple", "binance"))
	stored := item
	stored.Underlying = exchange.UnderlyingReference{}
	restoreInstrumentUnderlying(&stored)
	if stored.Underlying.AssetClass != exchange.AssetClassStock || stored.Underlying.Symbol != "AAPL" || stored.Underlying.Region != "US" && stored.Underlying.UnderlyingCurrency != "USD" {
		t.Fatalf("restored=%+v", stored.Underlying)
	}
	if stored.Underlying.PerpPriceCurrency != "USD" || stored.Underlying.Source != "binance" {
		t.Fatalf("price=%+v", stored.Underlying)
	}
	broken := exchange.Instrument{Metadata: []byte(`{"underlyingReference":`)}
	restoreInstrumentUnderlying(&broken)
	if broken.Underlying.AssetClass != "" || broken.Underlying.Symbol != "" {
		t.Fatalf("corrupt=%+v", broken.Underlying)
	}
}

func TestFailedCatalogDoesNotDeactivate(t *testing.T) {
	writer := &memInstrumentWriter{}
	synchronizer := NewSynchronizer(nil, []exchange.Adapter{
		&catalogAdapter{name: "hyperliquid", err: errors.New("meta failed")},
	}, NewSnapshotStore(), nil)
	synchronizer.instrumentWrites = writer
	if _, err := synchronizer.SyncInstruments(context.Background()); err == nil {
		t.Fatal("catalog failure must be reported")
	}
	if len(writer.saved) != 0 || len(writer.deactivated) != 0 {
		t.Fatalf("saved=%v deactivated=%v", writer.saved, writer.deactivated)
	}
}

func aaplCanonical(active bool, status string, nextVerify time.Time) IBResolution {
	conID := int64(265598)
	venues := []struct{ venue, symbol string }{
		{"binance", "AAPLUSDT"},
		{"okx", "AAPL-USDT-SWAP"},
		{"bybit", "AAPLUSDT"},
		{"bitget", "AAPLUSDT"},
		{"gate", "AAPL_USDT"},
		{"hyperliquid", "xyz:AAPL"},
		{"aster", "AAPLUSDT"},
		{"lighter", "AAPL"},
	}
	matches := make(map[string]PairMatch, len(venues))
	for _, venue := range venues {
		matches[pairKey(venue.venue, venue.symbol)] = PairMatch{
			MatchClass: MatchVerifiedListing, PerpPriceCurrency: "USDT", Multiplier: 1,
		}
	}
	return IBResolution{
		Symbol: "conid:265598", UnderlyingSymbol: "AAPL", UnderlyingCurrency: "USD",
		AssetClass: "stock", Region: "US", SecType: "STK",
		Active: active, Status: status, ConID: &conID,
		NextVerify: nextVerify, LastVerified: time.Now().UTC(),
		PairMatches: matches, Aliases: []string{"AAPL", "stock|AAPL|USD|US"},
	}
}

func aaplRates(resolution IBResolution) []Rate {
	rates := make([]Rate, 0, len(resolution.PairMatches))
	for key := range resolution.PairMatches {
		venue, symbol, _ := strings.Cut(key, "|")
		rates = append(rates, freshRate(venue, symbol, "AAPL", 0.1))
	}
	return rates
}

func TestCanonicalOwnerResolutionsIgnoreAliasCopies(t *testing.T) {
	owner := aaplCanonical(true, "active", time.Now().Add(time.Hour))
	stale := owner
	stale.Active = false
	stale.Status = "inactive"
	stale.NextVerify = time.Now().Add(-time.Hour)
	stale.PairMatches = map[string]PairMatch{}
	cache := map[string]IBResolution{
		owner.Symbol:        owner,
		"AAPL":              stale,
		"stock|AAPL|USD|US": stale,
		"crypto|AAPL|USD|":  {Symbol: "crypto|AAPL|USD|", Status: "not_found"},
	}
	got := canonicalOwnerResolutions(cache)
	if len(got) != 2 {
		t.Fatalf("owners=%d %+v", len(got), got)
	}
	for _, item := range got {
		if item.Symbol != owner.Symbol && item.Symbol != "crypto|AAPL|USD|" {
			t.Fatalf("alias entered candidates: %+v", item)
		}
		if item.Symbol == owner.Symbol && (!item.Active || len(item.PairMatches) != 8) {
			t.Fatalf("owner was replaced by stale alias copy: %+v", item)
		}
	}
}

func TestPublishFundingIgnoresPollutedAlias(t *testing.T) {
	owner := aaplCanonical(true, "active", time.Now().Add(time.Hour))
	rates := aaplRates(owner)
	service := testTradefi(nil, newMemIBCatalog(), rates)
	service.store(owner)
	polluted := owner
	polluted.Active = false
	polluted.Status = "inactive"
	polluted.NextVerify = time.Now().Add(-time.Hour)
	polluted.PairMatches = nil
	service.mu.Lock()
	service.cache["AAPL"] = polluted
	service.cache["stock|AAPL|USD|US"] = polluted
	service.mu.Unlock()
	want := 0
	for i := 0; i < 100; i++ {
		service.PublishFunding(rates)
		got := len(service.Snapshots().Get().Rows)
		if i == 0 {
			want = got
		}
		if got != 8 || got != want {
			t.Fatalf("publish %d rows=%d want=8", i, got)
		}
	}
}

func TestRestorePrefersCanonicalOverInactiveAlias(t *testing.T) {
	canonical := aaplCanonical(true, "active", time.Now().Add(time.Hour))
	inactive := IBResolution{
		Symbol: "stock|AAPL|USD|US", UnderlyingSymbol: "AAPL",
		Active: false, Status: "inactive", NextVerify: time.Now().Add(-time.Hour),
	}
	crypto := IBResolution{
		Symbol: "crypto|AAPL|USD|", UnderlyingSymbol: "AAPL",
		Active: false, Status: "not_found", NextVerify: time.Now().Add(time.Hour),
	}
	service := testTradefi(nil, newMemIBCatalog(inactive, crypto, canonical), nil)
	if err := service.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, ok := service.cached("stock|AAPL|USD|US")
	if !ok || got.Symbol != "conid:265598" || !got.Active || got.ConID == nil || *got.ConID != 265598 {
		t.Fatalf("alias=%+v ok=%v", got, ok)
	}
	alias, ok := service.cached("AAPL")
	if !ok || alias.Symbol != "conid:265598" || !alias.Active {
		t.Fatalf("AAPL=%+v ok=%v", alias, ok)
	}
	negative, ok := service.cached("crypto|AAPL|USD|")
	if !ok || negative.Symbol != "crypto|AAPL|USD|" || negative.Status != "not_found" {
		t.Fatalf("crypto=%+v ok=%v", negative, ok)
	}
}

func TestInactiveCanonicalIsNotRevivedByActiveAlias(t *testing.T) {
	owner := aaplCanonical(false, "inactive", time.Now().Add(time.Hour))
	rates := aaplRates(owner)
	service := testTradefi(nil, newMemIBCatalog(), rates)
	service.store(owner)
	activeCopy := aaplCanonical(true, "active", time.Now().Add(time.Hour))
	service.mu.Lock()
	service.cache["AAPL"] = activeCopy
	service.mu.Unlock()
	service.PublishFunding(rates)
	if rows := service.Snapshots().Get().Rows; len(rows) != 0 {
		t.Fatalf("rows=%d", len(rows))
	}
}

func TestPersistStoresMergedCanonicalResolution(t *testing.T) {
	conID := int64(265598)
	existing := IBResolution{
		Symbol: "conid:265598", ConID: &conID, Active: true, Status: "active",
		UnderlyingSymbol: "AAPL", UnderlyingCurrency: "USD",
		Aliases:     []string{"AAPL"},
		PairMatches: map[string]PairMatch{"binance|AAPLUSDT": {MatchClass: MatchVerifiedListing, PerpPriceCurrency: "USDT"}},
		NextVerify:  time.Now().Add(time.Hour),
	}
	service := testTradefi(nil, newMemIBCatalog(existing), nil)
	if err := service.persist(context.Background(), IBResolution{
		ConID: &conID, Active: true, Status: "active",
		UnderlyingSymbol: "AAPL", UnderlyingCurrency: "USD",
		Aliases:     []string{"stock|AAPL|USD|US"},
		PairMatches: map[string]PairMatch{"okx|AAPL-USDT-SWAP": {MatchClass: MatchVerifiedListing, PerpPriceCurrency: "USDT"}},
		NextVerify:  time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"conid:265598", "AAPL", "stock|AAPL|USD|US"} {
		got, ok := service.cached(key)
		if !ok || got.Symbol != "conid:265598" {
			t.Fatalf("key=%s ok=%v item=%+v", key, ok, got)
		}
		if _, ok := got.PairMatches["binance|AAPLUSDT"]; !ok {
			t.Fatalf("key=%s pairs=%v", key, got.PairMatches)
		}
		if _, ok := got.PairMatches["okx|AAPL-USDT-SWAP"]; !ok {
			t.Fatalf("key=%s pairs=%v", key, got.PairMatches)
		}
		aliases := map[string]bool{}
		for _, alias := range got.Aliases {
			aliases[alias] = true
		}
		if !aliases["AAPL"] || !aliases["stock|AAPL|USD|US"] {
			t.Fatalf("key=%s aliases=%v", key, got.Aliases)
		}
	}
}

func TestCachedResolutionDoesNotMutateCache(t *testing.T) {
	conID := int64(265598)
	service := testTradefi(nil, newMemIBCatalog(), nil)
	service.store(IBResolution{
		Symbol: "conid:265598", ConID: &conID, Aliases: []string{"AAPL"},
		PairMatches: map[string]PairMatch{"binance|AAPLUSDT": {MatchClass: MatchVerifiedListing}},
	})
	got, ok := service.cached("conid:265598")
	if !ok {
		t.Fatal("missing cached resolution")
	}
	got.PairMatches["okx|X"] = PairMatch{MatchClass: MatchCandidate}
	got.Aliases[0] = "CHANGED"
	*got.ConID = 1
	again, ok := service.cached("AAPL")
	if !ok || again.Aliases[0] != "AAPL" || *again.ConID != 265598 {
		t.Fatalf("cache mutated %+v", again)
	}
	if _, exists := again.PairMatches["okx|X"]; exists {
		t.Fatalf("pair map shared %+v", again.PairMatches)
	}
}

func TestPublishFundingDoesNotRaceWithCachedMutation(t *testing.T) {
	owner := aaplCanonical(true, "active", time.Now().Add(time.Hour))
	rates := aaplRates(owner)
	service := testTradefi(nil, newMemIBCatalog(), rates)
	service.store(owner)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			got, ok := service.cached(owner.Symbol)
			if !ok {
				continue
			}
			got.PairMatches["extra"] = PairMatch{MatchClass: MatchCandidate}
			got.Aliases = append(got.Aliases, "extra")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			service.PublishFunding(rates)
		}
	}()
	wg.Wait()
}
