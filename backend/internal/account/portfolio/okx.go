package portfolio

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

type okxAdapter struct {
	client *http.Client
	base   string
}

func canonicalOKXSpotAsset(
	asset string,
	positionAssets map[string]struct{},
) string {
	asset = normalizeSymbol(asset)
	if len(asset) > 1 && asset[0] == 'X' {
		candidate := asset[1:]
		if _, paired := positionAssets[candidate]; paired {
			return candidate
		}
	}
	return asset
}

func newOKX(client *http.Client, base string) Adapter {
	if base == "" {
		base = "https://www.okx.com"
	}
	return &okxAdapter{client: client, base: strings.TrimRight(base, "/")}
}

func (a *okxAdapter) get(ctx context.Context, path string, credentials Credentials, target any) error {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	headers := http.Header{
		"Ok-Access-Key": {credentials.APIKey}, "Ok-Access-Passphrase": {credentials.Passphrase},
		"Ok-Access-Timestamp": {now}, "Ok-Access-Sign": {hmacBase64(credentials.APISecret, now+http.MethodGet+path)},
	}
	return getJSON(ctx, a.client, a.base+path, headers, target)
}

func (a *okxAdapter) Snapshot(ctx context.Context, credentials Credentials) (Snapshot, error) {
	var balance struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			TotalEq, AdjEq, AvailEq string
			Details                 []struct {
				Ccy, AvailEq, EqUsd string
				CashBal, Liab, Eq   string
			}
		} `json:"data"`
	}
	if err := a.get(ctx, "/api/v5/account/balance", credentials, &balance); err != nil {
		return Snapshot{}, fmt.Errorf("okx balance: %w", err)
	}
	if balance.Code != "0" || len(balance.Data) == 0 {
		return Snapshot{}, fmt.Errorf("okx balance rejected: %s", balance.Msg)
	}
	var wire struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			InstID, Pos, PosSide, AvgPx, MarkPx, Upl, NotionalUsd, Mmr string
		} `json:"data"`
	}
	if err := a.get(ctx, "/api/v5/account/positions", credentials, &wire); err != nil {
		return Snapshot{}, fmt.Errorf("okx positions: %w", err)
	}
	if wire.Code != "0" {
		return Snapshot{}, fmt.Errorf("okx positions rejected: %s", wire.Msg)
	}
	equity := balance.Data[0].TotalEq
	available := balance.Data[0].AvailEq
	if available == "" {
		available = balance.Data[0].AdjEq
	}
	maintenance := "0"
	for _, p := range wire.Data {
		if mm, err := parseDecimal(p.Mmr); err == nil {
			current, _ := parseDecimal(maintenance)
			maintenance = current.Add(mm).String()
		}
	}
	result := Snapshot{
		AccountEquityUSD: equity, AvailableFundsUSD: available,
		RiskPercent:  riskPercent(maintenance, equity),
		SpotBalances: make(map[string]string), UpdatedAt: time.Now().UTC(),
	}
	positionAssets := make(map[string]struct{}, len(wire.Data))
	for _, item := range wire.Data {
		positionAssets[baseAssetSymbol(item.InstID)] = struct{}{}
	}
	for _, detail := range balance.Data[0].Details {
		cash, cashErr := parseDecimal(detail.CashBal)
		liability, liabilityErr := parseDecimal(detail.Liab)
		if cashErr == nil || liabilityErr == nil {
			if cashErr != nil {
				cash = decimal.Zero
			}
			if liabilityErr != nil {
				liability = decimal.Zero
			}
			setSpotBalance(
				result.SpotBalances,
				canonicalOKXSpotAsset(detail.Ccy, positionAssets),
				cash.Sub(liability),
			)
			continue
		}
		if net, netErr := parseDecimal(detail.Eq); netErr == nil {
			setSpotBalance(
				result.SpotBalances,
				canonicalOKXSpotAsset(detail.Ccy, positionAssets),
				net,
			)
		}
	}
	for _, item := range wire.Data {
		size, err := parseDecimal(item.Pos)
		if err != nil || size.IsZero() {
			continue
		}
		side := sideFromSigned(size)
		if item.PosSide == "long" || item.PosSide == "short" {
			side = item.PosSide
		}
		notional, err := absoluteDecimal(item.NotionalUsd)
		if err != nil {
			continue
		}
		symbol := normalizeSymbol(item.InstID)
		result.Positions = append(result.Positions, Position{
			Key: "okx:" + symbol + ":" + side, Kind: "cex", Exchange: "OKX", Symbol: symbol, Side: side,
			NotionalUSD: notional.String(), Size: size.Abs().String(), EntryPrice: item.AvgPx,
			MarkPrice: item.MarkPx, UnrealizedPnL: item.Upl,
			WireSymbol: item.InstID, ProductCategory: "swap",
		})
	}
	attachQuantities(&result)
	return result, nil
}
