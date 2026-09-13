package trader

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/account/portfolio"
	"selfquant/backend/internal/trader/exchange"
)

type positionAuditStore struct {
	item          ArbitrageCombination
	items         []ArbitrageCombination
	orders        []Order
	uncertain     bool
	events        int
	eventTypes    []string
	eventPayloads []map[string]any
	message       string
	updateErr     error
	recomputeIDs  []string
	auditUpdates  int
	applyCalls    int
	applyRejected bool
	applyErr      error
}

type auditValuations map[string]ArbitrageValuation

func (v auditValuations) ArbitrageValuation(
	id string,
) (ArbitrageValuation, bool) {
	value, ok := v[id]
	return value, ok
}

func (s *positionAuditStore) ListArbitrageCombinationsForPositionAudit(
	context.Context, int,
) ([]ArbitrageCombination, error) {
	if len(s.items) > 0 {
		return s.items, nil
	}
	return []ArbitrageCombination{s.item}, nil
}

func (s *positionAuditStore) UpdateArbitragePositionAudit(
	_ context.Context,
	_ string,
	_ int64,
	venueA, venueB, differenceA, differenceB string,
	uncertain bool,
	message string,
) (ArbitrageCombination, error) {
	if s.updateErr != nil {
		return ArbitrageCombination{}, s.updateErr
	}
	s.auditUpdates++
	s.item.LegAVenueBasePosition = venueA
	s.item.LegBVenueBasePosition = venueB
	s.item.LegAPositionDifference = differenceA
	s.item.LegBPositionDifference = differenceB
	previousRuntime := s.item.RuntimeState
	previousUncertain := s.item.PositionUncertain
	isAuditSource := strings.HasPrefix(
		s.item.ErrorMessage, arbitragePositionAuditErrorPrefix,
	)
	if uncertain &&
		(!s.item.PositionUncertain || s.item.ErrorMessage == "" || isAuditSource) {
		s.item.PositionUncertain = true
		s.item.RuntimeState = "position_uncertain"
		s.item.ErrorMessage = message
	} else if !uncertain && s.item.PositionUncertain && isAuditSource {
		s.item.PositionUncertain = false
		s.item.RuntimeState = "monitoring"
		s.item.ErrorMessage = ""
	} else if !uncertain && s.item.PositionUncertain &&
		s.item.ErrorMessage == "" && s.item.RuntimeState == "monitoring" &&
		differenceA == "0" && differenceB == "0" {
		s.item.PositionUncertain = false
	}
	if !uncertain &&
		s.item.Status == "running" &&
		!s.item.CircuitOpen &&
		!previousUncertain &&
		!s.item.PositionUncertain &&
		previousRuntime == "manual_intervention" &&
		exactZeroDecimal(differenceA) && exactZeroDecimal(differenceB) {
		s.item.RuntimeState = "monitoring"
		s.item.ErrorMessage = ""
	}
	s.uncertain = s.item.PositionUncertain
	s.message = s.item.ErrorMessage
	return s.item, nil
}

func (s *positionAuditStore) AppendArbitrageEvent(
	_ context.Context, _, _, eventType string, payload map[string]any,
) error {
	s.events++
	s.eventTypes = append(s.eventTypes, eventType)
	s.eventPayloads = append(s.eventPayloads, payload)
	return nil
}

func (s *positionAuditStore) ListArbitrageOrders(
	context.Context, string, string,
) ([]Order, error) {
	return s.orders, nil
}

func (s *positionAuditStore) UpdateResultWithFillDelta(
	_ context.Context, id string, result VenueResult,
) (Order, bool, error) {
	for index := range s.orders {
		if s.orders[index].ID == id {
			changed := !parseDecimal(s.orders[index].FilledQuantity).Equal(
				parseDecimal(result.FilledQuantity),
			)
			s.orders[index].FilledQuantity = result.FilledQuantity
			s.orders[index].AveragePrice = result.AveragePrice
			return s.orders[index], changed, nil
		}
	}
	return Order{}, false, nil
}

func (s *positionAuditStore) RecomputeArbitrageBasePositionsForExecution(
	_ context.Context, executionID string,
) (ArbitrageCombination, error) {
	s.recomputeIDs = append(s.recomputeIDs, executionID)
	a, b := decimal.Zero, decimal.Zero
	for _, order := range s.orders {
		quantity := parseDecimal(order.FilledQuantity)
		if order.Side == "sell" {
			quantity = quantity.Neg()
		}
		if order.ArbitrageLeg == "b" {
			b = b.Add(quantity)
		} else {
			a = a.Add(quantity)
		}
	}
	s.item.LegABasePosition = a.String()
	s.item.LegBBasePosition = b.String()
	return s.item, nil
}

func (s *positionAuditStore) ApplyClosingExternalFlatReconcile(
	_ context.Context,
	request closingExternalFlatReconcileRequest,
) (ArbitrageCombination, bool, error) {
	s.applyCalls++
	if s.applyErr != nil {
		return ArbitrageCombination{}, false, s.applyErr
	}
	if s.applyRejected {
		return s.item, false, nil
	}
	baselineA := parseDecimal(s.item.LegAVenueBaselineBasePosition)
	baselineB := parseDecimal(s.item.LegBVenueBaselineBasePosition)
	diffA := request.VenueA.Sub(baselineA.Add(parseDecimal(s.item.LegABasePosition)))
	diffB := request.VenueB.Sub(baselineB.Add(parseDecimal(s.item.LegBBasePosition)))
	s.item.LegAReconciliationAdjustment = parseDecimal(s.item.LegAReconciliationAdjustment).
		Add(diffA).String()
	s.item.LegBReconciliationAdjustment = parseDecimal(s.item.LegBReconciliationAdjustment).
		Add(diffB).String()
	s.item.LegABasePosition = "0"
	s.item.LegBBasePosition = "0"
	s.item.CarryBaseQuantity = "0"
	s.item.PositionNotional = "0"
	s.item.LegAPositionDifference = "0"
	s.item.LegBPositionDifference = "0"
	s.item.LegAVenueBasePosition = request.VenueA.String()
	s.item.LegBVenueBasePosition = request.VenueB.String()
	s.item.PositionUncertain = false
	s.item.RuntimeState = "closing"
	s.item.ErrorMessage = ""
	s.item.Version++
	s.uncertain = false
	s.message = ""
	return s.item, true, nil
}

type positionAuditCredentials struct{}

func (positionAuditCredentials) GetInternal(
	_ context.Context, _, _ string, accountID int64,
) (Credentials, error) {
	exchange := "gate"
	if accountID == 2 {
		exchange = "okx"
	}
	return Credentials{
		TradingAccountID: accountID, Exchange: exchange,
		APIKey: "key", APISecret: "secret",
	}, nil
}

type positionAuditPortfolios struct {
	snapshots map[string]portfolio.Snapshot
	fills     map[string][]portfolio.TradeFill
}

func (p positionAuditPortfolios) Snapshot(
	_ context.Context, exchange string, _ portfolio.Credentials,
) (portfolio.Snapshot, error) {
	return p.snapshots[exchange], nil
}

func (p positionAuditPortfolios) TradeFills(
	_ context.Context, exchange string, _ portfolio.Credentials, _ portfolio.TradeQuery,
) ([]portfolio.TradeFill, error) {
	return p.fills[exchange], nil
}

type positionAuditCatalog struct {
	items map[int64]Instrument
}

func (c positionAuditCatalog) Get(
	_ context.Context, id int64,
) (Instrument, error) {
	return c.items[id], nil
}

func (c positionAuditCatalog) List(
	context.Context, string, string,
) ([]Instrument, error) {
	return nil, nil
}

type positionAuditDEXCredentials struct{}

func (positionAuditDEXCredentials) GetInternal(
	_ context.Context, _, _ string, accountID int64,
) (Credentials, error) {
	if accountID == 2 {
		accountIndex, apiKeyIndex := int64(7), int32(2)
		return Credentials{
			TradingAccountID: accountID, Exchange: "hyperliquid",
			APIKey: "owner", APISecret: "secret", SigningAddress: "signer",
			AccountIndex: &accountIndex, APIKeyIndex: &apiKeyIndex,
		}, nil
	}
	return Credentials{
		TradingAccountID: accountID, Exchange: "gate",
		APIKey: "key", APISecret: "secret",
	}, nil
}

type positionAuditDEXFillReader struct {
	calls       int
	credentials exchange.Credentials
	instrument  exchange.Instrument
	fills       []exchange.Fill
}

func (*positionAuditDEXFillReader) GetBBO(
	context.Context, exchange.Instrument,
) (exchange.BBO, error) {
	return exchange.BBO{}, nil
}

func (*positionAuditDEXFillReader) PlaceOrder(
	context.Context, exchange.Credentials, exchange.OrderRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}

func (*positionAuditDEXFillReader) GetOrder(
	context.Context, exchange.Credentials, exchange.QueryRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}

func (*positionAuditDEXFillReader) CancelOrder(
	context.Context, exchange.Credentials, exchange.CancelRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}

func (r *positionAuditDEXFillReader) CancelAndGetOrder(
	ctx context.Context, credentials exchange.Credentials, request exchange.CancelRequest,
) (exchange.Result, error) {
	return r.CancelOrder(ctx, credentials, request)
}

func (r *positionAuditDEXFillReader) ListFills(
	_ context.Context,
	credentials exchange.Credentials,
	instrument exchange.Instrument,
	_ time.Time,
) ([]exchange.Fill, error) {
	r.calls++
	r.credentials = credentials
	r.instrument = instrument
	return r.fills, nil
}

func TestPositionAuditorConvertsVenueContractsAndFlagsDifference(t *testing.T) {
	store := &positionAuditStore{item: ArbitrageCombination{
		ID: "combo-1", OwnerUsername: "admin",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 10, Exchange: "gate",
			ContractType: "perpetual", ExchangeSymbol: "BEAT_USDT",
			BaseAsset: "BEAT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 20, Exchange: "okx",
			ContractType: "perpetual", ExchangeSymbol: "BEAT-USDT-SWAP",
			BaseAsset: "BEAT",
		},
		LegABasePosition: "150", LegBBasePosition: "-160",
	}}
	catalog := positionAuditCatalog{items: map[int64]Instrument{
		10: {
			ID: 10, Exchange: "gate", ContractType: "perpetual",
			ExchangeSymbol: "BEAT_USDT", ContractSize: "10", QuantityStep: "10",
		},
		20: {
			ID: 20, Exchange: "okx", ContractType: "perpetual",
			ExchangeSymbol: "BEAT-USDT-SWAP", ContractSize: "10", QuantityStep: "10",
		},
	}}
	auditor := NewArbitragePositionAuditor(
		store, catalog, positionAuditCredentials{},
		positionAuditPortfolios{snapshots: map[string]portfolio.Snapshot{
			"gate": {Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "Gate", WireSymbol: "BEAT_USDT",
				SignedContractSize: "15",
			}}},
			"okx": {Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "OKX", WireSymbol: "BEAT-USDT-SWAP",
				SignedContractSize: "-16",
			}}},
		}},
		"token", time.Minute, time.Second, 20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := auditor.audit(context.Background(), store.item, nil); err != nil {
		t.Fatal(err)
	}
	if store.item.LegAVenueBasePosition != "150" ||
		store.item.LegBVenueBasePosition != "-160" ||
		store.item.LegAPositionDifference != "0" ||
		store.item.LegBPositionDifference != "0" {
		t.Fatalf("item=%+v", store.item)
	}
	if store.uncertain {
		t.Fatal("matching venue and ledger positions should pass")
	}

	store.item.LegBBasePosition = "-140"
	if err := auditor.audit(context.Background(), store.item, nil); err != nil {
		t.Fatal(err)
	}
	if !store.uncertain || store.events != 1 {
		t.Fatalf("uncertain=%t events=%d item=%+v", store.uncertain, store.events, store.item)
	}
	if err := auditor.audit(context.Background(), store.item, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(store.message, arbitragePositionAuditErrorPrefix) ||
		store.events != 1 {
		t.Fatalf("second audit message=%q events=%d", store.message, store.events)
	}
}

func TestPositionAuditorUsesCapturedBaselineForNewCombination(t *testing.T) {
	capturedAt := time.Now().UTC().Add(-time.Hour)
	store := &positionAuditStore{item: ArbitrageCombination{
		ID: "combo-baseline", OwnerUsername: "admin",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 10, Exchange: "gate",
			ContractType: "perpetual", ExchangeSymbol: "BEAT_USDT",
			BaseAsset: "BEAT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 20, Exchange: "okx",
			ContractType: "perpetual", ExchangeSymbol: "BEAT-USDT-SWAP",
			BaseAsset: "BEAT",
		},
		LegABasePosition:              "50",
		LegBBasePosition:              "-60",
		LegAVenueBaselineBasePosition: "100",
		LegBVenueBaselineBasePosition: "-100",
		VenueBaselineCapturedAt:       capturedAt,
	}}
	catalog := positionAuditCatalog{items: map[int64]Instrument{
		10: {
			ID: 10, Exchange: "gate", ContractType: "perpetual",
			ExchangeSymbol: "BEAT_USDT", ContractSize: "10", QuantityStep: "10",
		},
		20: {
			ID: 20, Exchange: "okx", ContractType: "perpetual",
			ExchangeSymbol: "BEAT-USDT-SWAP", ContractSize: "10", QuantityStep: "10",
		},
	}}
	portfolios := positionAuditPortfolios{snapshots: map[string]portfolio.Snapshot{
		"gate": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "Gate", WireSymbol: "BEAT_USDT",
			SignedContractSize: "15",
		}}},
		"okx": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "OKX", WireSymbol: "BEAT-USDT-SWAP",
			SignedContractSize: "-16",
		}}},
	}}
	auditor := NewArbitragePositionAuditor(
		store, catalog, positionAuditCredentials{}, portfolios,
		"token", time.Minute, time.Second, 20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := auditor.audit(context.Background(), store.item, nil); err != nil {
		t.Fatal(err)
	}
	if store.uncertain || store.item.LegAPositionDifference != "0" ||
		store.item.LegBPositionDifference != "0" {
		t.Fatalf("matching baseline delta must pass: %+v", store.item)
	}
	if store.item.LegAVenueBaselineBasePosition != "100" ||
		!store.item.VenueBaselineCapturedAt.Equal(capturedAt) {
		t.Fatalf("audit overwrote immutable baseline: %+v", store.item)
	}

	portfolios.snapshots["gate"] = portfolio.Snapshot{Positions: []portfolio.Position{{
		Kind: "cex", Exchange: "Gate", WireSymbol: "BEAT_USDT",
		SignedContractSize: "16",
	}}}
	auditor.portfolios = portfolios
	if err := auditor.audit(context.Background(), store.item, nil); err != nil {
		t.Fatal(err)
	}
	if !store.uncertain || store.item.LegAPositionDifference != "10" ||
		!strings.Contains(store.message, "leg_a_expected=150") ||
		!strings.Contains(store.message, "leg_a_observed=160") {
		t.Fatalf("manual drift was not flagged: %+v message=%q", store.item, store.message)
	}
}

func TestPositionDifferenceMaterialUsesExecutableMinimums(t *testing.T) {
	instrument := Instrument{
		QuantityStep:      "1",
		MinQuantity:       "10",
		MinQuantityStatus: exchange.ConstraintKnown,
		MinNotionalStatus: exchange.ConstraintNotApplicable,
	}
	if positionDifferenceMaterial(decimal.NewFromInt(5), decimal.NewFromInt(2), instrument) {
		t.Fatal("difference below minimum quantity should be dust")
	}
	if !positionDifferenceMaterial(decimal.NewFromInt(10), decimal.NewFromInt(2), instrument) {
		t.Fatal("executable difference should be material")
	}

	instrument.MinQuantity = "1"
	instrument.MinNotional = "10"
	instrument.MinNotionalStatus = exchange.ConstraintKnown
	if positionDifferenceMaterial(decimal.NewFromInt(5), decimal.NewFromInt(1), instrument) {
		t.Fatal("difference below minimum notional should be dust")
	}
	if !positionDifferenceMaterial(decimal.NewFromInt(5), decimal.NewFromInt(2), instrument) {
		t.Fatal("difference meeting minimum notional should be material")
	}

	instrument.MinQuantityStatus = exchange.ConstraintUnknown
	if !positionDifferenceMaterial(decimal.NewFromInt(1), decimal.NewFromInt(2), instrument) {
		t.Fatal("unknown constraints must fail closed after the step threshold")
	}
}

func TestPositionAuditUsesLiveMidForZeroLedgerMark(t *testing.T) {
	item := ArbitrageCombination{ID: "combo-zero-mark"}
	auditor := &ArbitragePositionAuditor{
		valuations: auditValuations{
			item.ID: {
				LegAMid: "100",
				LegBMid: "101",
			},
		},
	}
	markA, markB := auditor.arbitragePositionAuditMarks(item)
	if !markA.Equal(decimal.NewFromInt(100)) ||
		!markB.Equal(decimal.NewFromInt(101)) {
		t.Fatalf("marks=%s/%s", markA, markB)
	}
	instrument := Instrument{
		QuantityStep: "0.001", MinQuantity: "0.001", MinNotional: "10",
		MinQuantityStatus: exchange.ConstraintKnown,
		MaxQuantityStatus: exchange.ConstraintNotApplicable,
		MinNotionalStatus: exchange.ConstraintKnown,
	}
	if positionDifferenceMaterial(
		decimal.RequireFromString("0.001"), markA, instrument,
	) {
		t.Fatal("dust difference was classified as material with live mid")
	}
}

func TestPositionAuditorUsesTradeFillFallbackBeforeFlaggingDifference(t *testing.T) {
	item := ArbitrageCombination{
		ID: "combo-2", OwnerUsername: "admin", CreatedAt: time.Now().Add(-time.Hour),
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 10, Exchange: "gate",
			ContractType: "perpetual", ExchangeSymbol: "BEAT_USDT", BaseAsset: "BEAT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 20, Exchange: "okx",
			ContractType: "perpetual", ExchangeSymbol: "BEAT-USDT-SWAP", BaseAsset: "BEAT",
		},
		LegABasePosition: "150", LegBBasePosition: "-150",
	}
	store := &positionAuditStore{
		item: item,
		orders: []Order{
			{
				ID: "a", VenueOrderID: "venue-a", TradingAccountID: 1,
				ArbitrageExecutionID: "exec-a", ArbitrageLeg: "a", Side: "buy",
				Quantity: "200", FilledQuantity: "150",
			},
			{
				ID: "b", VenueOrderID: "venue-b", TradingAccountID: 2,
				ArbitrageExecutionID: "exec-b", ArbitrageLeg: "b", Side: "sell",
				Quantity: "200", FilledQuantity: "150",
				Status: "canceled",
			},
		},
	}
	catalog := positionAuditCatalog{items: map[int64]Instrument{
		10: {
			ID: 10, Exchange: "gate", ContractType: "perpetual",
			ExchangeSymbol: "BEAT_USDT", ContractSize: "10", QuantityStep: "10",
		},
		20: {
			ID: 20, Exchange: "okx", ContractType: "perpetual",
			ExchangeSymbol: "BEAT-USDT-SWAP", ContractSize: "10", QuantityStep: "10",
		},
	}}
	portfolios := positionAuditPortfolios{
		snapshots: map[string]portfolio.Snapshot{
			"gate": {Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "Gate", WireSymbol: "BEAT_USDT",
				SignedContractSize: "15",
			}}},
			"okx": {Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "OKX", WireSymbol: "BEAT-USDT-SWAP",
				SignedContractSize: "-16",
			}}},
		},
		fills: map[string][]portfolio.TradeFill{
			"okx": {{
				OrderID: "venue-b", Quantity: "16", Price: "0.12",
			}},
		},
	}
	auditor := NewArbitragePositionAuditor(
		store, catalog, positionAuditCredentials{}, portfolios,
		"token", time.Minute, time.Second, 20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := auditor.audit(context.Background(), item, nil); err != nil {
		t.Fatal(err)
	}
	if store.uncertain || store.item.LegBBasePosition != "-160" ||
		store.item.LegBPositionDifference != "0" {
		t.Fatalf("item=%+v uncertain=%t", store.item, store.uncertain)
	}
	if len(store.recomputeIDs) != 1 || store.recomputeIDs[0] != "exec-b" {
		t.Fatalf("recomputeIDs=%v", store.recomputeIDs)
	}
}

func TestPositionAuditorUsesDEXExchangeFillReader(t *testing.T) {
	item := ArbitrageCombination{
		ID: "combo-dex", OwnerUsername: "admin", CreatedAt: time.Now().Add(-time.Hour),
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 10, Exchange: "gate",
			ContractType: "perpetual", ExchangeSymbol: "BEAT_USDT", BaseAsset: "BEAT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 20, Exchange: "hyperliquid",
			ContractType: "perpetual", ExchangeSymbol: "BEAT", BaseAsset: "BEAT",
		},
		LegABasePosition: "15", LegBBasePosition: "-15",
	}
	store := &positionAuditStore{
		item: item,
		orders: []Order{
			{
				ID: "a", VenueOrderID: "venue-a", TradingAccountID: 1,
				ArbitrageExecutionID: "exec-a", ArbitrageLeg: "a", Side: "buy",
				Quantity: "20", FilledQuantity: "15", Status: "canceled",
			},
			{
				ID: "b", VenueOrderID: "venue-b", TradingAccountID: 2,
				ArbitrageExecutionID: "exec-b", ArbitrageLeg: "b", Side: "sell",
				Quantity: "20", FilledQuantity: "15", Status: "canceled",
			},
		},
	}
	catalog := positionAuditCatalog{items: map[int64]Instrument{
		10: {
			ID: 10, Exchange: "gate", ContractType: "perpetual",
			ExchangeSymbol: "BEAT_USDT", ContractSize: "1", QuantityStep: "1",
		},
		20: {
			ID: 20, Exchange: "hyperliquid", ContractType: "perpetual",
			ExchangeSymbol: "BEAT", ContractSize: "1", QuantityStep: "1",
		},
	}}
	portfolios := positionAuditPortfolios{
		snapshots: map[string]portfolio.Snapshot{
			"gate": {Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "Gate", WireSymbol: "BEAT_USDT",
				SignedContractSize: "15",
			}}},
			"hyperliquid": {Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "Hyperliquid", WireSymbol: "BEAT",
				BaseAsset: "BEAT", SignedContractSize: "-16",
			}}},
		},
		fills: map[string][]portfolio.TradeFill{"gate": nil},
	}
	reader := &positionAuditDEXFillReader{fills: []exchange.Fill{{
		TradeID: "trade-1", VenueOrderID: "venue-b", Quantity: "16", Price: "100",
	}}}
	auditor := NewArbitragePositionAuditor(
		store, catalog, positionAuditDEXCredentials{}, portfolios,
		"token", time.Minute, time.Second, 20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	auditor.ConfigureDEXFillReaders(exchange.NewTestRegistry(map[string]exchange.Adapter{
		"hyperliquid": reader,
	}))
	if err := auditor.audit(context.Background(), item, nil); err != nil {
		t.Fatal(err)
	}
	if store.uncertain || store.item.LegBBasePosition != "-16" ||
		store.item.LegBPositionDifference != "0" {
		t.Fatalf("item=%+v uncertain=%t", store.item, store.uncertain)
	}
	if len(store.recomputeIDs) != 1 || store.recomputeIDs[0] != "exec-b" {
		t.Fatalf("recomputeIDs=%v", store.recomputeIDs)
	}
	if reader.calls != 1 || reader.instrument.ExchangeSymbol != "BEAT" ||
		reader.credentials.SigningAddress != "signer" ||
		reader.credentials.AccountIndex == nil || *reader.credentials.AccountIndex != 7 {
		t.Fatalf(
			"calls=%d instrument=%+v credentials=%+v",
			reader.calls, reader.instrument, reader.credentials,
		)
	}
}

func TestPositionAuditorClearsAuditRiskBelowMaterialThreshold(t *testing.T) {
	store := &positionAuditStore{item: ArbitrageCombination{
		ID: "combo-recover", OwnerUsername: "admin", Version: 7,
		Status: "running", RuntimeState: "position_uncertain",
		PositionUncertain: true,
		ErrorMessage:      arbitragePositionAuditErrorPrefix + " prior drift",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 10, Exchange: "gate",
			ContractType: "perpetual", ExchangeSymbol: "BEAT_USDT", BaseAsset: "BEAT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 20, Exchange: "okx",
			ContractType: "perpetual", ExchangeSymbol: "BEAT-USDT-SWAP", BaseAsset: "BEAT",
		},
		LegABasePosition: "100", LegBBasePosition: "-100",
	}}
	auditor := NewArbitragePositionAuditor(
		store,
		positionAuditCatalog{items: map[int64]Instrument{
			10: {
				ID: 10, Exchange: "gate", ContractType: "perpetual",
				ExchangeSymbol: "BEAT_USDT", ContractSize: "1", QuantityStep: "1",
				MinQuantity: "10", MinQuantityStatus: exchange.ConstraintKnown,
				MinNotionalStatus: exchange.ConstraintNotApplicable,
			},
			20: {
				ID: 20, Exchange: "okx", ContractType: "perpetual",
				ExchangeSymbol: "BEAT-USDT-SWAP", ContractSize: "1", QuantityStep: "1",
				MinQuantity: "10", MinQuantityStatus: exchange.ConstraintKnown,
				MinNotionalStatus: exchange.ConstraintNotApplicable,
			},
		}},
		positionAuditCredentials{},
		positionAuditPortfolios{snapshots: map[string]portfolio.Snapshot{
			"gate": {Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "Gate", WireSymbol: "BEAT_USDT",
				SignedContractSize: "105",
			}}},
			"okx": {Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "OKX", WireSymbol: "BEAT-USDT-SWAP",
				SignedContractSize: "-100",
			}}},
		}},
		"token", time.Minute, time.Second, 20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := auditor.audit(context.Background(), store.item, nil); err != nil {
		t.Fatal(err)
	}
	if store.item.PositionUncertain {
		t.Fatalf("sub-threshold audit risk not cleared: %+v", store.item)
	}
	if len(store.eventTypes) != 1 || store.eventTypes[0] != "position_uncertain_recovered" {
		t.Fatalf("events=%v", store.eventTypes)
	}
}

func TestPositionAuditorRecoversManualInterventionOnce(t *testing.T) {
	store := &positionAuditStore{item: ArbitrageCombination{
		ID: "combo-manual-recover", OwnerUsername: "admin", Version: 4,
		Status: "running", RuntimeState: "manual_intervention",
		CarryBaseQuantity: "0",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 10, Exchange: "gate",
			ContractType: "perpetual", ExchangeSymbol: "BEAT_USDT", BaseAsset: "BEAT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 20, Exchange: "okx",
			ContractType: "perpetual", ExchangeSymbol: "BEAT-USDT-SWAP", BaseAsset: "BEAT",
		},
		LegABasePosition: "100", LegBBasePosition: "-100",
	}}
	auditor := NewArbitragePositionAuditor(
		store,
		positionAuditCatalog{items: map[int64]Instrument{
			10: {
				ID: 10, Exchange: "gate", ContractType: "perpetual",
				ExchangeSymbol: "BEAT_USDT", ContractSize: "1", QuantityStep: "1",
				MinQuantity: "10", MinQuantityStatus: exchange.ConstraintKnown,
				MinNotionalStatus: exchange.ConstraintNotApplicable,
			},
			20: {
				ID: 20, Exchange: "okx", ContractType: "perpetual",
				ExchangeSymbol: "BEAT-USDT-SWAP", ContractSize: "1", QuantityStep: "1",
				MinQuantity: "10", MinQuantityStatus: exchange.ConstraintKnown,
				MinNotionalStatus: exchange.ConstraintNotApplicable,
			},
		}},
		positionAuditCredentials{},
		positionAuditPortfolios{snapshots: map[string]portfolio.Snapshot{
			"gate": {Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "Gate", WireSymbol: "BEAT_USDT",
				SignedContractSize: "100",
			}}},
			"okx": {Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "OKX", WireSymbol: "BEAT-USDT-SWAP",
				SignedContractSize: "-100",
			}}},
		}},
		"token", time.Minute, time.Second, 20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := auditor.audit(context.Background(), store.item, nil); err != nil {
		t.Fatal(err)
	}
	if store.item.RuntimeState != "monitoring" || store.item.PositionUncertain {
		t.Fatalf("manual intervention not recovered: %+v", store.item)
	}
	if len(store.eventTypes) != 1 || store.eventTypes[0] != "manual_intervention_recovered" {
		t.Fatalf("events=%v", store.eventTypes)
	}
	payload := store.eventPayloads[0]
	if payload["reason"] != "execution_completed_and_positions_matched" ||
		payload["carryBaseQuantity"] != "0" ||
		payload["legAPositionDifference"] != "0" ||
		payload["legBPositionDifference"] != "0" {
		t.Fatalf("payload=%v", payload)
	}
	if err := auditor.audit(context.Background(), store.item, nil); err != nil {
		t.Fatal(err)
	}
	if store.item.RuntimeState != "monitoring" {
		t.Fatalf("second audit runtime=%s", store.item.RuntimeState)
	}
	if len(store.eventTypes) != 1 {
		t.Fatalf("duplicate recovery events=%v", store.eventTypes)
	}
}

func TestPositionAuditorTracksDeferredReasonsWithoutRiskEvent(t *testing.T) {
	tests := []struct {
		name    string
		reason  ArbitragePositionAuditDeferredReason
		version uint64
		active  uint64
	}{
		{name: "version conflict", reason: ArbitragePositionAuditDeferredVersionConflict, version: 1},
		{name: "active execution", reason: ArbitragePositionAuditDeferredActiveExecution, active: 1},
		{name: "active order", reason: ArbitragePositionAuditDeferredActiveOrder, active: 1},
		{name: "reconcile failure", reason: ArbitragePositionAuditDeferredReconcileFailure, active: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &positionAuditStore{
				item: ArbitrageCombination{
					ID: "combo-deferred", OwnerUsername: "admin", Version: 3,
					Status: "running", RuntimeState: "monitoring",
					LegA: ArbitrageLeg{
						TradingAccountID: 1, InstrumentID: 10, Exchange: "gate",
						ContractType: "perpetual", ExchangeSymbol: "BEAT_USDT", BaseAsset: "BEAT",
					},
					LegB: ArbitrageLeg{
						TradingAccountID: 2, InstrumentID: 20, Exchange: "okx",
						ContractType: "perpetual", ExchangeSymbol: "BEAT-USDT-SWAP", BaseAsset: "BEAT",
					},
				},
				updateErr: &ArbitragePositionAuditDeferredError{
					Reason: test.reason, ExecutionID: "execution-1",
				},
			}
			auditor := NewArbitragePositionAuditor(
				store,
				positionAuditCatalog{items: map[int64]Instrument{
					10: {ID: 10, ExchangeSymbol: "BEAT_USDT", ContractSize: "1", QuantityStep: "1"},
					20: {ID: 20, ExchangeSymbol: "BEAT-USDT-SWAP", ContractSize: "1", QuantityStep: "1"},
				}},
				positionAuditCredentials{},
				positionAuditPortfolios{snapshots: map[string]portfolio.Snapshot{
					"gate": {}, "okx": {},
				}},
				"token", time.Minute, time.Second, 20,
				slog.New(slog.NewTextHandler(io.Discard, nil)),
			)
			auditor.runOnce(context.Background())
			stats := auditor.PositionAuditStats()
			if stats.DeferredVersionConflict != test.version ||
				stats.DeferredActiveWork != test.active {
				t.Fatalf("stats=%+v", stats)
			}
			if store.events != 0 || store.item.PositionUncertain {
				t.Fatalf("deferred audit changed risk: item=%+v events=%v", store.item, store.eventTypes)
			}
		})
	}
}

func TestPositionAuditorDoesNotRecomputeWhenTradeFillDoesNotIncrease(t *testing.T) {
	item := ArbitrageCombination{
		ID: "combo-same", OwnerUsername: "admin", CreatedAt: time.Now().Add(-time.Hour),
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 10, Exchange: "gate",
			ContractType: "perpetual", ExchangeSymbol: "BEAT_USDT", BaseAsset: "BEAT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 20, Exchange: "okx",
			ContractType: "perpetual", ExchangeSymbol: "BEAT-USDT-SWAP", BaseAsset: "BEAT",
		},
		LegABasePosition: "15", LegBBasePosition: "-15",
	}
	store := &positionAuditStore{
		item: item,
		orders: []Order{
			{
				ID: "a", VenueOrderID: "venue-a", TradingAccountID: 1,
				ArbitrageExecutionID: "exec-a", ArbitrageLeg: "a", Side: "buy",
				Quantity: "20", FilledQuantity: "15",
			},
			{
				ID: "b", VenueOrderID: "venue-b", TradingAccountID: 2,
				ArbitrageExecutionID: "exec-b", ArbitrageLeg: "b", Side: "sell",
				Quantity: "20", FilledQuantity: "15", Status: "canceled",
			},
		},
	}
	auditor := NewArbitragePositionAuditor(
		store, positionAuditCatalog{items: map[int64]Instrument{
			10: {
				ID: 10, Exchange: "gate", ContractType: "perpetual",
				ExchangeSymbol: "BEAT_USDT", ContractSize: "1", QuantityStep: "1",
			},
			20: {
				ID: 20, Exchange: "okx", ContractType: "perpetual",
				ExchangeSymbol: "BEAT-USDT-SWAP", ContractSize: "1", QuantityStep: "1",
			},
		}}, positionAuditCredentials{},
		positionAuditPortfolios{
			snapshots: map[string]portfolio.Snapshot{
				"gate": {Positions: []portfolio.Position{{
					Kind: "cex", Exchange: "Gate", WireSymbol: "BEAT_USDT",
					SignedContractSize: "15",
				}}},
				"okx": {Positions: []portfolio.Position{{
					Kind: "cex", Exchange: "OKX", WireSymbol: "BEAT-USDT-SWAP",
					SignedContractSize: "-15",
				}}},
			},
			fills: map[string][]portfolio.TradeFill{
				"okx": {{OrderID: "venue-b", Quantity: "15", Price: "0.12"}},
			},
		},
		"token", time.Minute, time.Second, 20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := auditor.audit(context.Background(), item, nil); err != nil {
		t.Fatal(err)
	}
	if len(store.recomputeIDs) != 0 {
		t.Fatalf("recomputeIDs=%v", store.recomputeIDs)
	}
}

func TestVenueMatchesCreationBaselineRequiresExactDifferenceZero(t *testing.T) {
	baseline := decimal.RequireFromString("1.25")
	if !venueMatchesCreationBaseline(decimal.RequireFromString("1.25"), baseline) {
		t.Fatal("matching venue and baseline should pass")
	}
	if venueMatchesCreationBaseline(decimal.Zero, baseline) {
		t.Fatal("absolute zero must not match a non-zero baseline")
	}
	if venueMatchesCreationBaseline(decimal.RequireFromString("1.2501"), baseline) {
		t.Fatal("inexact venue must not match")
	}
}

func TestClosingExternalFlatReadyRequiresClosingUncertainAndTwoTicks(t *testing.T) {
	capturedAt := time.Now().UTC().Add(-time.Hour)
	item := closingExternalFlatCombo(capturedAt)
	if !closingExternalFlatReady(item, decimal.Zero, decimal.Zero) {
		t.Fatal("VVV snapshot should be ready")
	}
	running := item
	running.Status = "running"
	running.RuntimeState = "monitoring"
	if closingExternalFlatReady(running, decimal.Zero, decimal.Zero) {
		t.Fatal("running combinations must not trigger")
	}
	firstTick := item
	firstTick.LegAVenueBasePosition = "0.5"
	firstTick.LegBVenueBasePosition = "-0.57"
	if closingExternalFlatReady(firstTick, decimal.Zero, decimal.Zero) {
		t.Fatal("first confirmation must wait for last venue to match baseline")
	}
	oneLeg := item
	if closingExternalFlatReady(oneLeg, decimal.Zero, decimal.RequireFromString("0.1")) {
		t.Fatal("one-leg return to baseline must not trigger")
	}
}

func TestPositionAuditorAppliesClosingExternalFlatWithoutAuditUpdate(t *testing.T) {
	capturedAt := time.Now().UTC().Add(-time.Hour)
	store := &positionAuditStore{item: closingExternalFlatCombo(capturedAt)}
	auditor := newClosingExternalFlatAuditor(t, store, map[string]portfolio.Snapshot{
		"gate": {},
		"okx":  {},
	})
	if err := auditor.audit(context.Background(), store.item, nil); err != nil {
		t.Fatal(err)
	}
	if store.applyCalls != 1 || store.auditUpdates != 0 {
		t.Fatalf("applyCalls=%d auditUpdates=%d", store.applyCalls, store.auditUpdates)
	}
	if store.item.Status != "closing" || store.item.PositionUncertain ||
		store.item.RuntimeState != "closing" ||
		store.item.LegABasePosition != "0" || store.item.LegBBasePosition != "0" {
		t.Fatalf("applied=%+v", store.item)
	}
	if parseDecimal(store.item.LegAReconciliationAdjustment).Cmp(decimal.RequireFromString("-0.5")) != 0 ||
		parseDecimal(store.item.LegBReconciliationAdjustment).Cmp(decimal.RequireFromString("0.57")) != 0 {
		t.Fatalf("adjustments a=%s b=%s",
			store.item.LegAReconciliationAdjustment, store.item.LegBReconciliationAdjustment)
	}
	if len(store.orders) != 0 {
		t.Fatalf("created orders=%v", store.orders)
	}
}

func TestPositionAuditorAppliesClosingExternalFlatWithNonZeroBaseline(t *testing.T) {
	capturedAt := time.Now().UTC().Add(-time.Hour)
	item := closingExternalFlatCombo(capturedAt)
	item.LegAVenueBaselineBasePosition = "10"
	item.LegBVenueBaselineBasePosition = "-10"
	item.LegAVenueBasePosition = "10"
	item.LegBVenueBasePosition = "-10"
	item.LegABasePosition = "0.5"
	item.LegBBasePosition = "-0.57"
	store := &positionAuditStore{item: item}
	auditor := newClosingExternalFlatAuditor(t, store, map[string]portfolio.Snapshot{
		"gate": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "Gate", WireSymbol: "BEAT_USDT",
			SignedContractSize: "10",
		}}},
		"okx": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "OKX", WireSymbol: "BEAT-USDT-SWAP",
			SignedContractSize: "-10",
		}}},
	})
	if err := auditor.audit(context.Background(), store.item, nil); err != nil {
		t.Fatal(err)
	}
	if store.applyCalls != 1 || store.auditUpdates != 0 {
		t.Fatalf("applyCalls=%d auditUpdates=%d", store.applyCalls, store.auditUpdates)
	}
	if store.item.LegABasePosition != "0" || store.item.LegBBasePosition != "0" ||
		store.item.LegAVenueBaselineBasePosition != "10" ||
		store.item.LegBVenueBaselineBasePosition != "-10" {
		t.Fatalf("baseline mutated: %+v", store.item)
	}
}

func TestPositionAuditorClosingExternalFlatFirstTickUsesNormalAudit(t *testing.T) {
	capturedAt := time.Now().UTC().Add(-time.Hour)
	item := closingExternalFlatCombo(capturedAt)
	item.LegAVenueBasePosition = "0.5"
	item.LegBVenueBasePosition = "-0.57"
	store := &positionAuditStore{item: item}
	auditor := newClosingExternalFlatAuditor(t, store, map[string]portfolio.Snapshot{
		"gate": {},
		"okx":  {},
	})
	if err := auditor.audit(context.Background(), store.item, nil); err != nil {
		t.Fatal(err)
	}
	if store.applyCalls != 0 || store.auditUpdates != 1 {
		t.Fatalf("applyCalls=%d auditUpdates=%d", store.applyCalls, store.auditUpdates)
	}
	if !store.item.PositionUncertain || store.item.Status != "closing" {
		t.Fatalf("first tick=%+v", store.item)
	}
}

func TestPositionAuditorClosingExternalFlatOneLegDoesNotApply(t *testing.T) {
	capturedAt := time.Now().UTC().Add(-time.Hour)
	store := &positionAuditStore{item: closingExternalFlatCombo(capturedAt)}
	auditor := newClosingExternalFlatAuditor(t, store, map[string]portfolio.Snapshot{
		"gate": {},
		"okx": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "OKX", WireSymbol: "BEAT-USDT-SWAP",
			SignedContractSize: "-1",
		}}},
	})
	if err := auditor.audit(context.Background(), store.item, nil); err != nil {
		t.Fatal(err)
	}
	if store.applyCalls != 0 || store.auditUpdates != 1 {
		t.Fatalf("applyCalls=%d auditUpdates=%d", store.applyCalls, store.auditUpdates)
	}
}

func TestPositionAuditorClosingExternalFlatRejectedLeavesRowUnchanged(t *testing.T) {
	capturedAt := time.Now().UTC().Add(-time.Hour)
	store := &positionAuditStore{
		item:          closingExternalFlatCombo(capturedAt),
		applyRejected: true,
	}
	before := store.item
	auditor := newClosingExternalFlatAuditor(t, store, map[string]portfolio.Snapshot{
		"gate": {},
		"okx":  {},
	})
	if err := auditor.audit(context.Background(), store.item, nil); err != nil {
		t.Fatal(err)
	}
	if store.applyCalls != 1 || store.auditUpdates != 0 {
		t.Fatalf("applyCalls=%d auditUpdates=%d", store.applyCalls, store.auditUpdates)
	}
	if store.item.LegABasePosition != before.LegABasePosition ||
		store.item.Version != before.Version || !store.item.PositionUncertain {
		t.Fatalf("rejected apply mutated item=%+v", store.item)
	}
}

func TestPositionAuditorRunningMonitoringSkipsClosingExternalFlat(t *testing.T) {
	capturedAt := time.Now().UTC().Add(-time.Hour)
	item := closingExternalFlatCombo(capturedAt)
	item.Status = "running"
	item.RuntimeState = "monitoring"
	item.PositionUncertain = false
	item.ErrorMessage = ""
	item.LegABasePosition = "0"
	item.LegBBasePosition = "0"
	store := &positionAuditStore{item: item}
	auditor := newClosingExternalFlatAuditor(t, store, map[string]portfolio.Snapshot{
		"gate": {},
		"okx":  {},
	})
	if err := auditor.audit(context.Background(), store.item, nil); err != nil {
		t.Fatal(err)
	}
	if store.applyCalls != 0 || store.auditUpdates != 1 {
		t.Fatalf("applyCalls=%d auditUpdates=%d", store.applyCalls, store.auditUpdates)
	}
}

func closingExternalFlatCombo(capturedAt time.Time) ArbitrageCombination {
	return ArbitrageCombination{
		ID: "combo-vvv", OwnerUsername: "admin", Version: 3,
		Status: "closing", RuntimeState: "position_uncertain",
		PositionUncertain: true,
		ErrorMessage:      arbitragePositionAuditErrorPrefix + " residual",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 10, Exchange: "gate",
			ContractType: "perpetual", ExchangeSymbol: "BEAT_USDT",
			BaseAsset: "BEAT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 20, Exchange: "okx",
			ContractType: "perpetual", ExchangeSymbol: "BEAT-USDT-SWAP",
			BaseAsset: "BEAT",
		},
		LegABasePosition:              "0.5",
		LegBBasePosition:              "-0.57",
		LegAVenueBaselineBasePosition: "0",
		LegBVenueBaselineBasePosition: "0",
		VenueBaselineCapturedAt:       capturedAt,
		LegAVenueBasePosition:         "0",
		LegBVenueBasePosition:         "0",
	}
}

func newClosingExternalFlatAuditor(
	t *testing.T,
	store *positionAuditStore,
	snapshots map[string]portfolio.Snapshot,
) *ArbitragePositionAuditor {
	t.Helper()
	return NewArbitragePositionAuditor(
		store, positionAuditCatalog{items: map[int64]Instrument{
			10: {
				ID: 10, Exchange: "gate", ContractType: "perpetual",
				ExchangeSymbol: "BEAT_USDT", ContractSize: "1", QuantityStep: "0.001",
			},
			20: {
				ID: 20, Exchange: "okx", ContractType: "perpetual",
				ExchangeSymbol: "BEAT-USDT-SWAP", ContractSize: "1", QuantityStep: "0.001",
			},
		}}, positionAuditCredentials{},
		positionAuditPortfolios{snapshots: snapshots},
		"token", time.Minute, time.Second, 20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

func TestPositionAuditorSharesAccountSnapshotWithinRound(t *testing.T) {
	combo := ArbitrageCombination{
		ID: "combo-1", OwnerUsername: "admin",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 10, Exchange: "gate",
			ContractType: "perpetual", ExchangeSymbol: "BEAT_USDT",
			BaseAsset: "BEAT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 20, Exchange: "okx",
			ContractType: "perpetual", ExchangeSymbol: "BEAT-USDT-SWAP",
			BaseAsset: "BEAT",
		},
		LegABasePosition: "150", LegBBasePosition: "-160",
	}
	combo2 := combo
	combo2.ID = "combo-2"
	store := &positionAuditStore{items: []ArbitrageCombination{combo, combo2}}
	portfolios := &countingAuditPortfolios{snapshots: map[string]portfolio.Snapshot{
		"gate": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "Gate", WireSymbol: "BEAT_USDT",
			SignedContractSize: "15",
		}}},
		"okx": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "OKX", WireSymbol: "BEAT-USDT-SWAP",
			SignedContractSize: "-16",
		}}},
	}}
	auditor := NewArbitragePositionAuditor(
		store, positionAuditCatalog{items: map[int64]Instrument{
			10: {
				ID: 10, Exchange: "gate", ContractType: "perpetual",
				ExchangeSymbol: "BEAT_USDT", ContractSize: "10", QuantityStep: "10",
			},
			20: {
				ID: 20, Exchange: "okx", ContractType: "perpetual",
				ExchangeSymbol: "BEAT-USDT-SWAP", ContractSize: "10", QuantityStep: "10",
			},
		}},
		positionAuditCredentials{}, portfolios, "token", time.Minute, time.Second, 20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	auditor.runOnce(context.Background())
	if portfolios.calls != 2 {
		t.Fatalf("snapshot calls=%d want 2", portfolios.calls)
	}
}

func TestPositionAuditorCachesSnapshotFailureWithinRound(t *testing.T) {
	combo := ArbitrageCombination{
		ID: "combo-1", OwnerUsername: "admin",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 10, Exchange: "gate",
			ContractType: "perpetual", ExchangeSymbol: "BEAT_USDT",
			BaseAsset: "BEAT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 10, Exchange: "gate",
			ContractType: "perpetual", ExchangeSymbol: "BEAT_USDT",
			BaseAsset: "BEAT",
		},
	}
	combo2 := combo
	combo2.ID = "combo-2"
	store := &positionAuditStore{items: []ArbitrageCombination{combo, combo2}}
	portfolios := &countingAuditPortfolios{err: errors.New("snapshot failed")}
	auditor := NewArbitragePositionAuditor(
		store, positionAuditCatalog{items: map[int64]Instrument{
			10: {
				ID: 10, Exchange: "gate", ContractType: "perpetual",
				ExchangeSymbol: "BEAT_USDT", ContractSize: "10", QuantityStep: "10",
			},
		}},
		positionAuditCredentials{}, portfolios, "token", time.Minute, time.Second, 20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	auditor.runOnce(context.Background())
	if portfolios.calls != 1 {
		t.Fatalf("failed snapshot calls=%d want 1", portfolios.calls)
	}
}

type countingAuditPortfolios struct {
	snapshots map[string]portfolio.Snapshot
	err       error
	calls     int
}

func (p *countingAuditPortfolios) Snapshot(
	_ context.Context, exchange string, _ portfolio.Credentials,
) (portfolio.Snapshot, error) {
	p.calls++
	if p.err != nil {
		return portfolio.Snapshot{}, p.err
	}
	return p.snapshots[exchange], nil
}

func (p *countingAuditPortfolios) TradeFills(
	context.Context, string, portfolio.Credentials, portfolio.TradeQuery,
) ([]portfolio.TradeFill, error) {
	return nil, nil
}
