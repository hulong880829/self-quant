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
	"selfquant/backend/internal/trader/exchange"
)

func TestIncreaseTargetUsesActualGapNotTargetDelta(t *testing.T) {
	extra := legAdditionalNotional(decimal.RequireFromString("1500"), "500")
	if !extra.Equal(decimal.RequireFromString("1000")) {
		t.Fatalf("additional=%s", extra)
	}
	if got := legAdditionalNotional(decimal.RequireFromString("800"), "1000"); !got.IsZero() {
		t.Fatalf("reduction additional=%s", got)
	}
	combo := ArbitrageCombination{
		Status: "running", RunMode: "spread", PositionNotional: "400",
		PositionUncertain: true, RuntimeState: "manual_intervention",
	}
	if err := increaseTargetGate(combo, decimal.RequireFromString("1500"), true); err != nil {
		t.Fatalf("spread gate rejected a PATCH-legal target: %v", err)
	}
	combo.RunMode = "one_shot"
	combo.OneShotPhase = "building_target"
	combo.PositionUncertain = false
	combo.RuntimeState = "running"
	combo.TargetNotional = "1000"
	if err := increaseTargetGate(combo, decimal.RequireFromString("1500"), true); err == nil {
		t.Fatal("one-shot live work must be rejected")
	}
}

func TestIncreasePreflightRejectsHighLeverageBeforeVenue(t *testing.T) {
	const id = "7f928919-1f36-45e9-9e78-7834465b220b"
	instrumentA := readyArbitrageInstrument(11, "aster", "perpetual", "BEATUSDT")
	instrumentB := readyArbitrageInstrument(22, "okx", "perpetual", "BEAT-USDT-SWAP")
	store := &arbitrageCaptureStore{item: ArbitrageCombination{
		ID: id, Status: "running", RunMode: "spread", TargetNotional: "1000",
		PositionNotional: "500", PositionUncertain: true, RuntimeState: "manual_intervention",
		LegALeverage: "11", LegBLeverage: "4",
		LegA: ArbitrageLeg{TradingAccountID: 1, InstrumentID: 11, Exchange: "aster"},
		LegB: ArbitrageLeg{TradingAccountID: 2, InstrumentID: 22, Exchange: "okx"},
	}}
	aster := &preflightPreviewAdapter{stubAdapter: &stubAdapter{positionMode: exchange.PositionModeOneWay}}
	okx := &preflightPreviewAdapter{stubAdapter: &stubAdapter{positionMode: exchange.PositionModeOneWay}}
	snapshots := &arbitrageBaselineSnapshots{calls: map[string]int{}, snapshots: map[string]portfolio.Snapshot{}}
	service := newIncreasePreflightService(store, snapshots, instrumentA, instrumentB, aster, okx)
	result, err := service.PreflightIncreaseTarget(context.Background(), "token", id, "1500")
	if err != nil {
		t.Fatal(err)
	}
	if result.Code != "invalid_leverage" || result.Leg != "a" || result.Executable {
		t.Fatalf("result=%+v", result)
	}
	if snapshots.calls["aster"] != 0 || snapshots.calls["okx"] != 0 || aster.leverageSets != 0 || okx.leverageSets != 0 {
		t.Fatalf("venue calls snapshots=%v aster=%d okx=%d", snapshots.calls, aster.leverageSets, okx.leverageSets)
	}
}

func TestIncreasePreflightChecksEachLegGap(t *testing.T) {
	const id = "7f928919-1f36-45e9-9e78-7834465b220b"
	instrumentA := readyArbitrageInstrument(11, "aster", "perpetual", "BEATUSDT")
	instrumentB := readyArbitrageInstrument(22, "okx", "perpetual", "BEAT-USDT-SWAP")
	store := &arbitrageCaptureStore{item: ArbitrageCombination{
		ID: id, Status: "running", RunMode: "spread", TargetNotional: "1000",
		PositionNotional: "500", LegALeverage: "4", LegBLeverage: "4",
		LegAVenueNotional: "500", LegBVenueNotional: "500",
		ExecutionMode: "simultaneous_market", MakerLeg: "a",
		LegA: ArbitrageLeg{TradingAccountID: 1, InstrumentID: 11, Exchange: "aster"},
		LegB: ArbitrageLeg{TradingAccountID: 2, InstrumentID: 22, Exchange: "okx"},
	}}
	aster := &preflightPreviewAdapter{
		stubAdapter: &stubAdapter{positionMode: exchange.PositionModeOneWay},
		preview: exchange.LeverageSetPreview{
			EstMaxOpen: "100000", EstMaxOpenUnit: exchange.EstMaxOpenUnitQuoteNotional,
		},
	}
	okx := &preflightPreviewAdapter{
		stubAdapter: &stubAdapter{positionMode: exchange.PositionModeOneWay},
		preview: exchange.LeverageSetPreview{
			EstMaxOpen: "100000", EstMaxOpenUnit: exchange.EstMaxOpenUnitQuoteNotional,
		},
	}
	snapshots := &arbitrageBaselineSnapshots{
		calls: map[string]int{},
		snapshots: map[string]portfolio.Snapshot{
			"aster": {AvailableFundsUSD: "200"},
			"okx":   {AvailableFundsUSD: "5000"},
		},
	}
	service := newIncreasePreflightService(store, snapshots, instrumentA, instrumentB, aster, okx)
	result, err := service.PreflightIncreaseTarget(context.Background(), "token", id, "1500")
	if err != nil {
		t.Fatal(err)
	}
	if result.RequestedTargetDelta != "500" || result.LegA.AdditionalNotional != "1000" ||
		result.Code != "insufficient_margin" || result.Executable {
		t.Fatalf("result=%+v", result)
	}
	if aster.leverageSets != 0 || okx.leverageSets != 0 {
		t.Fatal("preflight set leverage")
	}
}

func TestIncreasePreflightUnitsAndFailureStatuses(t *testing.T) {
	const id = "7f928919-1f36-45e9-9e78-7834465b220b"
	service := newIncreasePreflightService(
		&arbitrageCaptureStore{}, nil,
		readyArbitrageInstrument(11, "aster", "perpetual", "BEATUSDT"),
		readyArbitrageInstrument(22, "okx", "spot", "BEAT-USDT"),
		&stubAdapter{positionMode: exchange.PositionModeOneWay},
		&stubAdapter{positionMode: exchange.PositionModeOneWay},
	)
	invalid, err := service.PreflightIncreaseTarget(context.Background(), "token", id, "0")
	if err != nil || invalid.Code != "invalid_target" || invalid.TargetNotionalUnit != preflightUSDEquivalent {
		t.Fatalf("invalid=%+v err=%v", invalid, err)
	}
	if invalid.LegA.IncrementalQuantityUnit != "" || invalid.LegA.MinQuantity.Unit != "" ||
		invalid.LegA.MinNotional.Unit != "" {
		t.Fatalf("invalid target leaked instrument units: %+v", invalid.LegA)
	}

	missingCatalog := newIncreasePreflightService(
		&arbitrageCaptureStore{item: runningIncreaseCombo(id)},
		fundedIncreaseSnapshots(),
		readyArbitrageInstrument(11, "aster", "perpetual", "BEATUSDT"),
		readyArbitrageInstrument(22, "okx", "spot", "BEAT-USDT"),
		&stubAdapter{positionMode: exchange.PositionModeOneWay},
		&stubAdapter{positionMode: exchange.PositionModeOneWay},
	)
	missingCatalog.catalog = arbitrageServiceCatalog{items: map[int64]Instrument{}}
	missing, err := missingCatalog.PreflightIncreaseTarget(context.Background(), "token", id, "1200")
	if !errors.Is(err, ErrInstrumentUnavailable) || missing.TargetNotionalUnit != preflightUSDEquivalent {
		t.Fatalf("missing=%+v err=%v", missing, err)
	}
	if missing.LegA.IncrementalQuantityUnit != "" || missing.LegB.MinQuantity.Unit != "" {
		t.Fatalf("missing instrument leaked units: %+v %+v", missing.LegA, missing.LegB)
	}

	unsupported := newIncreasePreflightService(
		&arbitrageCaptureStore{item: runningIncreaseCombo(id)},
		fundedIncreaseSnapshots(),
		readyArbitrageInstrument(11, "aster", "perpetual", "BEATUSDT"),
		readyArbitrageInstrument(22, "okx", "spot", "BEAT-USDT"),
		&stubAdapter{positionMode: exchange.PositionModeOneWay},
		&stubAdapter{positionMode: exchange.PositionModeOneWay},
	)
	unsupported.ConfigureArbitrageExchanges(map[string]bool{"okx": true})
	rejected, err := unsupported.PreflightIncreaseTarget(context.Background(), "token", id, "1200")
	if err != nil || rejected.Code != "unsupported_exchange" {
		t.Fatalf("unsupported=%+v err=%v", rejected, err)
	}
	if rejected.LegA.IncrementalQuantityUnit != "BEAT" || rejected.LegA.MinQuantity.Unit != "BEAT" ||
		rejected.LegA.MinNotional.Unit != "USDT" || rejected.LegA.QuantityStep.Unit != "BEAT" ||
		rejected.LegB.IncrementalMargin.Unit != preflightUSDEquivalent {
		t.Fatalf("resolved units=%+v %+v", rejected.LegA, rejected.LegB)
	}

	spot := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	perp := &stubAdapter{positionMode: exchange.PositionModeOneWay, bboErr: errors.New("price down")}
	ready := newIncreasePreflightService(
		&arbitrageCaptureStore{item: runningIncreaseCombo(id)},
		fundedIncreaseSnapshots(),
		readyArbitrageInstrument(11, "aster", "perpetual", "BEATUSDT"),
		readyArbitrageInstrument(22, "okx", "spot", "BEAT-USDT"),
		spot, perp,
	)
	result, err := ready.PreflightIncreaseTarget(context.Background(), "token", id, "1200")
	if err != nil || !result.Executable {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.TargetNotionalUnit != preflightUSDEquivalent ||
		result.LegA.IncrementalQuantityUnit != "BEAT" ||
		result.LegB.IncrementalQuantityUnit != "BEAT" {
		t.Fatalf("target units=%+v", result)
	}
	if result.LegB.IncrementalMargin.Status != "known" ||
		result.LegB.IncrementalMargin.Unit != preflightUSDEquivalent ||
		result.LegA.IncrementalMargin.Status != "unknown" ||
		result.LegA.IncrementalMargin.Unit != preflightUSDEquivalent {
		t.Fatalf("perp=%+v spot=%+v", result.LegA.IncrementalMargin, result.LegB.IncrementalMargin)
	}
	if result.LegB.MinNotional.Unit != "USDT" || result.LegB.MinQuantity.Unit != "BEAT" ||
		result.LegB.QuantityStep.Unit != "BEAT" ||
		result.LegB.EstimatedFee.Status != "unknown" || result.LegB.EstimatedFee.Value != "" ||
		result.LegB.EstimatedFee.Unit != "" {
		t.Fatalf("constraints=%+v fee=%+v", result.LegB, result.LegB.EstimatedFee)
	}
	sawFee, sawMargin := false, false
	for _, warning := range result.Warnings {
		switch warning.Code {
		case "fee_unknown":
			sawFee = warning.Details["reason"] == "fee_estimate_unavailable" && warning.Details["unit"] == ""
		case "margin_unknown":
			sawMargin = warning.Details["reason"] == "margin_preview_unavailable" &&
				warning.Details["unit"] == preflightUSDEquivalent
		}
	}
	if !sawFee || !sawMargin {
		t.Fatalf("warnings=%+v", result.Warnings)
	}
}

func TestCreatePreflightPriceWarningUsesBaseAsset(t *testing.T) {
	instrumentA := readyArbitrageInstrument(11, "aster", "perpetual", "BEATUSDT")
	instrumentB := readyArbitrageInstrument(22, "okx", "spot", "BEAT-USDT")
	aster := &stubAdapter{positionMode: exchange.PositionModeOneWay, bboErr: errors.New("price down")}
	okx := &stubAdapter{positionMode: exchange.PositionModeOneWay, bboErr: errors.New("price down")}
	snapshots := &arbitrageBaselineSnapshots{
		calls: map[string]int{},
		snapshots: map[string]portfolio.Snapshot{
			"okx":   {AvailableFundsUSD: "100000", SpotBalances: map[string]string{"USDT": "100000", "BEAT": "100000"}},
			"aster": {AvailableFundsUSD: "100000"},
		},
	}
	service := newIncreasePreflightService(
		&arbitrageCaptureStore{}, snapshots, instrumentA, instrumentB, aster, okx,
	)
	result, err := service.PreflightArbitrageCombination(context.Background(), CreateArbitrageInput{
		Token: "token", LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		AskThresholdBps: "1", BidThresholdBps: "-1", TargetNotional: "1000",
		ExecutionMode: "simultaneous_market", MakerLeg: "a",
	})
	if err != nil || !result.Executable {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.LegB.RequiredMargin.Unit != preflightUSDEquivalent ||
		result.LegB.RequiredMargin.Status != "known" ||
		result.LegA.RequiredMargin.Unit != preflightUSDEquivalent ||
		result.LegA.RequiredMargin.Status != "unknown" {
		t.Fatalf("perp=%+v spot=%+v", result.LegA.RequiredMargin, result.LegB.RequiredMargin)
	}
	sawPrice := false
	for _, warning := range result.Warnings {
		if warning.Code == "price_unknown" && warning.Details["reason"] == "reliable_price_unavailable" &&
			warning.Details["unit"] == "BEAT" {
			sawPrice = true
		}
	}
	if !sawPrice {
		t.Fatalf("warnings=%+v", result.Warnings)
	}
}

func runningIncreaseCombo(id string) ArbitrageCombination {
	return ArbitrageCombination{
		ID: id, Status: "running", RunMode: "spread", TargetNotional: "1000",
		LegALeverage: "4", LegBLeverage: "1",
		LegAVenueNotional: "1000", LegBVenueNotional: "1000",
		ExecutionMode: "simultaneous_market", MakerLeg: "a",
		LegA: ArbitrageLeg{TradingAccountID: 1, InstrumentID: 11, Exchange: "aster"},
		LegB: ArbitrageLeg{TradingAccountID: 2, InstrumentID: 22, Exchange: "okx"},
	}
}

func fundedIncreaseSnapshots() *arbitrageBaselineSnapshots {
	return &arbitrageBaselineSnapshots{
		calls: map[string]int{},
		snapshots: map[string]portfolio.Snapshot{
			"okx": {
				AvailableFundsUSD: "100000",
				SpotBalances:      map[string]string{"USDT": "100000", "BEAT": "100000"},
			},
			"aster": {AvailableFundsUSD: "100000"},
		},
	}
}

func newIncreasePreflightService(
	store *arbitrageCaptureStore,
	snapshots *arbitrageBaselineSnapshots,
	instrumentA, instrumentB Instrument,
	aster, okx exchange.Adapter,
) *Service {
	service := NewService(
		newMemoryStore(),
		arbitrageServiceCatalog{items: map[int64]Instrument{
			instrumentA.ID: instrumentA, instrumentB.ID: instrumentB,
		}},
		arbitrageServiceCredentials{owner: "admin", items: map[int64]Credentials{
			1: {TradingAccountID: 1, ProductName: "ARB", Exchange: "aster"},
			2: {TradingAccountID: 2, ProductName: "ARB", Exchange: "okx"},
		}},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"aster": aster, "okx": okx}),
		time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	service.ConfigureArbitrage(store)
	service.ConfigureArbitragePositionSnapshots(snapshots)
	return service
}
