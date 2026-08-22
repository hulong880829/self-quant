package portfolio

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

type bitgetAdapter struct {
	client *http.Client
	base   string
}

func newBitget(client *http.Client, base string) Adapter {
	if base == "" {
		base = "https://api.bitget.com"
	}
	return &bitgetAdapter{client: client, base: strings.TrimRight(base, "/")}
}

func (a *bitgetAdapter) get(ctx context.Context, path string, credentials Credentials, target any) error {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	headers := http.Header{
		"Access-Key": {credentials.APIKey}, "Access-Passphrase": {credentials.Passphrase},
		"Access-Timestamp": {ts}, "Access-Sign": {hmacBase64(credentials.APISecret, ts+http.MethodGet+path)},
		"Locale": {"en-US"},
	}
	return getJSON(ctx, a.client, a.base+path, headers, target)
}

func (a *bitgetAdapter) Snapshot(ctx context.Context, credentials Credentials) (Snapshot, error) {
	var assets struct {
		Code, Msg string
		Data      struct {
			AccountEquity string `json:"accountEquity"`
			EffEquity     string `json:"effEquity"`
			Assets        []struct {
				Coin    string `json:"coin"`
				Balance string `json:"balance"`
				Debt    string `json:"debt"`
			} `json:"assets"`
		} `json:"data"`
	}
	if err := a.get(ctx, "/api/v3/account/assets", credentials, &assets); err != nil {
		return Snapshot{}, fmt.Errorf("bitget assets: %w", err)
	}
	if assets.Code != "00000" {
		return Snapshot{}, fmt.Errorf("bitget assets rejected: %s", assets.Msg)
	}
	available := assets.Data.EffEquity
	if available == "" {
		available = assets.Data.AccountEquity
	}
	result := Snapshot{
		AccountEquityUSD: assets.Data.AccountEquity, AvailableFundsUSD: available,
		SpotBalances: make(map[string]string), UpdatedAt: time.Now().UTC(),
	}
	for _, item := range assets.Data.Assets {
		balance, balanceErr := parseDecimal(item.Balance)
		debt, debtErr := parseDecimal(item.Debt)
		if balanceErr != nil {
			balance = decimal.Zero
		}
		if debtErr != nil {
			debt = decimal.Zero
		}
		setSpotBalance(result.SpotBalances, item.Coin, balance.Sub(debt))
	}
	for _, productType := range []string{"USDT-FUTURES", "USDC-FUTURES", "COIN-FUTURES"} {
		path := "/api/v3/position/current-position?category=" + productType
		var wire struct {
			Code, Msg string
			Data      struct {
				List []struct {
					Symbol          string `json:"symbol"`
					PosSide         string `json:"posSide"`
					Total           string `json:"total"`
					AvgPrice        string `json:"avgPrice"`
					MarkPrice       string `json:"markPrice"`
					UnrealisedPnL   string `json:"unrealisedPnl"`
					PositionBalance string `json:"positionBalance"`
					Leverage        string `json:"leverage"`
				} `json:"list"`
			} `json:"data"`
		}
		if err := a.get(ctx, path, credentials, &wire); err != nil {
			return Snapshot{}, fmt.Errorf("bitget positions: %w", err)
		}
		if wire.Code != "00000" {
			return Snapshot{}, fmt.Errorf("bitget positions rejected: %s", wire.Msg)
		}
		for _, item := range wire.Data.List {
			size, err := absoluteDecimal(item.Total)
			if err != nil || size.IsZero() {
				continue
			}
			balance, balanceErr := absoluteDecimal(item.PositionBalance)
			leverage, leverageErr := absoluteDecimal(item.Leverage)
			mark, markErr := absoluteDecimal(item.MarkPrice)
			if balanceErr != nil || leverageErr != nil || markErr != nil {
				continue
			}
			notional := balance.Mul(leverage)
			if productType == "COIN-FUTURES" {
				notional = notional.Mul(mark)
			}
			side := strings.ToLower(item.PosSide)
			if side != "short" {
				side = "long"
			}
			symbol := normalizeSymbol(item.Symbol)
			result.Positions = append(result.Positions, Position{
				Key: "bitget:" + symbol + ":" + side, Kind: "cex", Exchange: "Bitget", Symbol: symbol, Side: side,
				NotionalUSD: notional.String(), Size: size.String(), EntryPrice: item.AvgPrice,
				MarkPrice: item.MarkPrice, UnrealizedPnL: item.UnrealisedPnL,
				WireSymbol: item.Symbol, ProductCategory: strings.ToLower(productType),
			})
		}
	}
	attachQuantities(&result)
	return result, nil
}
