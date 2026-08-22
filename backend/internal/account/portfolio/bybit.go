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

type bybitAdapter struct {
	client *http.Client
	base   string
}

func newBybit(client *http.Client, base string) Adapter {
	if base == "" {
		base = "https://api.bybit.com"
	}
	return &bybitAdapter{client: client, base: strings.TrimRight(base, "/")}
}

func (a *bybitAdapter) get(ctx context.Context, path string, credentials Credentials, target any) error {
	ts, window := strconv.FormatInt(time.Now().UnixMilli(), 10), "5000"
	parts := strings.SplitN(path, "?", 2)
	query := ""
	if len(parts) == 2 {
		query = parts[1]
	}
	headers := http.Header{
		"X-Bapi-Api-Key": {credentials.APIKey}, "X-Bapi-Timestamp": {ts}, "X-Bapi-Recv-Window": {window},
		"X-Bapi-Sign": {hmacHex256(credentials.APISecret, ts+credentials.APIKey+window+query)},
	}
	return getJSON(ctx, a.client, a.base+path, headers, target)
}

func (a *bybitAdapter) Snapshot(ctx context.Context, credentials Credentials) (Snapshot, error) {
	var wallet struct {
		RetCode int    `json:"retCode"`
		RetMsg  string `json:"retMsg"`
		Result  struct {
			List []struct {
				TotalAvailableBalance, TotalEquity, TotalMaintenanceMargin string
				Coin                                                       []struct {
					Coin, WalletBalance, BorrowAmount, SpotBorrow string
				} `json:"coin"`
			} `json:"list"`
		} `json:"result"`
	}
	if err := a.get(ctx, "/v5/account/wallet-balance?accountType=UNIFIED", credentials, &wallet); err != nil {
		return Snapshot{}, fmt.Errorf("bybit wallet: %w", err)
	}
	if wallet.RetCode != 0 || len(wallet.Result.List) == 0 {
		return Snapshot{}, fmt.Errorf("bybit wallet rejected: %s", wallet.RetMsg)
	}
	item := wallet.Result.List[0]
	result := Snapshot{
		AccountEquityUSD: item.TotalEquity, AvailableFundsUSD: item.TotalAvailableBalance,
		RiskPercent:  riskPercent(item.TotalMaintenanceMargin, item.TotalEquity),
		SpotBalances: make(map[string]string), UpdatedAt: time.Now().UTC(),
	}
	for _, coin := range item.Coin {
		balance, balanceErr := parseDecimal(coin.WalletBalance)
		if balanceErr != nil {
			balance = decimal.Zero
		}
		borrowed, borrowErr := parseDecimal(coin.SpotBorrow)
		if borrowErr != nil {
			borrowed, borrowErr = parseDecimal(coin.BorrowAmount)
		}
		if borrowErr != nil {
			borrowed = decimal.Zero
		}
		setSpotBalance(result.SpotBalances, coin.Coin, balance.Sub(borrowed))
	}
	targets := []url.Values{
		{"category": {"linear"}, "settleCoin": {"USDT"}, "limit": {"200"}},
		{"category": {"linear"}, "settleCoin": {"USDC"}, "limit": {"200"}},
		{"category": {"inverse"}, "limit": {"200"}},
		{"category": {"option"}, "limit": {"200"}},
	}
	for _, query := range targets {
		category := query.Get("category")
		cursor := ""
		for {
			q := url.Values{}
			for key, values := range query {
				q[key] = append([]string(nil), values...)
			}
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			var wire struct {
				RetCode int    `json:"retCode"`
				RetMsg  string `json:"retMsg"`
				Result  struct {
					NextPageCursor string                                                                                   `json:"nextPageCursor"`
					List           []struct{ Symbol, Side, Size, AvgPrice, MarkPrice, PositionValue, UnrealisedPnl string } `json:"list"`
				} `json:"result"`
			}
			if err := a.get(ctx, "/v5/position/list?"+q.Encode(), credentials, &wire); err != nil {
				return Snapshot{}, fmt.Errorf("bybit positions: %w", err)
			}
			if wire.RetCode != 0 {
				return Snapshot{}, fmt.Errorf("bybit positions rejected: %s", wire.RetMsg)
			}
			for _, position := range wire.Result.List {
				size, err := absoluteDecimal(position.Size)
				if err != nil || size.IsZero() {
					continue
				}
				notional, err := absoluteDecimal(position.PositionValue)
				if err != nil {
					continue
				}
				side := "long"
				if strings.EqualFold(position.Side, "Sell") {
					side = "short"
				}
				symbol := normalizeSymbol(position.Symbol)
				result.Positions = append(result.Positions, Position{
					Key: "bybit:" + symbol + ":" + side, Kind: "cex", Exchange: "Bybit", Symbol: symbol, Side: side,
					NotionalUSD: notional.String(), Size: size.String(), EntryPrice: position.AvgPrice,
					MarkPrice: position.MarkPrice, UnrealizedPnL: position.UnrealisedPnl,
					WireSymbol: position.Symbol, ProductCategory: category,
				})
			}
			cursor = wire.Result.NextPageCursor
			if cursor == "" {
				break
			}
		}
	}
	attachQuantities(&result)
	return result, nil
}
