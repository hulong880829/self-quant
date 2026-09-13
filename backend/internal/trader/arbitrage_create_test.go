package trader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/account/portfolio"
	"selfquant/backend/internal/trader/exchange"
)

type stubPreviewAdapter struct {
	stubAdapter
	preview                  exchange.LeverageSetPreview
	previewErr               error
	previewCalls             int
	leaveEstMaxOpenUnitEmpty bool
}

func (a *stubPreviewAdapter) PreviewSetLeverage(
	context.Context,
	exchange.Credentials,
	exchange.Instrument,
	decimal.Decimal,
) (exchange.LeverageSetPreview, error) {
	a.previewCalls++
	if a.previewErr != nil {
		return exchange.LeverageSetPreview{}, a.previewErr
	}
	out := a.preview
	if strings.TrimSpace(out.EstMaxOpen) == "" {
		out.EstMaxOpen = "1000000"
	}
	if !a.leaveEstMaxOpenUnitEmpty && strings.TrimSpace(out.EstMaxOpenUnit) == "" {
		out.EstMaxOpenUnit = exchange.EstMaxOpenUnitQuoteNotional
	}
	return out, nil
}

func bitgetCapacityStub(preview exchange.LeverageSetPreview) *stubPreviewAdapter {
	return &stubPreviewAdapter{
		stubAdapter: stubAdapter{
			positionMode: exchange.PositionModeOneWay,
			bboErr:       errors.New("bitget capacity must not call GetBBO"),
			bbo: exchange.BBO{
				BidPrice: "0.0010839", AskPrice: "0.0010839", Timestamp: time.Now(),
			},
		},
		preview: preview,
	}
}

func TestDefaultLegLeverage(t *testing.T) {
	got, err := defaultLegLeverage("perpetual", "")
	if err != nil || !got.Equal(decimal.NewFromInt(4)) {
		t.Fatalf("perp default=%s err=%v", got, err)
	}
	got, err = defaultLegLeverage("spot", "")
	if err != nil || !got.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("spot default=%s err=%v", got, err)
	}
	for _, raw := range []string{"4.5", "1.2", "0", "-1", "abc"} {
		_, err := defaultLegLeverage("perpetual", raw)
		var createErr *ArbitrageCreateError
		if !errors.As(err, &createErr) || createErr.Code != "invalid_leverage" {
			t.Fatalf("raw=%q err=%v", raw, err)
		}
	}
	_, err = defaultLegLeverage("spot", "2")
	var createErr *ArbitrageCreateError
	if !errors.As(err, &createErr) || createErr.Code != "invalid_leverage" {
		t.Fatalf("spot 2 err=%v", err)
	}
}

func TestReadOnlyCreateLegCheckUsesFullTargetMargin(t *testing.T) {
	account := Credentials{Exchange: "okx"}
	instrument := readyArbitrageInstrument(1, "okx", "perpetual", "BTCUSDT")
	err := readOnlyCreateLegCheck(
		account, instrument, "a", decimal.NewFromInt(4),
		decimal.NewFromInt(100),
		portfolio.Snapshot{AvailableFundsUSD: "24"},
	)
	var createErr *ArbitrageCreateError
	if !errors.As(err, &createErr) || createErr.Code != "insufficient_margin" {
		t.Fatalf("err=%v", err)
	}
	if createErr.Details["required"] != "25" || createErr.Details["leverage"] != "4" {
		t.Fatalf("details=%v", createErr.Details)
	}
	if err := readOnlyCreateLegCheck(
		account, instrument, "a", decimal.NewFromInt(4),
		decimal.NewFromInt(100),
		portfolio.Snapshot{
			AvailableFundsUSD: "25",
			Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "okx", Symbol: "BTCUSDT",
				SignedContractSize: "10", MarkPrice: "50",
			}},
		},
	); err != nil {
		t.Fatal(err)
	}
}

func TestBBOMidPriceRejectsStaleQuotes(t *testing.T) {
	fresh := bboMidPrice(exchange.BBO{
		BidPrice: "99", AskPrice: "101", Timestamp: time.Now(),
	}, time.Now())
	if !fresh.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("fresh=%s", fresh)
	}
	stale := bboMidPrice(exchange.BBO{
		BidPrice: "99", AskPrice: "101", Timestamp: time.Now().Add(-time.Minute),
	}, time.Now())
	if stale.IsPositive() {
		t.Fatalf("stale=%s", stale)
	}
}

func TestCreateArbitrageCombinationRejectsFractionalLeverageBeforeVenue(t *testing.T) {
	adapterA := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	service := newCreateTestService(t, adapterA, adapterB, "gate", "okx", nil)
	_, err := service.CreateArbitrageCombination(context.Background(), CreateArbitrageInput{
		Token: "token", LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional: "100", ExecutionMode: "simultaneous_market", MakerLeg: "a",
		LegALeverage: "4.5", IdempotencyKey: "frac-lev",
	})
	var createErr *ArbitrageCreateError
	if !errors.As(err, &createErr) || createErr.Code != "invalid_leverage" || createErr.Leg != "a" {
		t.Fatalf("err=%v", err)
	}
	if adapterA.leverageSets != 0 || adapterB.leverageSets != 0 {
		t.Fatalf("leverage sets a=%d b=%d", adapterA.leverageSets, adapterB.leverageSets)
	}
}

func TestCreateArbitrageCombinationInsufficientMarginSkipsVenue(t *testing.T) {
	preview := &stubPreviewAdapter{stubAdapter: stubAdapter{positionMode: exchange.PositionModeOneWay}}
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	service := newCreateTestService(
		t, preview, adapterB, "bitget", "okx",
		map[string]portfolio.Snapshot{
			"bitget": {AvailableFundsUSD: "10"},
			"okx":    {AvailableFundsUSD: "1000000"},
		},
	)
	_, err := createTestCombination(service, "low-margin")
	var createErr *ArbitrageCreateError
	if !errors.As(err, &createErr) || createErr.Code != "insufficient_margin" {
		t.Fatalf("err=%v", err)
	}
	if preview.previewCalls != 0 || preview.leverageSets != 0 || adapterB.leverageSets != 0 {
		t.Fatalf("preview=%d setA=%d setB=%d", preview.previewCalls, preview.leverageSets, adapterB.leverageSets)
	}
}

func TestCreateArbitrageCombinationBitgetPreviewThenSet(t *testing.T) {
	preview := bitgetCapacityStub(exchange.LeverageSetPreview{
		EstMaxOpen: "100000", MarginChange: decimal.Zero,
	})
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	instrumentA := readyArbitrageInstrument(11, "bitget", "perpetual", "BTCUSDT")
	instrumentA.ContractSize = "100"
	service := newCreateTestServiceWithInstruments(
		t, preview, adapterB, instrumentA,
		readyArbitrageInstrument(22, "okx", "perpetual", "BTC-USDT-SWAP"),
		map[string]portfolio.Snapshot{
			"bitget": {AvailableFundsUSD: "1000000"},
			"okx":    {AvailableFundsUSD: "1000000"},
		},
	)
	_, err := service.CreateArbitrageCombination(context.Background(), CreateArbitrageInput{
		Token: "token", LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional: "80", ExecutionMode: "simultaneous_market", MakerLeg: "a",
		IdempotencyKey: "bitget-preview-set",
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.previewCalls != 1 || preview.leverageSets != 1 || adapterB.leverageSets != 1 {
		t.Fatalf("preview=%d setA=%d setB=%d", preview.previewCalls, preview.leverageSets, adapterB.leverageSets)
	}
	if preview.bboCalls != 0 {
		t.Fatalf("capacity preview called GetBBO %d times", preview.bboCalls)
	}
}

func TestCreateArbitrageCombinationBitgetDoesNotMultiplyEstMaxOpenByPrice(t *testing.T) {
	preview := bitgetCapacityStub(exchange.LeverageSetPreview{
		EstMaxOpen: "100000", MarginChange: decimal.Zero,
	})
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	instrumentA := readyArbitrageInstrument(11, "bitget", "perpetual", "BTCUSDT")
	instrumentA.ContractSize = "100"
	service := newCreateTestServiceWithInstruments(
		t, preview, adapterB, instrumentA,
		readyArbitrageInstrument(22, "okx", "perpetual", "BTC-USDT-SWAP"),
		map[string]portfolio.Snapshot{
			"bitget": {AvailableFundsUSD: "1000000"},
			"okx":    {AvailableFundsUSD: "1000000"},
		},
	)
	_, err := service.CreateArbitrageCombination(context.Background(), CreateArbitrageInput{
		Token: "token", LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional: "150000", ExecutionMode: "simultaneous_market", MakerLeg: "a",
		IdempotencyKey: "bitget-contract-size",
	})
	var createErr *ArbitrageCreateError
	if !errors.As(err, &createErr) || createErr.Code != "position_capacity_exceeded" {
		t.Fatalf("err=%v", err)
	}
	if createErr.Details["remaining"] != "100000" || createErr.Details["required"] != "150000" ||
		createErr.Details["existing"] != "0" {
		t.Fatalf("details=%v", createErr.Details)
	}
	if preview.leverageSets != 0 {
		t.Fatalf("set after capacity fail: %d", preview.leverageSets)
	}
	if preview.bboCalls != 0 {
		t.Fatalf("capacity preview called GetBBO %d times", preview.bboCalls)
	}
}

func TestCreateArbitrageCombinationBitgetIOSTQuoteNotionalPasses(t *testing.T) {
	preview := bitgetCapacityStub(exchange.LeverageSetPreview{
		EstMaxOpen: "100000", MarginChange: decimal.Zero,
	})
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	instrumentA := readyArbitrageInstrument(11, "bitget", "perpetual", "IOSTUSDT")
	instrumentA.BaseAsset = "IOST"
	instrumentA.QuoteAsset = "USDT"
	instrumentA.SettleAsset = "USDT"
	instrumentB := readyArbitrageInstrument(22, "okx", "perpetual", "IOST-USDT-SWAP")
	instrumentB.BaseAsset = "IOST"
	service := newCreateTestServiceWithInstruments(
		t, preview, adapterB, instrumentA, instrumentB,
		map[string]portfolio.Snapshot{
			"bitget": {AvailableFundsUSD: "1000000"},
			"okx":    {AvailableFundsUSD: "1000000"},
		},
	)
	_, err := service.CreateArbitrageCombination(context.Background(), CreateArbitrageInput{
		Token: "token", LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional: "600", ExecutionMode: "simultaneous_market", MakerLeg: "a",
		IdempotencyKey: "bitget-iost-notional",
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.bboCalls != 0 {
		t.Fatalf("capacity preview called GetBBO %d times", preview.bboCalls)
	}
}

func TestCreateArbitrageCombinationBitgetExistingPlusTarget(t *testing.T) {
	preview := bitgetCapacityStub(exchange.LeverageSetPreview{
		EstMaxOpen: "100000", MarginChange: decimal.Zero,
	})
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	instrumentA := readyArbitrageInstrument(11, "bitget", "perpetual", "IOSTUSDT")
	service := newCreateTestServiceWithInstruments(
		t, preview, adapterB, instrumentA,
		readyArbitrageInstrument(22, "okx", "perpetual", "IOST-USDT-SWAP"),
		map[string]portfolio.Snapshot{
			"bitget": {
				AvailableFundsUSD: "1000000",
				Positions: []portfolio.Position{{
					Kind: "cex", Exchange: "bitget", Symbol: "IOSTUSDT",
					WireSymbol: "IOSTUSDT", NotionalUSD: "40000",
					SignedContractSize: "1", MarkPrice: "0.001",
				}},
			},
			"okx": {AvailableFundsUSD: "1000000"},
		},
	)
	_, err := service.CreateArbitrageCombination(context.Background(), CreateArbitrageInput{
		Token: "token", LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional: "60000", ExecutionMode: "simultaneous_market", MakerLeg: "a",
		IdempotencyKey: "bitget-existing-ok",
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.bboCalls != 0 {
		t.Fatalf("capacity preview called GetBBO %d times", preview.bboCalls)
	}

	preview = bitgetCapacityStub(exchange.LeverageSetPreview{
		EstMaxOpen: "100000", MarginChange: decimal.Zero,
	})
	service = newCreateTestServiceWithInstruments(
		t, preview, adapterB, instrumentA,
		readyArbitrageInstrument(22, "okx", "perpetual", "IOST-USDT-SWAP"),
		map[string]portfolio.Snapshot{
			"bitget": {
				AvailableFundsUSD: "1000000",
				Positions: []portfolio.Position{{
					Kind: "cex", Exchange: "bitget", Symbol: "IOSTUSDT",
					WireSymbol: "IOSTUSDT", NotionalUSD: "50000",
					SignedContractSize: "1", MarkPrice: "0.001",
				}},
			},
			"okx": {AvailableFundsUSD: "1000000"},
		},
	)
	_, err = service.CreateArbitrageCombination(context.Background(), CreateArbitrageInput{
		Token: "token", LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional: "60000", ExecutionMode: "simultaneous_market", MakerLeg: "a",
		IdempotencyKey: "bitget-existing-exceed",
	})
	var createErr *ArbitrageCreateError
	if !errors.As(err, &createErr) || createErr.Code != "position_capacity_exceeded" {
		t.Fatalf("err=%v", err)
	}
	if createErr.Details["existing"] != "50000" || createErr.Details["target"] != "60000" ||
		createErr.Details["required"] != "110000" || createErr.Details["remaining"] != "100000" {
		t.Fatalf("details=%v", createErr.Details)
	}
	if preview.leverageSets != 0 || preview.bboCalls != 0 {
		t.Fatalf("set=%d bbo=%d", preview.leverageSets, preview.bboCalls)
	}
}

func TestCreateArbitrageCombinationBitgetUSDCQuoteNotional(t *testing.T) {
	preview := bitgetCapacityStub(exchange.LeverageSetPreview{
		EstMaxOpen: "100000", MarginChange: decimal.Zero,
	})
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	instrumentA := readyArbitrageInstrument(11, "bitget", "perpetual", "BTCPERP")
	instrumentA.BaseAsset = "BTC"
	instrumentA.QuoteAsset = "USDC"
	instrumentA.SettleAsset = "USDC"
	instrumentB := readyArbitrageInstrument(22, "okx", "perpetual", "BTC-USDC-SWAP")
	instrumentB.BaseAsset = "BTC"
	instrumentB.QuoteAsset = "USDC"
	service := newCreateTestServiceWithInstruments(
		t, preview, adapterB, instrumentA, instrumentB,
		map[string]portfolio.Snapshot{
			"bitget": {AvailableFundsUSD: "1000000"},
			"okx":    {AvailableFundsUSD: "1000000"},
		},
	)
	_, err := service.CreateArbitrageCombination(context.Background(), CreateArbitrageInput{
		Token: "token", LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional: "600", ExecutionMode: "simultaneous_market", MakerLeg: "a",
		IdempotencyKey: "bitget-usdc-ok",
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.bboCalls != 0 {
		t.Fatalf("capacity preview called GetBBO %d times", preview.bboCalls)
	}

	preview = bitgetCapacityStub(exchange.LeverageSetPreview{
		EstMaxOpen: "100", MarginChange: decimal.Zero,
	})
	service = newCreateTestServiceWithInstruments(
		t, preview, adapterB, instrumentA, instrumentB,
		map[string]portfolio.Snapshot{
			"bitget": {AvailableFundsUSD: "1000000"},
			"okx":    {AvailableFundsUSD: "1000000"},
		},
	)
	_, err = service.CreateArbitrageCombination(context.Background(), CreateArbitrageInput{
		Token: "token", LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional: "600", ExecutionMode: "simultaneous_market", MakerLeg: "a",
		IdempotencyKey: "bitget-usdc-exceed",
	})
	var createErr *ArbitrageCreateError
	if !errors.As(err, &createErr) || createErr.Code != "position_capacity_exceeded" {
		t.Fatalf("err=%v", err)
	}
	if createErr.Details["remaining"] != "100" || createErr.Details["required"] != "600" {
		t.Fatalf("details=%v", createErr.Details)
	}
	if preview.leverageSets != 0 || preview.bboCalls != 0 {
		t.Fatalf("set=%d bbo=%d", preview.leverageSets, preview.bboCalls)
	}
}

func TestCreateArbitrageCombinationBitgetUnknownEstMaxOpenUnitFailsClosed(t *testing.T) {
	preview := bitgetCapacityStub(exchange.LeverageSetPreview{EstMaxOpen: "100000"})
	preview.leaveEstMaxOpenUnitEmpty = true
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	service := newCreateTestService(t, preview, adapterB, "bitget", "okx", nil)
	_, err := createTestCombination(service, "bitget-unknown-unit")
	var createErr *ArbitrageCreateError
	if !errors.As(err, &createErr) || createErr.Code != "venue_unavailable" {
		t.Fatalf("err=%v", err)
	}
	if preview.leverageSets != 0 || adapterB.leverageSets != 0 {
		t.Fatalf("setA=%d setB=%d", preview.leverageSets, adapterB.leverageSets)
	}
}

func TestCreateArbitrageCombinationBitgetExistingMissingMarkFailsClosed(t *testing.T) {
	preview := bitgetCapacityStub(exchange.LeverageSetPreview{EstMaxOpen: "100000"})
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	service := newCreateTestService(t, preview, adapterB, "bitget", "okx", map[string]portfolio.Snapshot{
		"bitget": {
			AvailableFundsUSD: "1000000",
			Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "bitget", Symbol: "BEAT_USDT",
				WireSymbol: "BEAT_USDT", SignedContractSize: "80",
			}},
		},
		"okx": {AvailableFundsUSD: "1000000"},
	})
	_, err := createTestCombination(service, "bitget-existing-no-mark")
	var createErr *ArbitrageCreateError
	if !errors.As(err, &createErr) || createErr.Code != "venue_unavailable" {
		t.Fatalf("err=%v", err)
	}
	if preview.leverageSets != 0 {
		t.Fatalf("set=%d", preview.leverageSets)
	}
}

func TestCreateArbitrageCombinationBitgetCombinedMargin(t *testing.T) {
	preview := bitgetCapacityStub(exchange.LeverageSetPreview{
		EstMaxOpen: "1000", RequiredMargin: decimal.NewFromInt(9999),
		MarginChange: decimal.NewFromInt(20),
	})
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	service := newCreateTestService(
		t, preview, adapterB, "bitget", "okx",
		map[string]portfolio.Snapshot{
			"bitget": {AvailableFundsUSD: "30"},
			"okx":    {AvailableFundsUSD: "1000000"},
		},
	)
	_, err := service.CreateArbitrageCombination(context.Background(), CreateArbitrageInput{
		Token: "token", LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional: "100", ExecutionMode: "simultaneous_market", MakerLeg: "a",
		IdempotencyKey: "bitget-sum-margin",
	})
	var createErr *ArbitrageCreateError
	if !errors.As(err, &createErr) || createErr.Code != "insufficient_margin" {
		t.Fatalf("err=%v", err)
	}
	if createErr.Details["required"] != "45" || createErr.Details["targetMargin"] != "25" {
		t.Fatalf("details=%v", createErr.Details)
	}
	if preview.leverageSets != 0 {
		t.Fatalf("set=%d", preview.leverageSets)
	}
}

func TestCreateArbitrageCombinationBitgetRequiredMarginDoesNotGate(t *testing.T) {
	preview := bitgetCapacityStub(exchange.LeverageSetPreview{
		EstMaxOpen: "1000", RequiredMargin: decimal.NewFromInt(9999),
		MarginChange: decimal.Zero,
	})
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	service := newCreateTestService(
		t, preview, adapterB, "bitget", "okx",
		map[string]portfolio.Snapshot{
			"bitget": {AvailableFundsUSD: "30"},
			"okx":    {AvailableFundsUSD: "1000000"},
		},
	)
	if _, err := service.CreateArbitrageCombination(context.Background(), CreateArbitrageInput{
		Token: "token", LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional: "100", ExecutionMode: "simultaneous_market", MakerLeg: "a",
		IdempotencyKey: "bitget-required-margin-ignored",
	}); err != nil {
		t.Fatal(err)
	}
	if preview.leverageSets != 1 {
		t.Fatalf("set=%d", preview.leverageSets)
	}
}

func TestCreateArbitrageCombinationBitgetMissingPrice(t *testing.T) {
	preview := bitgetCapacityStub(exchange.LeverageSetPreview{})
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	service := newCreateTestService(t, preview, adapterB, "bitget", "okx", nil)
	if _, err := createTestCombination(service, "no-price"); err != nil {
		t.Fatal(err)
	}
	if preview.previewCalls != 1 || preview.leverageSets != 1 {
		t.Fatalf("preview=%d set=%d", preview.previewCalls, preview.leverageSets)
	}
	if preview.bboCalls != 0 {
		t.Fatalf("capacity preview called GetBBO %d times", preview.bboCalls)
	}
}

func TestCreateArbitrageCombinationBitgetStaleBBOWithoutMark(t *testing.T) {
	preview := bitgetCapacityStub(exchange.LeverageSetPreview{})
	preview.bbo = exchange.BBO{
		BidPrice: "100", AskPrice: "100", Timestamp: time.Now().Add(-time.Minute),
	}
	preview.bboErr = errors.New("bitget capacity must not call GetBBO")
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	service := newCreateTestService(t, preview, adapterB, "bitget", "okx", nil)
	if _, err := createTestCombination(service, "stale-bbo"); err != nil {
		t.Fatal(err)
	}
	if preview.previewCalls != 1 {
		t.Fatalf("preview=%d", preview.previewCalls)
	}
	if preview.bboCalls != 0 {
		t.Fatalf("capacity preview called GetBBO %d times", preview.bboCalls)
	}
}

func TestCreateArbitrageCombinationBitgetPreviewFailureDoesNotSet(t *testing.T) {
	preview := &stubPreviewAdapter{
		stubAdapter: stubAdapter{positionMode: exchange.PositionModeOneWay},
		previewErr:  errors.New("preview down"),
	}
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	service := newCreateTestService(t, preview, adapterB, "bitget", "okx", nil)
	_, err := createTestCombination(service, "preview-fail")
	var createErr *ArbitrageCreateError
	if !errors.As(err, &createErr) || createErr.Code != "venue_unavailable" ||
		createErr.Details["appliedLegs"] != "[]" {
		t.Fatalf("err=%v details=%v", err, createErr.Details)
	}
	if preview.leverageSets != 0 || adapterB.leverageSets != 0 {
		t.Fatalf("setA=%d setB=%d", preview.leverageSets, adapterB.leverageSets)
	}
}

func TestCreateArbitrageCombinationCapacityFailureKeepsAppliedLegs(t *testing.T) {
	adapterA := &stubAdapter{
		positionMode: exchange.PositionModeOneWay,
		leverageResult: exchange.LeverageApplyResult{
			MaxNotional: decimal.NewFromInt(1), CapacityKnown: true,
		},
	}
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	service := newCreateTestService(t, adapterA, adapterB, "binance", "okx", nil)
	_, err := createTestCombination(service, "capacity-a")
	var createErr *ArbitrageCreateError
	if !errors.As(err, &createErr) {
		t.Fatalf("err=%v", err)
	}
	if createErr.Code != "position_capacity_exceeded" ||
		createErr.Details["appliedLegs"] != `["a"]` ||
		createErr.Details["existing"] != "0" || createErr.Details["target"] != "10000" ||
		createErr.Details["required"] != "10000" || createErr.Details["max"] != "1" {
		t.Fatalf("err=%v details=%v", err, createErr.Details)
	}
	if adapterA.leverageSets != 1 || adapterB.leverageSets != 0 {
		t.Fatalf("setA=%d setB=%d", adapterA.leverageSets, adapterB.leverageSets)
	}
}

func TestCreateArbitrageCombinationBCapacityFailureAppliedAB(t *testing.T) {
	adapterA := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	adapterB := &stubAdapter{
		positionMode: exchange.PositionModeOneWay,
		leverageResult: exchange.LeverageApplyResult{
			MaxNotional: decimal.NewFromInt(1), CapacityKnown: true,
		},
	}
	service := newCreateTestService(t, adapterA, adapterB, "okx", "binance", nil)
	_, err := createTestCombination(service, "capacity-b")
	var createErr *ArbitrageCreateError
	if !errors.As(err, &createErr) {
		t.Fatalf("err=%v", err)
	}
	if createErr.Details["appliedLegs"] != `["a","b"]` {
		t.Fatalf("err=%v details=%v", err, createErr.Details)
	}
	if adapterA.leverageSets != 1 || adapterB.leverageSets != 1 {
		t.Fatalf("setA=%d setB=%d", adapterA.leverageSets, adapterB.leverageSets)
	}
}

func TestCreateArbitrageCombinationCapacityWithinMaxPasses(t *testing.T) {
	adapterA := &stubAdapter{
		positionMode: exchange.PositionModeOneWay,
		leverageResult: exchange.LeverageApplyResult{
			MaxNotional: decimal.NewFromInt(20000), CapacityKnown: true,
		},
	}
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	service := newCreateTestService(t, adapterA, adapterB, "binance", "okx", nil)
	item, err := createTestCombination(service, "capacity-ok")
	if err != nil {
		t.Fatal(err)
	}
	if item.IdempotencyKey != "capacity-ok" {
		t.Fatalf("item=%+v", item)
	}
	if adapterA.leverageSets != 1 || adapterB.leverageSets != 1 {
		t.Fatalf("setA=%d setB=%d", adapterA.leverageSets, adapterB.leverageSets)
	}
}

func TestCreateArbitrageCombinationZeroExistingCapacityPasses(t *testing.T) {
	adapterA := &stubAdapter{
		positionMode: exchange.PositionModeOneWay,
		leverageResult: exchange.LeverageApplyResult{
			MaxNotional: decimal.NewFromInt(500000), CapacityKnown: true,
		},
	}
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	instrumentA := readyArbitrageInstrument(11, "binance", "perpetual", "TUSDT")
	instrumentB := readyArbitrageInstrument(22, "okx", "perpetual", "T-USDT-SWAP")
	service := newCreateTestServiceWithInstruments(t, adapterA, adapterB, instrumentA, instrumentB, map[string]portfolio.Snapshot{
		"binance": {AvailableFundsUSD: "1000000"},
		"okx":     {AvailableFundsUSD: "1000000"},
	})
	if _, err := createTestCombination(service, "tusdt-zero"); err != nil {
		t.Fatal(err)
	}
}

func TestCreateArbitrageCombinationExistingPlusTargetExceeds(t *testing.T) {
	adapterA := &stubAdapter{
		positionMode: exchange.PositionModeOneWay,
		leverageResult: exchange.LeverageApplyResult{
			MaxNotional: decimal.NewFromInt(15000), CapacityKnown: true,
		},
	}
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	service := newCreateTestService(t, adapterA, adapterB, "binance", "okx", map[string]portfolio.Snapshot{
		"binance": {
			AvailableFundsUSD: "1000000",
			Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "binance", Symbol: "BEAT_USDT",
				WireSymbol: "BEAT_USDT", SignedContractSize: "80", MarkPrice: "100",
			}},
		},
		"okx": {AvailableFundsUSD: "1000000"},
	})
	_, err := createTestCombination(service, "existing-exceed")
	var createErr *ArbitrageCreateError
	if !errors.As(err, &createErr) || createErr.Code != "position_capacity_exceeded" {
		t.Fatalf("err=%v", err)
	}
	if createErr.Details["existing"] != "8000" || createErr.Details["target"] != "10000" ||
		createErr.Details["required"] != "18000" || createErr.Details["max"] != "15000" ||
		createErr.Details["appliedLegs"] != `["a"]` {
		t.Fatalf("details=%v", createErr.Details)
	}
}

func TestCreateArbitrageCombinationExistingPlusTargetWithinMax(t *testing.T) {
	adapterA := &stubAdapter{
		positionMode: exchange.PositionModeOneWay,
		leverageResult: exchange.LeverageApplyResult{
			MaxNotional: decimal.NewFromInt(20000), CapacityKnown: true,
		},
	}
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	service := newCreateTestService(t, adapterA, adapterB, "binance", "okx", map[string]portfolio.Snapshot{
		"binance": {
			AvailableFundsUSD: "1000000",
			Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "binance", Symbol: "BEAT_USDT",
				WireSymbol: "BEAT_USDT", SignedContractSize: "80", MarkPrice: "100",
			}},
		},
		"okx": {AvailableFundsUSD: "1000000"},
	})
	if _, err := createTestCombination(service, "existing-ok"); err != nil {
		t.Fatal(err)
	}
}

func TestCreateArbitrageCombinationSetUncertain(t *testing.T) {
	adapterA := &stubAdapter{
		positionMode:   exchange.PositionModeOneWay,
		setLeverageErr: exchange.ErrUncertain,
	}
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	service := newCreateTestService(t, adapterA, adapterB, "gate", "okx", nil)
	_, err := createTestCombination(service, "uncertain-a")
	var createErr *ArbitrageCreateError
	if !errors.As(err, &createErr) || createErr.Code != "leverage_apply_failed" ||
		createErr.Leg != "a" ||
		createErr.Details["uncertain"] != "true" || createErr.Details["appliedLegs"] != "[]" {
		t.Fatalf("err=%v details=%v", err, createErr.Details)
	}
	if !strings.Contains(createErr.Message, "uncertain") ||
		strings.Contains(createErr.Message, "was not applied") {
		t.Fatalf("message=%q", createErr.Message)
	}
	if adapterB.leverageSets != 0 {
		t.Fatalf("setB=%d", adapterB.leverageSets)
	}
}

func TestCreateArbitrageCombinationSetRejected(t *testing.T) {
	adapterA := &stubAdapter{
		positionMode:   exchange.PositionModeOneWay,
		setLeverageErr: exchange.ErrRejected,
	}
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	service := newCreateTestService(t, adapterA, adapterB, "gate", "okx", nil)
	_, err := createTestCombination(service, "rejected-a")
	var createErr *ArbitrageCreateError
	if !errors.As(err, &createErr) || createErr.Details["uncertain"] == "true" ||
		!strings.Contains(createErr.Message, "was not applied") {
		t.Fatalf("err=%v details=%v", err, createErr.Details)
	}
	if adapterB.leverageSets != 0 {
		t.Fatalf("setB=%d", adapterB.leverageSets)
	}
}

func TestCreateArbitrageCombinationBSetUncertainKeepsA(t *testing.T) {
	adapterA := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	adapterB := &stubAdapter{
		positionMode:   exchange.PositionModeOneWay,
		setLeverageErr: exchange.ErrUncertain,
	}
	service := newCreateTestService(t, adapterA, adapterB, "gate", "okx", nil)
	_, err := createTestCombination(service, "uncertain-b")
	var createErr *ArbitrageCreateError
	if !errors.As(err, &createErr) || createErr.Details["appliedLegs"] != `["a"]` ||
		createErr.Details["uncertain"] != "true" {
		t.Fatalf("details=%v", createErr.Details)
	}
	if !strings.Contains(createErr.Message, "uncertain") ||
		strings.Contains(createErr.Message, "was not applied") {
		t.Fatalf("message=%q", createErr.Message)
	}
}

func TestCreateArbitrageCombinationInsertFailureLogsAppliedLegs(t *testing.T) {
	adapterA := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	var logs bytes.Buffer
	service := newCreateTestService(t, adapterA, adapterB, "gate", "okx", nil)
	service.logger = slog.New(slog.NewTextHandler(&logs, nil))
	store := &arbitrageCaptureStore{err: errors.New("db down")}
	service.ConfigureArbitrage(store)
	_, err := createTestCombination(service, "insert-fail")
	if !errors.Is(err, ErrPersistence) || !strings.Contains(err.Error(), `appliedLegs=["a","b"]`) {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(logs.String(), "appliedLegs") {
		t.Fatalf("logs=%s", logs.String())
	}
	if adapterA.leverageSets != 1 || adapterB.leverageSets != 1 {
		t.Fatalf("setA=%d setB=%d", adapterA.leverageSets, adapterB.leverageSets)
	}
}

func TestCreateArbitrageCombinationIdempotencySkipsPreview(t *testing.T) {
	preview := &stubPreviewAdapter{stubAdapter: stubAdapter{positionMode: exchange.PositionModeOneWay}}
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	service := newCreateTestService(t, preview, adapterB, "bitget", "okx", nil)
	input := CreateArbitrageInput{
		Token: "token", LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional: "10000", ExecutionMode: "simultaneous_market", MakerLeg: "a",
		IdempotencyKey: "idempotent-preview",
	}
	if _, err := service.CreateArbitrageCombination(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateArbitrageCombination(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if preview.previewCalls != 1 || preview.leverageSets != 1 || adapterB.leverageSets != 1 {
		t.Fatalf("preview=%d setA=%d setB=%d", preview.previewCalls, preview.leverageSets, adapterB.leverageSets)
	}
}

func TestCreateArbitrageCombinationAlreadySetLeverageCreates(t *testing.T) {
	adapterA := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	service := newCreateTestService(t, adapterA, adapterB, "bybit", "okx", nil)
	store := &arbitrageCaptureStore{}
	service.ConfigureArbitrage(store)
	created, err := createTestCombination(service, "already-set-leverage")
	if err != nil {
		t.Fatal(err)
	}
	if created.IdempotencyKey != "already-set-leverage" {
		t.Fatalf("created=%+v", created)
	}
	if adapterA.leverageSets != 1 || adapterB.leverageSets != 1 {
		t.Fatalf("setA=%d setB=%d", adapterA.leverageSets, adapterB.leverageSets)
	}
	if adapterA.calls != 0 || adapterB.calls != 0 {
		t.Fatalf("placeA=%d placeB=%d", adapterA.calls, adapterB.calls)
	}
	if store.item.IdempotencyKey != "already-set-leverage" {
		t.Fatalf("store=%+v", store.item)
	}
}

func TestCreateArbitrageCombinationBybitLeverageRejectedDoesNotInsert(t *testing.T) {
	adapterA := &stubAdapter{
		positionMode: exchange.PositionModeOneWay,
		setLeverageErr: fmt.Errorf(
			"%w: bybit retCode=110013 retMsg=leverage invalid",
			exchange.ErrRejected,
		),
	}
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	service := newCreateTestService(t, adapterA, adapterB, "bybit", "okx", nil)
	store := &arbitrageCaptureStore{}
	service.ConfigureArbitrage(store)
	_, err := createTestCombination(service, "bybit-110013")
	var createErr *ArbitrageCreateError
	if !errors.As(err, &createErr) || createErr.Code != "leverage_apply_failed" ||
		createErr.Details["appliedLegs"] != "[]" ||
		!strings.Contains(createErr.Details["error"], "retCode=110013") {
		t.Fatalf("err=%v details=%v", err, createErr)
	}
	if adapterB.leverageSets != 0 {
		t.Fatalf("setB=%d", adapterB.leverageSets)
	}
	if adapterA.calls != 0 {
		t.Fatalf("placeA=%d", adapterA.calls)
	}
	if store.item.IdempotencyKey != "" {
		t.Fatalf("partial combo=%+v", store.item)
	}
}

func TestCreateArbitrageCombinationDoesNotQueryUnsupportedCapacity(t *testing.T) {
	adapterA := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	adapterB := &stubAdapter{positionMode: exchange.PositionModeOneWay}
	service := newCreateTestService(t, adapterA, adapterB, "gate", "okx", nil)
	if _, err := createTestCombination(service, "no-capacity"); err != nil {
		t.Fatal(err)
	}
	if adapterA.leverageSets != 1 || adapterB.leverageSets != 1 {
		t.Fatalf("setA=%d setB=%d", adapterA.leverageSets, adapterB.leverageSets)
	}
}

func newCreateTestService(
	t *testing.T,
	adapterA, adapterB exchange.Adapter,
	venueA, venueB string,
	snapshots map[string]portfolio.Snapshot,
) *Service {
	t.Helper()
	return newCreateTestServiceWithInstruments(
		t, adapterA, adapterB,
		readyArbitrageInstrument(11, venueA, "perpetual", "BEAT_USDT"),
		readyArbitrageInstrument(22, venueB, "perpetual", "BEAT-USDT-SWAP"),
		snapshots,
	)
}

func newCreateTestServiceWithInstruments(
	t *testing.T,
	adapterA, adapterB exchange.Adapter,
	instrumentA, instrumentB Instrument,
	snapshots map[string]portfolio.Snapshot,
) *Service {
	t.Helper()
	if snapshots == nil {
		snapshots = map[string]portfolio.Snapshot{
			instrumentA.Exchange: {AvailableFundsUSD: "1000000"},
			instrumentB.Exchange: {AvailableFundsUSD: "1000000"},
		}
	}
	store := &arbitrageCaptureStore{}
	service := NewService(
		newMemoryStore(),
		arbitrageServiceCatalog{items: map[int64]Instrument{
			instrumentA.ID: instrumentA, instrumentB.ID: instrumentB,
		}},
		arbitrageServiceCredentials{
			owner: "admin",
			items: map[int64]Credentials{
				1: {
					TradingAccountID: 1, ProductName: "ARB", AccountName: venueAccountName(instrumentA.Exchange),
					Exchange: instrumentA.Exchange,
				},
				2: {
					TradingAccountID: 2, ProductName: "ARB", AccountName: venueAccountName(instrumentB.Exchange),
					Exchange: instrumentB.Exchange,
				},
			},
		},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			instrumentA.Exchange: adapterA, instrumentB.Exchange: adapterB,
		}),
		time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	service.ConfigureArbitrage(store)
	service.ConfigureArbitragePositionSnapshots(&arbitrageBaselineSnapshots{
		calls: map[string]int{}, snapshots: snapshots,
	})
	return service
}

func venueAccountName(venue string) string {
	return venue + "-main"
}

func createTestCombination(service *Service, key string) (ArbitrageCombination, error) {
	return service.CreateArbitrageCombination(context.Background(), CreateArbitrageInput{
		Token: "token", LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional: "10000", ExecutionMode: "simultaneous_market", MakerLeg: "a",
		IdempotencyKey: key,
	})
}
