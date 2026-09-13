package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	lighterclient "github.com/elliottech/lighter-go/client"
	"github.com/shopspring/decimal"
	corex "selfquant/backend/internal/exchange"
)

type lighterAdapter struct {
	client *http.Client
	base   string
}

func newLighter(client *http.Client, base string) Adapter {
	if base == "" {
		base = "https://mainnet.zklighter.elliot.ai"
	}
	return &lighterAdapter{client: client, base: strings.TrimRight(base, "/")}
}

type lighterAccountsResponse struct {
	Code     json.Number      `json:"code"`
	Message  string           `json:"message"`
	Accounts []lighterAccount `json:"accounts"`
	Account  *lighterAccount  `json:"account"`
}

type lighterAccount struct {
	AccountIndex                      json.Number       `json:"account_index"`
	Index                             json.Number       `json:"index"`
	Collateral                        string            `json:"collateral"`
	AvailableBalance                  string            `json:"available_balance"`
	CrossMaintenanceMarginRequirement string            `json:"cross_maintenance_margin_requirement"`
	Positions                         []lighterPosition `json:"positions"`
}

type lighterPosition struct {
	MarketID      json.Number `json:"market_id"`
	Symbol        string      `json:"symbol"`
	Sign          json.Number `json:"sign"`
	Position      string      `json:"position"`
	AvgEntryPrice string      `json:"avg_entry_price"`
	PositionValue string      `json:"position_value"`
	UnrealizedPnl string      `json:"unrealized_pnl"`
}

func (a *lighterAdapter) Snapshot(ctx context.Context, credentials Credentials) (Snapshot, error) {
	if credentials.AccountIndex == nil || *credentials.AccountIndex < 0 {
		return Snapshot{}, fmt.Errorf("lighter snapshot requires an account index")
	}
	query := url.Values{
		"by":          {"index"},
		"value":       {strconv.FormatInt(*credentials.AccountIndex, 10)},
		"active_only": {"true"},
	}
	var payload lighterAccountsResponse
	if err := getJSON(ctx, a.client, a.base+"/api/v1/account?"+query.Encode(), http.Header{
		"Accept": {"application/json"},
	}, &payload); err != nil {
		return Snapshot{}, fmt.Errorf("lighter account: %w", err)
	}
	if code, err := payload.Code.Int64(); err == nil && code != 0 && code != 200 {
		return Snapshot{}, fmt.Errorf("lighter account: code %s %s", payload.Code.String(), strings.TrimSpace(payload.Message))
	}
	accounts := payload.Accounts
	if len(accounts) == 0 && payload.Account != nil {
		accounts = []lighterAccount{*payload.Account}
	}
	if len(accounts) == 0 {
		return Snapshot{}, fmt.Errorf("lighter account: account index %d not found", *credentials.AccountIndex)
	}
	account, ok := lighterAccountByIndex(accounts, *credentials.AccountIndex)
	if !ok {
		return Snapshot{}, fmt.Errorf("lighter account: response omitted account index %d", *credentials.AccountIndex)
	}

	result := Snapshot{
		SpotBalances: make(map[string]string),
		UpdatedAt:    time.Now().UTC(),
	}
	collateral, err := walletDEXParseOptional(account.Collateral)
	if err != nil {
		return Snapshot{}, fmt.Errorf("lighter collateral: %w", err)
	}
	available, err := walletDEXParseOptional(account.AvailableBalance)
	if err != nil {
		return Snapshot{}, fmt.Errorf("lighter available: %w", err)
	}
	maintenance, err := walletDEXParseOptional(account.CrossMaintenanceMarginRequirement)
	if err != nil {
		return Snapshot{}, fmt.Errorf("lighter maintenance: %w", err)
	}
	equity := collateral
	accountIndex := strconv.FormatInt(*credentials.AccountIndex, 10)
	for _, item := range account.Positions {
		size, err := parseDecimal(item.Position)
		if err != nil || size.IsZero() {
			continue
		}
		sign, _ := item.Sign.Int64()
		if sign < 0 {
			size = size.Neg()
		} else if sign == 0 {
			continue
		}
		notional, err := absoluteDecimal(item.PositionValue)
		if err != nil {
			return Snapshot{}, fmt.Errorf("lighter position %s: invalid position_value", item.Symbol)
		}
		pnl, err := parseDecimal(item.UnrealizedPnl)
		if err != nil {
			return Snapshot{}, fmt.Errorf("lighter position %s: invalid unrealized_pnl", item.Symbol)
		}
		equity = equity.Add(pnl)
		side := sideFromSigned(size)
		symbol := normalizeSymbol(item.Symbol)
		if !strings.HasSuffix(symbol, "USD") && !strings.HasSuffix(symbol, "USDT") && !strings.HasSuffix(symbol, "USDC") {
			symbol += "USD"
		}
		mark := ""
		if notional.IsPositive() && !size.IsZero() {
			mark = notional.Div(size.Abs()).String()
		}
		result.Positions = append(result.Positions, Position{
			Key: "lighter:" + accountIndex + ":" + symbol + ":" + side, Kind: "cex", Exchange: "Lighter",
			Symbol: symbol, Side: side, NotionalUSD: notional.String(), Size: size.Abs().String(),
			EntryPrice: item.AvgEntryPrice, MarkPrice: mark, UnrealizedPnL: pnl.String(),
			WireSymbol: item.Symbol, ProductCategory: "perp",
		})
	}
	result.AccountEquityUSD = equity.String()
	result.AvailableFundsUSD = available.String()
	result.RiskPercent = walletDEXRiskPercent(maintenance.String(), result.AccountEquityUSD)
	attachQuantities(&result)
	return result, nil
}

func lighterAccountByIndex(accounts []lighterAccount, want int64) (lighterAccount, bool) {
	for _, account := range accounts {
		for _, raw := range []json.Number{account.AccountIndex, account.Index} {
			if strings.TrimSpace(raw.String()) == "" {
				continue
			}
			index, err := raw.Int64()
			if err == nil && index == want {
				return account, true
			}
		}
	}
	return lighterAccount{}, false
}

const lighterFeeTick = "0.000001"

func (a *lighterAdapter) AccountFeeRates(
	ctx context.Context,
	credentials Credentials,
) (AccountFeeRates, error) {
	if credentials.AccountIndex == nil || *credentials.AccountIndex < 0 {
		return AccountFeeRates{}, fmt.Errorf("lighter fee rates require an account index")
	}
	headers := http.Header{"Accept": {"application/json"}}
	token, err := lighterAuthToken(credentials)
	if err != nil {
		return AccountFeeRates{}, err
	}
	if token != "" {
		headers.Set("Authorization", token)
	}
	query := url.Values{
		"account_index": {strconv.FormatInt(*credentials.AccountIndex, 10)},
	}
	var payload struct {
		Code                 json.Number `json:"code"`
		Message              string      `json:"message"`
		CurrentMakerFeeTick  json.Number `json:"current_maker_fee_tick"`
		CurrentTakerFeeTick  json.Number `json:"current_taker_fee_tick"`
	}
	if err := getJSON(
		ctx, a.client, a.base+"/api/v1/accountLimits?"+query.Encode(), headers, &payload,
	); err != nil {
		return AccountFeeRates{}, fmt.Errorf("lighter accountLimits: %w", err)
	}
	if code, err := payload.Code.Int64(); err == nil && code != 0 && code != 200 {
		return AccountFeeRates{}, fmt.Errorf("lighter accountLimits: code %s %s", payload.Code.String(), strings.TrimSpace(payload.Message))
	}
	maker, err := lighterRateFromTick(payload.CurrentMakerFeeTick)
	if err != nil {
		return AccountFeeRates{}, fmt.Errorf("lighter maker fee: %w", err)
	}
	taker, err := lighterRateFromTick(payload.CurrentTakerFeeTick)
	if err != nil {
		return AccountFeeRates{}, fmt.Errorf("lighter taker fee: %w", err)
	}
	fee, err := parseMarketFee(maker, taker)
	if err != nil {
		return AccountFeeRates{}, err
	}
	return AccountFeeRates{Source: "venue", Spot: fee, Contract: fee}, nil
}

func lighterAuthToken(credentials Credentials) (string, error) {
	if credentials.AccountIndex == nil || credentials.APIKeyIndex == nil ||
		*credentials.AccountIndex < 0 || *credentials.APIKeyIndex < 0 || *credentials.APIKeyIndex > 255 ||
		strings.TrimSpace(credentials.APISecret) == "" {
		return "", fmt.Errorf("lighter fee rates require API credentials")
	}
	secret, err := corex.LighterTxPrivateKey(credentials.APISecret)
	if err != nil {
		return "", fmt.Errorf("invalid Lighter API private key: %w", err)
	}
	signer, err := lighterclient.NewTxClient(
		nil, secret, *credentials.AccountIndex, uint8(*credentials.APIKeyIndex), 304,
	)
	if err != nil {
		return "", fmt.Errorf("invalid Lighter API private key: %w", err)
	}
	token, err := signer.GetAuthToken(time.Now().Add(10 * time.Minute))
	if err != nil {
		return "", fmt.Errorf("sign Lighter auth token: %w", err)
	}
	return token, nil
}

func lighterRateFromTick(raw json.Number) (string, error) {
	tick, err := decimal.NewFromString(strings.TrimSpace(raw.String()))
	if err != nil {
		return "", fmt.Errorf("invalid fee tick %q", raw.String())
	}
	unit, _ := decimal.NewFromString(lighterFeeTick)
	return tick.Mul(unit).String(), nil
}
