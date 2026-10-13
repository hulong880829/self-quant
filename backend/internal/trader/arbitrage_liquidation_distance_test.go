package trader

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/account/portfolio"
)

func TestLiquidationDistancePct(t *testing.T) {
	perpetual := ArbitrageLeg{
		Exchange: "binance", ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC",
	}
	position := func(signed, side, mark, liq, symbol string) portfolio.Position {
		if symbol == "" {
			symbol = "BTCUSDT"
		}
		return portfolio.Position{
			Kind: "cex", Exchange: "Binance", Symbol: symbol, WireSymbol: symbol,
			BaseAsset: "BTC", Side: side, SignedContractSize: signed,
			MarkPrice: mark, LiquidationPrice: liq,
		}
	}
	snapshot := func(items ...portfolio.Position) portfolio.Snapshot {
		return portfolio.Snapshot{Positions: items}
	}
	assertDistance := func(t *testing.T, got, want string) {
		t.Helper()
		if want == "" {
			if got != "" {
				t.Fatalf("distance=%q want empty", got)
			}
			return
		}
		if !parseDecimal(got).Equal(decimal.RequireFromString(want)) {
			t.Fatalf("distance=%q want=%s", got, want)
		}
	}

	assertDistance(t, liquidationDistancePct(
		snapshot(position("2", "long", "100", "81.58", "")), perpetual, "1",
	), "18.42")
	assertDistance(t, liquidationDistancePct(
		snapshot(position("-2", "short", "100", "121.07", "")), perpetual, "-1",
	), "21.07")
	assertDistance(t, liquidationDistancePct(
		snapshot(position("2", "long", "100", "100", "")), perpetual, "1",
	), "0")
	assertDistance(t, liquidationDistancePct(
		snapshot(position("2", "long", "100", "110", "")), perpetual, "1",
	), "-10")
	assertDistance(t, liquidationDistancePct(
		snapshot(position("2", "", "100", "80", "")), perpetual, "1",
	), "20")

	for _, test := range []struct {
		name     string
		leg      ArbitrageLeg
		ledger   string
		snapshot portfolio.Snapshot
	}{
		{name: "spot", leg: ArbitrageLeg{Exchange: "binance", ContractType: "spot", ExchangeSymbol: "BTCUSDT"}, ledger: "1", snapshot: snapshot(position("2", "long", "100", "80", ""))},
		{name: "delivery", leg: ArbitrageLeg{Exchange: "binance", ContractType: "delivery", ExchangeSymbol: "BTCUSDT"}, ledger: "1", snapshot: snapshot(position("2", "long", "100", "80", ""))},
		{name: "blank contract", leg: ArbitrageLeg{Exchange: "binance", ExchangeSymbol: "BTCUSDT"}, ledger: "1", snapshot: snapshot(position("2", "long", "100", "80", ""))},
		{name: "flat ledger", leg: perpetual, ledger: "0", snapshot: snapshot(position("2", "long", "100", "80", ""))},
		{name: "no position", leg: perpetual, ledger: "1", snapshot: snapshot()},
		{name: "bad signed size", leg: perpetual, ledger: "1", snapshot: snapshot(position("bad", "long", "100", "80", ""))},
		{name: "zero signed size", leg: perpetual, ledger: "1", snapshot: snapshot(position("0", "long", "100", "80", ""))},
		{name: "side conflict", leg: perpetual, ledger: "1", snapshot: snapshot(position("-2", "long", "100", "80", ""))},
		{name: "zero mark", leg: perpetual, ledger: "1", snapshot: snapshot(position("2", "long", "0", "80", ""))},
		{name: "blank liquidation", leg: perpetual, ledger: "1", snapshot: snapshot(position("2", "long", "100", "", ""))},
		{name: "invalid liquidation", leg: perpetual, ledger: "1", snapshot: snapshot(position("2", "long", "100", "bad", ""))},
		{
			name: "ambiguous same side", leg: perpetual, ledger: "1",
			snapshot: snapshot(
				position("2", "long", "100", "80", ""),
				position("3", "long", "100", "70", ""),
			),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertDistance(t, liquidationDistancePct(test.snapshot, test.leg, test.ledger), "")
		})
	}

	hedge := liquidationDistancePct(snapshot(
		position("2", "long", "100", "80", ""),
		position("-4", "short", "100", "130", ""),
	), perpetual, "1")
	assertDistance(t, hedge, "20")
}

func TestPositionAuditSnapshotFailureKeepsLiquidationDistance(t *testing.T) {
	store := &positionAuditStore{item: ArbitrageCombination{
		ID: "combo-1", OwnerUsername: "admin",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 10, Exchange: "gate",
			ContractType: "perpetual", ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 20, Exchange: "okx",
			ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP", BaseAsset: "BTC",
		},
		LegABasePosition: "1", LegBBasePosition: "-1",
		LegALiquidationDistancePct: "18.42",
		LegBLiquidationDistancePct: "21.07",
	}}
	auditor := NewArbitragePositionAuditor(
		store, positionAuditCatalog{items: map[int64]Instrument{
			10: {ID: 10, Exchange: "gate", ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
			20: {ID: 20, Exchange: "okx", ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP"},
		}},
		positionAuditCredentials{}, failingAuditPortfolios{},
		"token", time.Minute, time.Second, 20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := auditor.audit(context.Background(), store.item, nil); err == nil {
		t.Fatal("expected snapshot error")
	}
	if store.auditUpdates != 0 ||
		store.item.LegALiquidationDistancePct != "18.42" ||
		store.item.LegBLiquidationDistancePct != "21.07" {
		t.Fatalf("distance overwritten updates=%d item=%+v", store.auditUpdates, store.item)
	}
	if len(store.exitMarks) != 0 {
		t.Fatalf("snapshot failure triggered exit marks=%v", store.exitMarks)
	}
}

func TestNormalizeEarlyExitLiquidationDistancePct(t *testing.T) {
	base := CreateArbitrageInput{
		Token: "token", IdempotencyKey: "request-123",
		LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		TargetNotional:   "1000",
		ExecutionMode:    "maker_then_hedge",
		RunMode:          "one_shot",
		ExitPolicy:       "time",
		ExitAfterSeconds: 86400,
	}
	empty, err := normalizeArbitrageInput(base)
	if err != nil {
		t.Fatal(err)
	}
	if empty.EarlyExitLiquidationDistancePct != "" {
		t.Fatalf("empty distance=%q", empty.EarlyExitLiquidationDistancePct)
	}
	for _, tc := range []struct{ raw, want string }{
		{raw: "20", want: "20"},
		{raw: "12.5", want: "12.5"},
		{raw: " 20.0 ", want: "20"},
	} {
		input := base
		input.EarlyExitLiquidationDistancePct = tc.raw
		got, normErr := normalizeArbitrageInput(input)
		if normErr != nil {
			t.Fatalf("raw=%q err=%v", tc.raw, normErr)
		}
		if got.EarlyExitLiquidationDistancePct != tc.want {
			t.Fatalf("raw=%q got=%q want=%q", tc.raw, got.EarlyExitLiquidationDistancePct, tc.want)
		}
	}
	for _, raw := range []string{"0", "-1", "100", "20%", "abc", "NaN", "Inf", "+Infinity", "-inf"} {
		input := base
		input.EarlyExitLiquidationDistancePct = raw
		if _, normErr := normalizeArbitrageInput(input); !errors.Is(normErr, ErrInvalidArgument) {
			t.Fatalf("raw=%q err=%v", raw, normErr)
		}
	}
	spread := base
	spread.RunMode = "spread"
	spread.AskThresholdBps = "12"
	spread.BidThresholdBps = "-8"
	spread.EarlyExitLiquidationDistancePct = "20"
	normalizedSpread, err := normalizeArbitrageInput(spread)
	if err != nil {
		t.Fatal(err)
	}
	if normalizedSpread.EarlyExitLiquidationDistancePct != "" {
		t.Fatalf("spread distance=%q", normalizedSpread.EarlyExitLiquidationDistancePct)
	}
	cleared := ""
	if _, err := normalizeUpdateArbitrageInput(UpdateArbitrageInput{
		Token: "token", CombinationID: "11111111-1111-1111-1111-111111111111",
		EarlyExitLiquidationDistancePct: &cleared,
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("clear err=%v", err)
	}
}

func TestArbitrageFingerprintIncludesLiquidationDistance(t *testing.T) {
	base := ArbitrageCombination{
		LegA:            ArbitrageLeg{TradingAccountID: 1, InstrumentID: 11},
		LegB:            ArbitrageLeg{TradingAccountID: 2, InstrumentID: 22},
		AskThresholdBps: "0", BidThresholdBps: "0",
		TargetNotional: "1000", ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		RunMode: "one_shot", EntryDirection: "ask",
		ExitPolicy: "time", ExitAfterSeconds: 3600,
	}
	withDistance := base
	withDistance.EarlyExitLiquidationDistancePct = "20"
	if arbitrageFingerprint(base) == arbitrageFingerprint(withDistance) {
		t.Fatal("threshold must change fingerprint")
	}
	if sameArbitrageRequest(base, withDistance) {
		t.Fatal("different thresholds must not be the same request")
	}
	if !sameArbitrageRequest(withDistance, withDistance) {
		t.Fatal("identical thresholds must match")
	}
}

func TestMaybeTriggerLiquidationDistanceExit(t *testing.T) {
	base := ArbitrageCombination{
		ID: "combo-1", Version: 4, RunMode: "one_shot", OneShotPhase: "waiting_exit",
		EarlyExitLiquidationDistancePct: "20",
		LegALiquidationDistancePct:      "18.6",
		LegBLiquidationDistancePct:      "35.2",
		RuntimeState:                    "monitoring",
	}
	store := &positionAuditStore{item: base, exitApplied: true}
	signal := &recordingExitSignal{}
	auditor := NewArbitragePositionAuditor(
		store, positionAuditCatalog{}, positionAuditCredentials{}, failingAuditPortfolios{},
		"token", time.Minute, time.Second, 20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	auditor.ConfigureExitSignal(signal)
	auditor.maybeTriggerLiquidationDistanceExit(context.Background(), base)
	if len(store.exitMarks) != 1 || store.exitMarks[0].version != 4 ||
		store.exitMarks[0].reason != "liquidation_distance_threshold" ||
		store.exitMarks[0].details["triggeredLeg"] != "a" {
		t.Fatalf("marks=%+v", store.exitMarks)
	}
	if signal.calls != 1 || signal.version != 5 || signal.reason != "liquidation_distance_threshold" {
		t.Fatalf("signal=%+v", signal)
	}

	both := base
	both.LegBLiquidationDistancePct = "20"
	store = &positionAuditStore{item: both, exitApplied: true}
	auditor.store = store
	signal.calls = 0
	auditor.maybeTriggerLiquidationDistanceExit(context.Background(), both)
	if store.exitMarks[0].details["triggeredLeg"] != "both" {
		t.Fatalf("triggeredLeg=%v", store.exitMarks[0].details["triggeredLeg"])
	}

	miss := base
	miss.LegALiquidationDistancePct = "20.1"
	miss.LegBLiquidationDistancePct = ""
	store = &positionAuditStore{item: miss, exitApplied: true}
	auditor.store = store
	auditor.maybeTriggerLiquidationDistanceExit(context.Background(), miss)
	if len(store.exitMarks) != 0 {
		t.Fatalf("miss should not mark: %+v", store.exitMarks)
	}

	negative := base
	negative.LegALiquidationDistancePct = "-1"
	negative.LegBLiquidationDistancePct = ""
	store = &positionAuditStore{item: negative, exitApplied: true}
	auditor.store = store
	auditor.maybeTriggerLiquidationDistanceExit(context.Background(), negative)
	if len(store.exitMarks) != 1 || store.exitMarks[0].details["triggeredLeg"] != "a" {
		t.Fatalf("negative marks=%+v", store.exitMarks)
	}

	for _, blocked := range []ArbitrageCombination{
		func() ArbitrageCombination { item := base; item.OneShotPhase = "exiting"; return item }(),
		func() ArbitrageCombination { item := base; item.PositionUncertain = true; return item }(),
		func() ArbitrageCombination {
			item := base
			item.RuntimeState = "manual_intervention"
			return item
		}(),
		func() ArbitrageCombination { item := base; item.RunMode = "spread"; return item }(),
		func() ArbitrageCombination {
			item := base
			item.EarlyExitLiquidationDistancePct = ""
			return item
		}(),
	} {
		store = &positionAuditStore{item: blocked, exitApplied: true}
		auditor.store = store
		auditor.maybeTriggerLiquidationDistanceExit(context.Background(), blocked)
		if len(store.exitMarks) != 0 {
			t.Fatalf("blocked item marked: %+v marks=%+v", blocked, store.exitMarks)
		}
	}
}

func TestPositionAuditTriggersLiquidationDistanceWithUpdatedVersion(t *testing.T) {
	store := &positionAuditStore{
		exitApplied: true,
		item: ArbitrageCombination{
			ID: "combo-1", OwnerUsername: "admin", Version: 7,
			Status: "running", RuntimeState: "monitoring",
			RunMode: "one_shot", OneShotPhase: "building_target",
			EarlyExitLiquidationDistancePct: "20",
			LegA: ArbitrageLeg{
				TradingAccountID: 1, InstrumentID: 10, Exchange: "gate",
				ContractType: "perpetual", ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC",
			},
			LegB: ArbitrageLeg{
				TradingAccountID: 2, InstrumentID: 20, Exchange: "okx",
				ContractType: "spot", ExchangeSymbol: "BTC-USDT", BaseAsset: "BTC",
			},
			LegABasePosition: "1", LegBBasePosition: "1",
		},
	}
	auditor := NewArbitragePositionAuditor(
		store, positionAuditCatalog{items: map[int64]Instrument{
			10: {
				ID: 10, Exchange: "gate", ContractType: "perpetual",
				ExchangeSymbol: "BTCUSDT", ContractSize: "1", QuantityStep: "0.001",
			},
			20: {
				ID: 20, Exchange: "okx", ContractType: "spot",
				ExchangeSymbol: "BTC-USDT", ContractSize: "1", QuantityStep: "0.001",
			},
		}},
		positionAuditCredentials{},
		positionAuditPortfolios{snapshots: map[string]portfolio.Snapshot{
			"gate": {Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "Gate", Symbol: "BTCUSDT", WireSymbol: "BTCUSDT",
				BaseAsset: "BTC", Side: "long", SignedContractSize: "1",
				MarkPrice: "100", LiquidationPrice: "80",
			}}},
			"okx": {SpotBalances: map[string]string{"BTC": "1"}},
		}},
		"token", time.Minute, time.Second, 20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	signal := &recordingExitSignal{}
	auditor.ConfigureExitSignal(signal)
	if err := auditor.audit(context.Background(), store.item, nil); err != nil {
		t.Fatal(err)
	}
	if len(store.exitMarks) != 1 || store.exitMarks[0].version != 8 {
		t.Fatalf("marks=%+v itemVersion=%d", store.exitMarks, store.item.Version)
	}
	if signal.calls != 1 || signal.version != 9 || signal.id != "combo-1" {
		t.Fatalf("signal=%+v", signal)
	}
	if store.exitMarks[0].details["triggeredLeg"] != "a" {
		t.Fatalf("details=%v", store.exitMarks[0].details)
	}
}

type failingAuditPortfolios struct{}

func (failingAuditPortfolios) Snapshot(
	context.Context, string, portfolio.Credentials,
) (portfolio.Snapshot, error) {
	return portfolio.Snapshot{}, errors.New("snapshot down")
}

func (failingAuditPortfolios) TradeFills(
	context.Context, string, portfolio.Credentials, portfolio.TradeQuery,
) ([]portfolio.TradeFill, error) {
	return nil, nil
}
