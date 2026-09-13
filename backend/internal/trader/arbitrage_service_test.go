package trader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/account/portfolio"
	"selfquant/backend/internal/trader/exchange"
)

type staticArbitrageValuation struct {
	value ArbitrageValuation
	ok    bool
}

func (s staticArbitrageValuation) ArbitrageValuation(
	string,
) (ArbitrageValuation, bool) {
	return s.value, s.ok
}

type arbitrageServiceCatalog struct {
	items map[int64]Instrument
}

func (c arbitrageServiceCatalog) Get(_ context.Context, id int64) (Instrument, error) {
	item, ok := c.items[id]
	if !ok {
		return Instrument{}, ErrInstrumentUnavailable
	}
	return item, nil
}

func (c arbitrageServiceCatalog) List(
	context.Context, string, string,
) ([]Instrument, error) {
	return nil, nil
}

type arbitrageServiceCredentials struct {
	owner string
	items map[int64]Credentials
}

type readinessArbitrageCredentials struct {
	arbitrageServiceCredentials
	readiness TradingReadiness
	err       error
	onInspect func()
}

func (c *readinessArbitrageCredentials) InspectTradingReadiness(
	context.Context, string, int64,
) (TradingReadiness, error) {
	if c.onInspect != nil {
		c.onInspect()
	}
	return c.readiness, c.err
}

func (c arbitrageServiceCredentials) Owner(context.Context, string) (string, error) {
	return c.owner, nil
}

func (c arbitrageServiceCredentials) Get(
	_ context.Context, _ string, id int64,
) (Credentials, error) {
	item, ok := c.items[id]
	if !ok {
		return Credentials{}, ErrNotFound
	}
	return item, nil
}

func (c arbitrageServiceCredentials) Meta(ctx context.Context, token string, id int64) (AccountMeta, error) {
	item, err := c.Get(ctx, token, id)
	if err != nil {
		return AccountMeta{}, err
	}
	return AccountMeta{
		TradingAccountID: item.TradingAccountID, ProductName: item.ProductName,
		Exchange: item.Exchange, AccountName: item.AccountName, CredentialKind: item.CredentialKind,
		AccountIndex: item.AccountIndex, APIKeyIndex: item.APIKeyIndex,
	}, nil
}

type arbitrageCaptureStore struct {
	arbitrageStore
	item ArbitrageCombination
	err  error
}

func (s *arbitrageCaptureStore) CreateArbitrageCombination(
	_ context.Context, item ArbitrageCombination,
) (ArbitrageCombination, bool, error) {
	s.item = item
	return item, s.err == nil, s.err
}

func (s *arbitrageCaptureStore) GetArbitrageCombinationByIdempotencyKey(
	_ context.Context, _, key string,
) (ArbitrageCombination, error) {
	if strings.TrimSpace(s.item.IdempotencyKey) != "" && s.item.IdempotencyKey == key {
		return s.item, nil
	}
	return ArbitrageCombination{}, ErrNotFound
}

type arbitrageBaselineSnapshots struct {
	mu        sync.Mutex
	calls     map[string]int
	snapshots map[string]portfolio.Snapshot
	err       error
}

func (s *arbitrageBaselineSnapshots) Snapshot(
	_ context.Context, venue string, _ portfolio.Credentials,
) (portfolio.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[venue]++
	return s.snapshots[venue], s.err
}

func readyArbitrageInstrument(
	id int64, venue, contractType, symbol string,
) Instrument {
	return Instrument{
		ID: id, Exchange: venue, ContractType: contractType,
		ExchangeSymbol: symbol, BaseAsset: "BEAT", QuoteAsset: "USDT",
		ContractSize: "1", PriceTick: "0.0001", QuantityStep: "1",
		MinQuantity: "1", MinQuantityStatus: exchange.ConstraintKnown,
		MinNotional: "1", MinNotionalStatus: exchange.ConstraintKnown,
		MaxQuantityStatus:  exchange.ConstraintNotApplicable,
		MarketQuantityStep: "1", MarketQuantityStepStatus: exchange.ConstraintKnown,
		MarketMinQuantity: "1", MarketMinQuantityStatus: exchange.ConstraintKnown,
		MarketMaxQuantityStatus: exchange.ConstraintNotApplicable,
		MarketMinNotional:       "1", MarketMinNotionalStatus: exchange.ConstraintKnown,
	}
}

func TestNormalizeUpdateArbitrageInput(t *testing.T) {
	id := "7f928919-1f36-45e9-9e78-7834465b220b"
	ask := " -7.500 "
	normalized, err := normalizeUpdateArbitrageInput(UpdateArbitrageInput{
		Token: " token ", CombinationID: id,
		AskThresholdBps: &ask,
	})
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Token != "token" || *normalized.AskThresholdBps != "-7.5" {
		t.Fatalf("normalized=%+v", normalized)
	}

	invalid := "not-a-decimal"
	_, err = normalizeUpdateArbitrageInput(UpdateArbitrageInput{
		Token: "token", CombinationID: id, BidThresholdBps: &invalid,
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid decimal err=%v", err)
	}
	_, err = normalizeUpdateArbitrageInput(UpdateArbitrageInput{
		Token: "token", CombinationID: id,
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty update err=%v", err)
	}
	zero := "0"
	_, err = normalizeUpdateArbitrageInput(UpdateArbitrageInput{
		Token: "token", CombinationID: id, TargetNotional: &zero,
	})
	if !errors.Is(err, ErrArbitrageConfigInvalid) {
		t.Fatalf("non-positive target err=%v", err)
	}
}

func TestCreateArbitrageCombinationCapturesVenueBaselines(t *testing.T) {
	instrumentA := readyArbitrageInstrument(11, "gate", "perpetual", "BEAT_USDT")
	instrumentB := readyArbitrageInstrument(22, "okx", "perpetual", "BEAT-USDT-SWAP")
	store := &arbitrageCaptureStore{}
	snapshots := &arbitrageBaselineSnapshots{
		calls: map[string]int{},
		snapshots: map[string]portfolio.Snapshot{
			"gate": {AvailableFundsUSD: "1000000", Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "gate", WireSymbol: "BEAT_USDT",
				SignedContractSize: "120",
			}}},
			"okx": {AvailableFundsUSD: "1000000", Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "okx", WireSymbol: "BEAT-USDT-SWAP",
				SignedContractSize: "-80",
			}}},
		},
	}
	service := NewService(
		newMemoryStore(),
		arbitrageServiceCatalog{items: map[int64]Instrument{
			instrumentA.ID: instrumentA, instrumentB.ID: instrumentB,
		}},
		arbitrageServiceCredentials{
			owner: "admin",
			items: map[int64]Credentials{
				1: {TradingAccountID: 1, ProductName: "ARB", AccountName: "gate-main", Exchange: "gate"},
				2: {TradingAccountID: 2, ProductName: "ARB", AccountName: "okx-main", Exchange: "okx"},
			},
		},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"gate": &stubAdapter{positionMode: exchange.PositionModeOneWay},
			"okx":  &stubAdapter{positionMode: exchange.PositionModeOneWay},
		}),
		time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	service.ConfigureArbitrage(store)
	service.ConfigureArbitragePositionSnapshots(snapshots)
	created, err := service.CreateArbitrageCombination(
		context.Background(),
		CreateArbitrageInput{
			Token: "token", LegATradingAccountID: 1, LegAInstrumentID: 11,
			LegBTradingAccountID: 2, LegBInstrumentID: 22,
			AskThresholdBps: "12", BidThresholdBps: "-8",
			TargetNotional: "10000",
			ExecutionMode:  "simultaneous_market", MakerLeg: "a",
			IdempotencyKey: "capture-baseline",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if created.LegAVenueBaselineBasePosition != "120" ||
		created.LegBVenueBaselineBasePosition != "-80" ||
		created.VenueBaselineCapturedAt.IsZero() {
		t.Fatalf("created=%+v", created)
	}
	if snapshots.calls["gate"] != 1 || snapshots.calls["okx"] != 1 {
		t.Fatalf("snapshot calls=%v", snapshots.calls)
	}
}

func TestCreateArbitrageCombinationReusesSameAccountSnapshot(t *testing.T) {
	spot := readyArbitrageInstrument(11, "gate", "spot", "BEAT_USDT")
	perpetual := readyArbitrageInstrument(22, "gate", "perpetual", "BEAT_USDT_PERP")
	store := &arbitrageCaptureStore{}
	snapshots := &arbitrageBaselineSnapshots{
		calls: map[string]int{},
		snapshots: map[string]portfolio.Snapshot{
			"gate": {
				AvailableFundsUSD: "1000000",
				SpotBalances:      map[string]string{"BEAT": "100"},
				Positions: []portfolio.Position{{
					Kind: "cex", Exchange: "gate", WireSymbol: "BEAT_USDT_PERP",
					SignedContractSize: "-50",
				}},
			},
		},
	}
	account := Credentials{
		TradingAccountID: 1, ProductName: "ARB",
		AccountName: "gate-main", Exchange: "gate",
	}
	service := NewService(
		newMemoryStore(),
		arbitrageServiceCatalog{items: map[int64]Instrument{
			spot.ID: spot, perpetual.ID: perpetual,
		}},
		arbitrageServiceCredentials{
			owner: "admin", items: map[int64]Credentials{1: account},
		},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"gate": &stubAdapter{positionMode: exchange.PositionModeOneWay},
		}),
		time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	service.ConfigureArbitrage(store)
	service.ConfigureArbitragePositionSnapshots(snapshots)
	created, err := service.CreateArbitrageCombination(
		context.Background(),
		CreateArbitrageInput{
			Token: "token", LegATradingAccountID: 1, LegAInstrumentID: 11,
			LegBTradingAccountID: 1, LegBInstrumentID: 22,
			AskThresholdBps: "12", BidThresholdBps: "-8",
			TargetNotional: "10000",
			ExecutionMode:  "simultaneous_market", MakerLeg: "a",
			IdempotencyKey: "same-account-baseline",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if snapshots.calls["gate"] != 1 ||
		created.LegAVenueBaselineBasePosition != "100" ||
		created.LegBVenueBaselineBasePosition != "-50" {
		t.Fatalf("created=%+v calls=%v", created, snapshots.calls)
	}
	snapshots.err = errors.New("snapshot unavailable")
	_, err = service.CreateArbitrageCombination(
		context.Background(),
		CreateArbitrageInput{
			Token: "token", LegATradingAccountID: 1, LegAInstrumentID: 11,
			LegBTradingAccountID: 1, LegBInstrumentID: 22,
			AskThresholdBps: "12", BidThresholdBps: "-8",
			TargetNotional: "10000",
			ExecutionMode:  "simultaneous_market", MakerLeg: "a",
			IdempotencyKey: "snapshot-failure",
		},
	)
	if !errors.Is(err, ErrVenueUnavailable) || store.item.ID != created.ID {
		t.Fatalf("snapshot failure err=%v store=%+v", err, store.item)
	}
}

func TestServiceDecoratesArbitrageWithSignedVenueNotional(t *testing.T) {
	valuedAt := time.Now().UTC()
	service := &Service{
		arbitrageValuations: staticArbitrageValuation{
			ok: true,
			value: ArbitrageValuation{
				LegAMid: "100", LegBMid: "1.01",
				LegAUpdatedAt: valuedAt, LegBUpdatedAt: valuedAt,
			},
		},
	}
	item := service.withArbitrageValuation(ArbitrageCombination{
		ID: "combo", LegAVenueBasePosition: "2.5",
		LegBVenueBasePosition: "-300",
	})
	if item.LegAVenueNotional != "250" ||
		item.LegBVenueNotional != "-303" ||
		item.LegAVenueValuationPrice != "100" ||
		item.LegBVenueValuationPrice != "1.01" ||
		!item.LegAVenueValuationAt.Equal(valuedAt) ||
		!item.LegBVenueValuationAt.Equal(valuedAt) {
		t.Fatalf("valued item=%+v", item)
	}
}

func TestValidArbitrageInstrumentPairSupportsSameAccountSpotPerpetual(t *testing.T) {
	account := Credentials{TradingAccountID: 7, ProductName: "BEAT套利", Exchange: "gate"}
	spot := Instrument{
		ID: 11, Exchange: "gate", ContractType: "spot",
		BaseAsset: "BEAT", QuoteAsset: "USDT",
	}
	perpetual := Instrument{
		ID: 12, Exchange: "gate", ContractType: "perpetual",
		BaseAsset: "BEAT", QuoteAsset: "USDT",
	}
	if !validArbitrageInstrumentPair(account, spot, account, perpetual) {
		t.Fatal("same-account spot/perpetual pair was rejected")
	}
	if !validArbitrageInstrumentPair(account, perpetual, account, spot) {
		t.Fatal("reversed same-account spot/perpetual pair was rejected")
	}
}

func TestValidArbitrageInstrumentPairRejectsUnsafeSameVenuePairs(t *testing.T) {
	account := Credentials{TradingAccountID: 7, ProductName: "BEAT套利", Exchange: "gate"}
	spot := Instrument{
		ID: 11, Exchange: "gate", ContractType: "spot",
		BaseAsset: "BEAT", QuoteAsset: "USDT",
	}
	otherSpot := spot
	otherSpot.ID = 12
	perpetual := spot
	perpetual.ID = 13
	perpetual.ContractType = "perpetual"

	tests := []struct {
		name string
		a    Instrument
		b    Instrument
	}{
		{name: "same instrument", a: spot, b: spot},
		{name: "two spot instruments", a: spot, b: otherSpot},
		{name: "different base", a: spot, b: withBase(perpetual, "OTHER")},
		{name: "different quote", a: spot, b: withQuote(perpetual, "USDC")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if validArbitrageInstrumentPair(account, test.a, account, test.b) {
				t.Fatal("unsafe same-venue pair was accepted")
			}
		})
	}
}

func TestValidArbitrageInstrumentPairPreservesCrossVenuePair(t *testing.T) {
	gate := Credentials{TradingAccountID: 7, Exchange: "gate"}
	okx := Credentials{TradingAccountID: 8, Exchange: "okx"}
	a := Instrument{
		ID: 11, Exchange: "gate", ContractType: "perpetual",
		BaseAsset: "BEAT", QuoteAsset: "USDT",
	}
	b := Instrument{
		ID: 12, Exchange: "okx", ContractType: "perpetual",
		BaseAsset: "BEAT", QuoteAsset: "USDT",
	}
	if !validArbitrageInstrumentPair(gate, a, okx, b) {
		t.Fatal("valid cross-venue pair was rejected")
	}
}

func TestValidArbitrageInstrumentPairSupportsPerpetualUSDCWithUSDT(t *testing.T) {
	tests := []struct {
		name           string
		venueA, quoteA string
		venueB, quoteB string
	}{
		{name: "hyperliquid and binance", venueA: "hyperliquid", quoteA: "USDC", venueB: "binance", quoteB: "USDT"},
		{name: "lighter and binance", venueA: "lighter", quoteA: "USDC", venueB: "binance", quoteB: "USDT"},
		{name: "lighter and aster", venueA: "lighter", quoteA: "USDC", venueB: "aster", quoteB: "USDT"},
		{name: "lighter and hyperliquid", venueA: "lighter", quoteA: "USDC", venueB: "hyperliquid", quoteB: "USDC"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			accountA := Credentials{TradingAccountID: 7, Exchange: test.venueA}
			accountB := Credentials{TradingAccountID: 8, Exchange: test.venueB}
			instrumentA := Instrument{
				ID: 11, Exchange: test.venueA, ContractType: "perpetual",
				BaseAsset: "BTC", QuoteAsset: test.quoteA,
			}
			instrumentB := Instrument{
				ID: 12, Exchange: test.venueB, ContractType: "perpetual",
				BaseAsset: "BTC", QuoteAsset: test.quoteB,
			}
			if !validArbitrageInstrumentPair(accountA, instrumentA, accountB, instrumentB) {
				t.Fatalf("%s/%s pair was rejected", test.venueA, test.venueB)
			}
		})
	}
	spotUSDT := Instrument{
		ID: 20, Exchange: "binance", ContractType: "spot",
		BaseAsset: "BTC", QuoteAsset: "USDT",
	}
	lighterUSDC := Instrument{
		ID: 21, Exchange: "lighter", ContractType: "perpetual",
		BaseAsset: "BTC", QuoteAsset: "USDC",
	}
	if validArbitrageInstrumentPair(
		Credentials{Exchange: "binance"}, spotUSDT,
		Credentials{Exchange: "lighter"}, lighterUSDC,
	) {
		t.Fatal("cross-stablecoin spot/perpetual pair was accepted")
	}
}

func TestInstrumentRulesReadyFailsClosedUntilSyncCompletes(t *testing.T) {
	instrument := Instrument{
		PriceTick: "0.1", QuantityStep: "0.001",
		MinQuantity: "0.001", MinQuantityStatus: exchange.ConstraintKnown,
		MaxQuantityStatus: exchange.ConstraintNotApplicable,
		MinNotional:       "5", MinNotionalStatus: exchange.ConstraintKnown,
		MarketQuantityStep: "0.001", MarketQuantityStepStatus: exchange.ConstraintKnown,
		MarketMinQuantity: "0.001", MarketMinQuantityStatus: exchange.ConstraintKnown,
		MarketMaxQuantityStatus: exchange.ConstraintNotApplicable,
		MarketMinNotional:       "5", MarketMinNotionalStatus: exchange.ConstraintKnown,
	}
	if !instrumentRulesReady(instrument, "limit") ||
		!instrumentRulesReady(instrument, "market") {
		t.Fatal("known synchronized rules were rejected")
	}
	instrument.MarketMinNotionalStatus = exchange.ConstraintUnknown
	if instrumentRulesReady(instrument, "market") {
		t.Fatal("unknown market rule did not fail closed")
	}
}

func TestEnsureArbitrageAccountReadyFailsClosedAndRefreshesLighterIndexes(t *testing.T) {
	accountIndex := int64(7)
	apiKeyIndex := int32(3)
	provider := &readinessArbitrageCredentials{
		arbitrageServiceCredentials: arbitrageServiceCredentials{
			items: map[int64]Credentials{1: {
				TradingAccountID: 1, Exchange: "lighter",
			}},
		},
		readiness: TradingReadiness{
			Ready: true, ResolvedAccountIndex: &accountIndex,
			ResolvedAPIKeyIndex: &apiKeyIndex,
		},
	}
	provider.onInspect = func() {
		refreshed := provider.items[1]
		refreshed.AccountIndex = &accountIndex
		refreshed.APIKeyIndex = &apiKeyIndex
		provider.items[1] = refreshed
	}
	service := &Service{credentials: provider, timeout: time.Second}
	refreshed, err := service.ensureArbitrageAccountReady(
		context.Background(), "token", provider.items[1],
	)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.AccountIndex == nil || *refreshed.AccountIndex != accountIndex ||
		refreshed.APIKeyIndex == nil || *refreshed.APIKeyIndex != apiKeyIndex {
		t.Fatalf("refreshed=%+v", refreshed)
	}

	provider.readiness = TradingReadiness{
		Ready: false, UnavailableCode: "wallet_unauthorized",
		UnavailableReason: "wallet is not authorized",
	}
	_, err = service.ensureArbitrageAccountReady(
		context.Background(), "token", provider.items[1],
	)
	if !errors.Is(err, ErrVenueUnavailable) {
		t.Fatalf("unready error=%v", err)
	}
}

func withBase(instrument Instrument, base string) Instrument {
	instrument.BaseAsset = base
	return instrument
}

func withQuote(instrument Instrument, quote string) Instrument {
	instrument.QuoteAsset = quote
	return instrument
}

func TestCreateArbitrageCombinationIdempotencySkipsLeverage(t *testing.T) {
	instrumentA := readyArbitrageInstrument(11, "gate", "perpetual", "BEAT_USDT")
	instrumentB := readyArbitrageInstrument(22, "okx", "perpetual", "BEAT-USDT-SWAP")
	store := &arbitrageCaptureStore{}
	adapterA := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	snapshots := &arbitrageBaselineSnapshots{
		calls: map[string]int{},
		snapshots: map[string]portfolio.Snapshot{
			"gate": {AvailableFundsUSD: "1000000"},
			"okx":  {AvailableFundsUSD: "1000000"},
		},
	}
	service := NewService(
		newMemoryStore(),
		arbitrageServiceCatalog{items: map[int64]Instrument{
			instrumentA.ID: instrumentA, instrumentB.ID: instrumentB,
		}},
		arbitrageServiceCredentials{
			owner: "admin",
			items: map[int64]Credentials{
				1: {TradingAccountID: 1, ProductName: "ARB", AccountName: "gate-main", Exchange: "gate"},
				2: {TradingAccountID: 2, ProductName: "ARB", AccountName: "okx-main", Exchange: "okx"},
			},
		},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"gate": adapterA, "okx": adapterB}),
		time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	service.ConfigureArbitrage(store)
	service.ConfigureArbitragePositionSnapshots(snapshots)
	input := CreateArbitrageInput{
		Token: "token", LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional: "10000",
		ExecutionMode:  "simultaneous_market", MakerLeg: "a",
		IdempotencyKey: "idempotent-create",
	}
	first, err := service.CreateArbitrageCombination(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if adapterA.leverageSets != 1 || adapterB.leverageSets != 1 {
		t.Fatalf("first create leverage sets a=%d b=%d", adapterA.leverageSets, adapterB.leverageSets)
	}
	second, err := service.CreateArbitrageCombination(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("second=%+v first=%+v", second, first)
	}
	if adapterA.leverageSets != 1 || adapterB.leverageSets != 1 {
		t.Fatalf("idempotent retry set leverage a=%d b=%d", adapterA.leverageSets, adapterB.leverageSets)
	}
	if snapshots.calls["gate"] != 1 || snapshots.calls["okx"] != 1 {
		t.Fatalf("idempotent retry recaptured snapshots=%v", snapshots.calls)
	}
}

func TestCreateArbitrageCombinationPartialLeverageFailure(t *testing.T) {
	instrumentA := readyArbitrageInstrument(11, "gate", "perpetual", "BEAT_USDT")
	instrumentB := readyArbitrageInstrument(22, "okx", "perpetual", "BEAT-USDT-SWAP")
	store := &arbitrageCaptureStore{}
	adapterA := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	adapterB := &stubAdapter{
		positionMode:   exchange.PositionModeOneWay,
		setLeverageErr: errors.New("venue rejected leverage"),
	}
	snapshots := &arbitrageBaselineSnapshots{
		calls: map[string]int{},
		snapshots: map[string]portfolio.Snapshot{
			"gate": {AvailableFundsUSD: "1000000"},
			"okx":  {AvailableFundsUSD: "1000000"},
		},
	}
	service := NewService(
		newMemoryStore(),
		arbitrageServiceCatalog{items: map[int64]Instrument{
			instrumentA.ID: instrumentA, instrumentB.ID: instrumentB,
		}},
		arbitrageServiceCredentials{
			owner: "admin",
			items: map[int64]Credentials{
				1: {TradingAccountID: 1, ProductName: "ARB", AccountName: "gate-main", Exchange: "gate"},
				2: {TradingAccountID: 2, ProductName: "ARB", AccountName: "okx-main", Exchange: "okx"},
			},
		},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"gate": adapterA, "okx": adapterB}),
		time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	service.ConfigureArbitrage(store)
	service.ConfigureArbitragePositionSnapshots(snapshots)
	_, err := service.CreateArbitrageCombination(
		context.Background(),
		CreateArbitrageInput{
			Token: "token", LegATradingAccountID: 1, LegAInstrumentID: 11,
			LegBTradingAccountID: 2, LegBInstrumentID: 22,
			AskThresholdBps: "12", BidThresholdBps: "-8",
			TargetNotional: "10000",
			ExecutionMode:  "simultaneous_market", MakerLeg: "a",
			IdempotencyKey: "partial-leverage",
		},
	)
	var createErr *ArbitrageCreateError
	if !errors.As(err, &createErr) || createErr.Code != "leverage_apply_failed" {
		t.Fatalf("err=%v", err)
	}
	if createErr.Details["appliedLegs"] != `["a"]` {
		t.Fatalf("details=%v", createErr.Details)
	}
	if adapterA.leverageSets != 1 || adapterB.leverageSets != 1 {
		t.Fatalf("leverage sets a=%d b=%d", adapterA.leverageSets, adapterB.leverageSets)
	}
	if store.item.ID != "" {
		t.Fatalf("combination was inserted: %+v", store.item)
	}
}

type conflictBeforeLeverageStore struct {
	arbitrageCaptureStore
	checked  int
	conflict error
}

func (s *conflictBeforeLeverageStore) FindActiveArbitrageInstrumentConflict(
	context.Context, string, string, int64, int64, int64, int64,
) error {
	s.checked++
	return s.conflict
}

func TestCreateArbitrageCombinationConflictSkipsLeverage(t *testing.T) {
	instrumentA := readyArbitrageInstrument(11, "gate", "perpetual", "BEAT_USDT")
	instrumentB := readyArbitrageInstrument(22, "okx", "perpetual", "BEAT-USDT-SWAP")
	store := &conflictBeforeLeverageStore{
		conflict: fmt.Errorf("%w: occupied", ErrActiveArbitrageInstrumentConflict),
	}
	adapterA := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	snapshots := &arbitrageBaselineSnapshots{
		calls: map[string]int{},
		snapshots: map[string]portfolio.Snapshot{
			"gate": {AvailableFundsUSD: "1000000"},
			"okx":  {AvailableFundsUSD: "1000000"},
		},
	}
	service := NewService(
		newMemoryStore(),
		arbitrageServiceCatalog{items: map[int64]Instrument{
			instrumentA.ID: instrumentA, instrumentB.ID: instrumentB,
		}},
		arbitrageServiceCredentials{
			owner: "admin",
			items: map[int64]Credentials{
				1: {TradingAccountID: 1, ProductName: "ARB", AccountName: "gate-main", Exchange: "gate"},
				2: {TradingAccountID: 2, ProductName: "ARB", AccountName: "okx-main", Exchange: "okx"},
			},
		},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"gate": adapterA, "okx": adapterB}),
		time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	service.ConfigureArbitrage(store)
	service.ConfigureArbitragePositionSnapshots(snapshots)
	_, err := service.CreateArbitrageCombination(
		context.Background(),
		CreateArbitrageInput{
			Token: "token", LegATradingAccountID: 1, LegAInstrumentID: 11,
			LegBTradingAccountID: 2, LegBInstrumentID: 22,
			AskThresholdBps: "12", BidThresholdBps: "-8",
			TargetNotional: "10000",
			ExecutionMode:  "simultaneous_market", MakerLeg: "a",
			IdempotencyKey: "conflict-before-leverage",
		},
	)
	if !errors.Is(err, ErrActiveArbitrageInstrumentConflict) {
		t.Fatalf("err=%v", err)
	}
	if store.checked != 1 {
		t.Fatalf("conflict checked=%d", store.checked)
	}
	if adapterA.leverageSets != 0 || adapterB.leverageSets != 0 {
		t.Fatalf("leverage sets a=%d b=%d", adapterA.leverageSets, adapterB.leverageSets)
	}
	if snapshots.calls["gate"] != 0 || snapshots.calls["okx"] != 0 {
		t.Fatalf("snapshots captured on conflict: %v", snapshots.calls)
	}
}

func TestExistingDirectionNotionalUsesMarkPrice(t *testing.T) {
	instrument := readyArbitrageInstrument(11, "gate", "perpetual", "BEAT_USDT")
	leg := ArbitrageLeg{
		Exchange: "gate", ContractType: "perpetual", ExchangeSymbol: "BEAT_USDT",
		BaseAsset: "BEAT",
	}
	snapshot := portfolio.Snapshot{
		Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "gate", Symbol: "BEAT_USDT",
			SignedContractSize: "2", MarkPrice: "50",
		}},
	}
	got := existingDirectionNotional(snapshot, leg, instrument, snapshotMarkPrice(snapshot, leg))
	if !got.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("existing notional=%s want 100", got)
	}
}
