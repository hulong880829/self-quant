package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

type hyperliquidAdapter struct {
	client *http.Client
	base   string
}

func newHyperliquid(client *http.Client, base string) Adapter {
	if base == "" {
		base = "https://api.hyperliquid.xyz"
	}
	return &hyperliquidAdapter{client: client, base: strings.TrimRight(base, "/")}
}

const hyperliquidHIP3Dex = "xyz"

type hyperliquidPerpState struct {
	MarginSummary struct {
		AccountValue    flexDecimalString `json:"accountValue"`
		TotalMarginUsed flexDecimalString `json:"totalMarginUsed"`
	} `json:"marginSummary"`
	CrossMaintenanceMarginUsed flexDecimalString `json:"crossMaintenanceMarginUsed"`
	Withdrawable               flexDecimalString `json:"withdrawable"`
	AssetPositions             []struct {
		Position struct {
			Coin          string            `json:"coin"`
			Szi           flexDecimalString `json:"szi"`
			EntryPx       flexDecimalString `json:"entryPx"`
			PositionValue flexDecimalString `json:"positionValue"`
			UnrealizedPnl flexDecimalString `json:"unrealizedPnl"`
		} `json:"position"`
	} `json:"assetPositions"`
}

type hyperliquidSpotBalance struct {
	Coin  string            `json:"coin"`
	Token json.Number       `json:"token"`
	Hold  flexDecimalString `json:"hold"`
	Total flexDecimalString `json:"total"`
}

type hyperliquidSpotState struct {
	Balances []hyperliquidSpotBalance `json:"balances"`
}

func (a *hyperliquidAdapter) Snapshot(ctx context.Context, credentials Credentials) (Snapshot, error) {
	user := strings.TrimSpace(credentials.APIKey)
	if user == "" {
		return Snapshot{}, fmt.Errorf("hyperliquid snapshot requires a wallet address")
	}

	var perp hyperliquidPerpState
	if err := a.postInfo(ctx, map[string]any{"type": "clearinghouseState", "user": user}, &perp); err != nil {
		return Snapshot{}, fmt.Errorf("hyperliquid clearinghouse: %w", err)
	}

	var xyz hyperliquidPerpState
	if err := a.postInfo(ctx, map[string]any{
		"type": "clearinghouseState", "user": user, "dex": hyperliquidHIP3Dex,
	}, &xyz); err != nil {
		slog.Default().Warn("hyperliquid xyz clearinghouse skipped", "err", err)
		xyz = hyperliquidPerpState{}
	}

	var spot hyperliquidSpotState
	if err := a.postInfo(ctx, map[string]any{"type": "spotClearinghouseState", "user": user}, &spot); err != nil {
		return Snapshot{}, fmt.Errorf("hyperliquid spot clearinghouse: %w", err)
	}

	mids, err := a.spotMids(ctx, spot)
	if err != nil {
		mids = nil
	}

	result := Snapshot{
		SpotBalances: make(map[string]string),
		UpdatedAt:    time.Now().UTC(),
	}
	spotEquity := decimal.Zero
	spotAvailable := decimal.Zero
	for _, item := range spot.Balances {
		total, parseErr := walletDEXParseOptional(string(item.Total))
		if parseErr != nil {
			continue
		}
		setSpotBalance(result.SpotBalances, item.Coin, total)
		if walletDEXStableUSD(item.Coin) {
			spotEquity = spotEquity.Add(total)
			spotAvailable = spotAvailable.Add(hyperliquidSpotAvailable(item))
			continue
		}
		if total.IsZero() {
			continue
		}
		if mid, ok := hyperliquidMid(mids, item.Coin, item.Token.String()); ok {
			spotEquity = spotEquity.Add(total.Mul(mid))
		}
	}

	result.AccountEquityUSD = spotEquity.String()
	result.AvailableFundsUSD = spotAvailable.String()
	result.RiskPercent = walletDEXRiskPercent(
		walletDEXAdd(hyperliquidMaintenance(perp), hyperliquidMaintenance(xyz)),
		result.AccountEquityUSD,
	)

	hyperliquidAppendPositions(&result, perp)
	hyperliquidAppendPositions(&result, xyz)
	attachQuantities(&result)
	return result, nil
}

func (a *hyperliquidAdapter) spotMids(ctx context.Context, spot hyperliquidSpotState) (map[string]flexDecimalString, error) {
	for _, item := range spot.Balances {
		if walletDEXStableUSD(item.Coin) || walletDEXZeroOrEmpty(string(item.Total)) {
			continue
		}
		var mids map[string]flexDecimalString
		if err := a.postInfo(ctx, map[string]any{"type": "allMids"}, &mids); err != nil {
			return nil, err
		}
		return mids, nil
	}
	return nil, nil
}

func (a *hyperliquidAdapter) postInfo(ctx context.Context, payload any, target any) error {
	return postJSON(ctx, a.client, a.base+"/info", http.Header{"Accept": {"application/json"}}, payload, target)
}

func hyperliquidAppendPositions(result *Snapshot, state hyperliquidPerpState) {
	for _, item := range state.AssetPositions {
		coin := strings.TrimSpace(item.Position.Coin)
		if coin == "" {
			continue
		}
		size, parseErr := parseDecimal(string(item.Position.Szi))
		if parseErr != nil || size.IsZero() {
			continue
		}
		notional, notionalErr := absoluteDecimal(string(item.Position.PositionValue))
		if notionalErr != nil {
			continue
		}
		mark := ""
		if notional.IsPositive() {
			mark = notional.Div(size.Abs()).String()
		}
		pnl, pnlErr := parseDecimal(string(item.Position.UnrealizedPnl))
		if pnlErr != nil {
			continue
		}
		side := sideFromSigned(size)
		baseAsset := hyperliquidPositionBaseAsset(coin)
		symbol := normalizeSymbol(baseAsset + "USDC")
		keySymbol := symbol
		if strings.Contains(coin, ":") {
			keySymbol = coin
		}
		result.Positions = append(result.Positions, Position{
			Key: "hyperliquid:" + keySymbol + ":" + side, Kind: "cex", Exchange: "Hyperliquid",
			Symbol: symbol, Side: side, NotionalUSD: notional.String(), Size: size.Abs().String(),
			EntryPrice: string(item.Position.EntryPx), MarkPrice: mark, UnrealizedPnL: pnl.String(),
			WireSymbol: coin, ProductCategory: "perp", BaseAsset: baseAsset,
		})
	}
}

func hyperliquidPositionBaseAsset(coin string) string {
	if index := strings.Index(coin, ":"); index >= 0 {
		coin = coin[index+1:]
	}
	return normalizeSymbol(coin)
}

func hyperliquidMaintenance(state hyperliquidPerpState) string {
	return walletDEXFirstMeaningful(
		string(state.CrossMaintenanceMarginUsed),
		string(state.MarginSummary.TotalMarginUsed),
	)
}

func hyperliquidSpotAvailable(item hyperliquidSpotBalance) decimal.Decimal {
	total, err := walletDEXParseOptional(string(item.Total))
	if err != nil {
		return decimal.Zero
	}
	hold, err := walletDEXParseOptional(string(item.Hold))
	if err != nil {
		return decimal.Zero
	}
	avail := total.Sub(hold)
	if avail.IsNegative() {
		return decimal.Zero
	}
	return avail
}

func hyperliquidMid(mids map[string]flexDecimalString, coin, token string) (decimal.Decimal, bool) {
	if len(mids) == 0 {
		return decimal.Zero, false
	}
	keys := []string{coin}
	if token != "" {
		keys = append(keys, "@"+token)
	}
	keys = append(keys, coin+"/USDC", coin+"/USDT")
	for _, key := range keys {
		raw, ok := mids[key]
		if !ok {
			continue
		}
		parsed, valid := walletDEXAmount(string(raw))
		if valid && parsed.IsPositive() {
			return parsed, true
		}
	}
	return decimal.Zero, false
}

func (a *hyperliquidAdapter) AccountFeeRates(
	ctx context.Context,
	credentials Credentials,
) (AccountFeeRates, error) {
	user := strings.TrimSpace(credentials.APIKey)
	if user == "" {
		return AccountFeeRates{}, fmt.Errorf("hyperliquid fee rates require a wallet address")
	}
	var payload struct {
		UserAddRate       string `json:"userAddRate"`
		UserCrossRate     string `json:"userCrossRate"`
		UserSpotAddRate   string `json:"userSpotAddRate"`
		UserSpotCrossRate string `json:"userSpotCrossRate"`
	}
	if err := a.postInfo(ctx, map[string]any{"type": "userFees", "user": user}, &payload); err != nil {
		return AccountFeeRates{}, fmt.Errorf("hyperliquid userFees: %w", err)
	}
	spot, err := parseMarketFee(payload.UserSpotAddRate, payload.UserSpotCrossRate)
	if err != nil {
		return AccountFeeRates{}, fmt.Errorf("hyperliquid spot fee: %w", err)
	}
	contract, err := parseMarketFee(payload.UserAddRate, payload.UserCrossRate)
	if err != nil {
		return AccountFeeRates{}, fmt.Errorf("hyperliquid contract fee: %w", err)
	}
	return AccountFeeRates{Source: "venue", Spot: spot, Contract: contract}, nil
}
