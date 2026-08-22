package portfolio

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

type binanceAdapter struct {
	client *http.Client
	base   string
}

func newBinance(client *http.Client, base string) Adapter {
	if base == "" {
		base = "https://papi.binance.com"
	}
	return &binanceAdapter{client: client, base: strings.TrimRight(base, "/")}
}

func (a *binanceAdapter) signedGet(ctx context.Context, path string, credentials Credentials, target any) error {
	query := url.Values{"timestamp": {strconv.FormatInt(time.Now().UnixMilli(), 10)}, "recvWindow": {"5000"}}
	unsigned := query.Encode()
	rawURL := a.base + path + "?" + unsigned + "&signature=" + hmacHex256(credentials.APISecret, unsigned)
	return getJSON(ctx, a.client, rawURL, http.Header{"X-Mbx-Apikey": {credentials.APIKey}}, target)
}

func (a *binanceAdapter) Snapshot(ctx context.Context, credentials Credentials) (Snapshot, error) {
	var account struct {
		ActualEquity           string `json:"actualEquity"`
		AccountInitialMargin   string `json:"accountInitialMargin"`
		AccountMaintMargin     string `json:"accountMaintMargin"`
		TotalMaintenanceMargin string `json:"totalMaintenanceMargin"`
		TotalAvailableBalance  string `json:"totalAvailableBalance"`
	}
	if err := a.signedGet(ctx, "/papi/v1/account", credentials, &account); err != nil {
		return Snapshot{}, fmt.Errorf("binance unified account: %w", err)
	}
	var balances []struct {
		Asset               string `json:"asset"`
		CrossMarginAsset    string `json:"crossMarginAsset"`
		CrossMarginBorrowed string `json:"crossMarginBorrowed"`
	}
	if err := a.signedGet(ctx, "/papi/v1/balance", credentials, &balances); err != nil {
		return Snapshot{}, fmt.Errorf("binance balances: %w", err)
	}
	type wirePosition struct {
		Symbol           string `json:"symbol"`
		PositionAmt      string `json:"positionAmt"`
		PositionSide     string `json:"positionSide"`
		EntryPrice       string `json:"entryPrice"`
		MarkPrice        string `json:"markPrice"`
		UnrealizedProfit string `json:"unRealizedProfit"`
		Notional         string `json:"notional"`
		NotionalValue    string `json:"notionalValue"`
	}
	type categorizedPosition struct {
		wirePosition
		category string
	}
	var positions []categorizedPosition
	for _, target := range []struct{ path, category string }{
		{"/papi/v1/um/positionRisk", "um"},
		{"/papi/v1/cm/positionRisk", "cm"},
	} {
		var source []wirePosition
		if err := a.signedGet(ctx, target.path, credentials, &source); err != nil {
			return Snapshot{}, fmt.Errorf("binance positions: %w", err)
		}
		for _, item := range source {
			positions = append(positions, categorizedPosition{wirePosition: item, category: target.category})
		}
	}
	result := Snapshot{
		AvailableFundsUSD: account.TotalAvailableBalance,
		SpotBalances:      make(map[string]string),
		UpdatedAt:         time.Now().UTC(),
	}
	for _, item := range balances {
		asset, assetErr := parseDecimal(item.CrossMarginAsset)
		if assetErr != nil {
			asset = decimal.Zero
		}
		borrowed, borrowedErr := parseDecimal(item.CrossMarginBorrowed)
		if borrowedErr != nil {
			borrowed = decimal.Zero
		}
		setSpotBalance(result.SpotBalances, item.Asset, asset.Sub(borrowed))
	}
	equity := account.ActualEquity
	if result.AvailableFundsUSD == "" {
		eq, eqErr := parseDecimal(equity)
		initial, initialErr := parseDecimal(account.AccountInitialMargin)
		if eqErr == nil && initialErr == nil {
			result.AvailableFundsUSD = eq.Sub(initial).String()
		}
	}
	result.AccountEquityUSD = equity
	maintenance := account.AccountMaintMargin
	if maintenance == "" {
		maintenance = account.TotalMaintenanceMargin
	}
	result.RiskPercent = riskPercent(maintenance, equity)
	for _, item := range positions {
		size, err := parseDecimal(item.PositionAmt)
		if err != nil || size.IsZero() {
			continue
		}
		side := sideFromSigned(size)
		if strings.EqualFold(item.PositionSide, "SHORT") {
			side = "short"
		} else if strings.EqualFold(item.PositionSide, "LONG") {
			side = "long"
		}
		notional := item.Notional
		if notional == "" {
			notional = item.NotionalValue
		}
		notionalValue, err := absoluteDecimal(notional)
		if err != nil {
			mark, markErr := parseDecimal(item.MarkPrice)
			if markErr != nil {
				continue
			}
			notionalValue = size.Abs().Mul(mark.Abs())
		}
		symbol := normalizeSymbol(item.Symbol)
		pnl, pnlErr := parseDecimal(item.UnrealizedProfit)
		if pnlErr != nil {
			continue
		}
		result.Positions = append(result.Positions, Position{
			Key: "binance:" + symbol + ":" + side, Kind: "cex", Exchange: "Binance",
			Symbol: symbol, Side: side, NotionalUSD: notionalValue.String(), Size: size.Abs().String(),
			EntryPrice: item.EntryPrice, MarkPrice: item.MarkPrice, UnrealizedPnL: pnl.String(),
			WireSymbol: item.Symbol, ProductCategory: item.category,
		})
	}
	attachQuantities(&result)
	return result, nil
}
