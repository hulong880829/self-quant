package account

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/account/portfolio"
)

type staticInstrumentCatalog map[string]instrumentSpec

func (c staticInstrumentCatalog) Lookup(
	_ context.Context,
	exchange string,
	symbol string,
) (instrumentSpec, bool, error) {
	item, ok := c[instrumentKey(exchange, symbol)]
	return item, ok, nil
}

func TestNormalizeCEXSnapshotUsesInstrumentContractUnits(t *testing.T) {
	service := &Service{instruments: staticInstrumentCatalog{
		instrumentKey("okx", "HYPE-USDT-SWAP"): {
			BaseAsset: "HYPE", ContractSize: decimal.RequireFromString("0.01"),
			Metadata: map[string]any{
				"contractModel": "linear", "positionSizeUnit": "contracts", "ctMult": "2",
			},
		},
		instrumentKey("gate", "GRVT_USDT"): {
			BaseAsset: "GRVT", ContractSize: decimal.RequireFromString("0.1"),
			Metadata: map[string]any{
				"contractModel": "linear", "positionSizeUnit": "contracts",
			},
		},
	}}
	okxSnapshot := portfolio.Snapshot{
		SpotBalances: map[string]string{"HYPE": "4"},
		Positions: []portfolio.Position{
			{Kind: "cex", Exchange: "OKX", Symbol: "HYPE-USDT-SWAP", WireSymbol: "HYPE-USDT-SWAP",
				Side: "long", Size: "880", MarkPrice: "40", NotionalUSD: "704"},
		},
	}
	if err := service.normalizeCEXSnapshot(context.Background(), "okx", &okxSnapshot); err != nil {
		t.Fatal(err)
	}
	gateSnapshot := portfolio.Snapshot{
		SpotBalances: map[string]string{"GRVT": "3"},
		Positions: []portfolio.Position{
			{Kind: "cex", Exchange: "Gate", Symbol: "GRVT-USDT", WireSymbol: "GRVT_USDT",
				Side: "short", Size: "5", MarkPrice: "2", NotionalUSD: "1"},
		},
	}
	if err := service.normalizeCEXSnapshot(context.Background(), "gate", &gateSnapshot); err != nil {
		t.Fatal(err)
	}
	if got := okxSnapshot.Positions[0]; got.Size != "17.6" ||
		got.SignedContractSize != "17.6" || got.BaseAsset != "HYPE" || got.SpotSize != "4" {
		t.Fatalf("okx position=%+v", got)
	}
	if got := gateSnapshot.Positions[0]; got.Size != "0.5" ||
		got.SignedContractSize != "-0.5" || got.BaseAsset != "GRVT" || got.SpotSize != "3" {
		t.Fatalf("gate position=%+v", got)
	}
}

func TestNormalizeCEXSnapshotPairsCanonicalXStockSpotKey(t *testing.T) {
	service := &Service{instruments: staticInstrumentCatalog{
		instrumentKey("okx", "GOOGL-USDT-SWAP"): {
			BaseAsset:    "GOOGL",
			ContractSize: decimal.NewFromInt(1),
			Metadata: map[string]any{
				"contractModel": "linear", "positionSizeUnit": "base",
			},
		},
	}}
	snapshot := portfolio.Snapshot{
		SpotBalances: map[string]string{"GOOGL": "0.58"},
		Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "OKX",
			Symbol: "GOOGL-USDT-SWAP", WireSymbol: "GOOGL-USDT-SWAP",
			Side: "short", Size: "0.58", MarkPrice: "346.77",
			NotionalUSD: "201.1266",
		}},
	}
	if err := service.normalizeCEXSnapshot(
		context.Background(), "okx", &snapshot,
	); err != nil {
		t.Fatal(err)
	}
	got := snapshot.Positions[0]
	if got.BaseAsset != "GOOGL" || got.SpotSize != "0.58" ||
		got.SignedContractSize != "-0.58" {
		t.Fatalf("position=%+v", got)
	}
}

func TestNormalizeCEXSnapshotFallsBackToNotionalOverMark(t *testing.T) {
	service := &Service{}
	snapshot := portfolio.Snapshot{
		SpotBalances: map[string]string{},
		Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "OKX", Symbol: "BTC-USDT-SWAP",
			Side: "short", Size: "100", MarkPrice: "50000", NotionalUSD: "1000",
		}},
	}
	if err := service.normalizeCEXSnapshot(context.Background(), "okx", &snapshot); err != nil {
		t.Fatal(err)
	}
	position := snapshot.Positions[0]
	if position.Size != "0.02" || position.SignedContractSize != "-0.02" ||
		position.BaseAsset != "BTC" {
		t.Fatalf("position=%+v", position)
	}
}

func TestNormalizeCEXSnapshotDoesNotAggregateUnknownRawContracts(t *testing.T) {
	service := &Service{}
	snapshot := portfolio.Snapshot{Positions: []portfolio.Position{{
		Kind: "cex", Exchange: "Gate", Symbol: "NEW-USDT",
		Side: "long", Size: "100", MarkPrice: "", NotionalUSD: "",
	}}}
	if err := service.normalizeCEXSnapshot(context.Background(), "gate", &snapshot); err == nil {
		t.Fatal("expected missing instrument warning")
	}
	if snapshot.Positions[0].SignedContractSize != "" {
		t.Fatalf("raw contracts leaked: %+v", snapshot.Positions[0])
	}
}
