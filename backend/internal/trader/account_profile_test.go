package trader

import (
	"context"
	"testing"
	"time"

	"selfquant/backend/internal/trader/exchange"
)

func TestApplyAccountProfileUsesSelectedOwnedAccount(t *testing.T) {
	adapter := &stubAdapter{profile: exchange.NewAccountProfileResult(
		exchange.AccountProfileStep(
			exchange.AccountProfileStepUnifiedAccount,
			exchange.AccountProfileStatusCompliant,
			"",
			"already enabled",
		),
		exchange.AccountProfileStep(
			exchange.AccountProfileStepMultiAssetCrossMargin,
			exchange.AccountProfileStatusApplied,
			"",
			"updated",
		),
		exchange.AccountProfileStep(
			exchange.AccountProfileStepOneWayPosition,
			exchange.AccountProfileStatusManualRequired,
			"OPEN_ORDERS",
			"cancel orders manually",
		),
	)}
	service := NewService(
		nil,
		stubCatalog{item: Instrument{
			ID: 1, Exchange: "binance", ContractType: "perpetual",
			ExchangeSymbol: "BTCUSDT", SettleAsset: "USDT",
		}},
		stubCredentials{
			owner: "alice",
			item: Credentials{
				TradingAccountID: 7, ProductName: "Funding Arb",
				AccountName: "main", Exchange: "binance",
				APIKey: "key", APISecret: "secret",
			},
		},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"binance": adapter}),
		time.Second,
		nil,
	)

	result, err := service.ApplyAccountProfile(context.Background(), "session", 7)
	if err != nil {
		t.Fatal(err)
	}
	if result.TradingAccountID != 7 || result.Exchange != "binance" ||
		result.OverallStatus != exchange.AccountProfileStatusManualRequired ||
		len(result.Steps) != 3 ||
		result.Steps[2].Code != "OPEN_ORDERS" {
		t.Fatalf("result=%+v", result)
	}
}

func TestApplyAccountProfileRejectsUnsupportedSelectedAccount(t *testing.T) {
	service := NewService(
		nil,
		stubCatalog{},
		stubCredentials{
			owner: "alice",
			item: Credentials{
				TradingAccountID: 9, ProductName: "Prediction",
				AccountName: "wallet", Exchange: "polymarket",
			},
		},
		exchange.NewTestRegistry(nil),
		time.Second,
		nil,
	)
	_, err := service.ApplyAccountProfile(context.Background(), "session", 9)
	if err != ErrUnsupportedExchange {
		t.Fatalf("err=%v", err)
	}
}
