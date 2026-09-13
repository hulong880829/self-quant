package portfolio

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/shopspring/decimal"
	"selfquant/backend/internal/exchange"
)

type asterAdapter struct {
	client *http.Client
	base   string
	now    func() time.Time
}

func newAster(client *http.Client, base string) Adapter {
	if base == "" {
		base = "https://fapi.asterdex.com"
	}
	return &asterAdapter{
		client: client,
		base:   strings.TrimRight(base, "/"),
		now:    time.Now,
	}
}

func (a *asterAdapter) Snapshot(ctx context.Context, credentials Credentials) (Snapshot, error) {
	user, signer, key, err := asterAuth(credentials)
	if err != nil {
		return Snapshot{}, err
	}

	var account struct {
		TotalWalletBalance flexDecimalString `json:"totalWalletBalance"`
		TotalMarginBalance flexDecimalString `json:"totalMarginBalance"`
		AvailableBalance   flexDecimalString `json:"availableBalance"`
		TotalMaintMargin   flexDecimalString `json:"totalMaintMargin"`
		Assets             []struct {
			Asset            string            `json:"asset"`
			WalletBalance    flexDecimalString `json:"walletBalance"`
			AvailableBalance flexDecimalString `json:"availableBalance"`
		} `json:"assets"`
	}
	if err := a.signedGet(ctx, "/fapi/v3/account", user, signer, key, &account); err != nil {
		return Snapshot{}, fmt.Errorf("aster account: %w", err)
	}

	var positions []struct {
		Symbol           string `json:"symbol"`
		PositionAmt      string `json:"positionAmt"`
		PositionSide     string `json:"positionSide"`
		EntryPrice       string `json:"entryPrice"`
		MarkPrice        string `json:"markPrice"`
		UnrealizedProfit string `json:"unRealizedProfit"`
		Notional         string `json:"notional"`
	}
	if err := a.signedGet(ctx, "/fapi/v3/positionRisk", user, signer, key, &positions); err != nil {
		return Snapshot{}, fmt.Errorf("aster positions: %w", err)
	}

	equity := walletDEXFirstMeaningful(string(account.TotalMarginBalance), string(account.TotalWalletBalance))
	available := strings.TrimSpace(string(account.AvailableBalance))
	assetEquity := decimal.Zero
	assetAvailable := decimal.Zero
	result := Snapshot{
		SpotBalances: make(map[string]string),
		UpdatedAt:    time.Now().UTC(),
	}
	for _, item := range account.Assets {
		balance, parseErr := walletDEXParseOptional(string(item.WalletBalance))
		if parseErr != nil {
			continue
		}
		setSpotBalance(result.SpotBalances, item.Asset, balance)
		if !walletDEXStableUSD(item.Asset) {
			continue
		}
		assetEquity = assetEquity.Add(balance)
		avail, availErr := walletDEXParseOptional(string(item.AvailableBalance))
		if availErr == nil {
			assetAvailable = assetAvailable.Add(avail)
		}
	}
	if walletDEXZeroOrEmpty(equity) {
		equity = assetEquity.String()
	}
	if walletDEXZeroOrEmpty(available) {
		available = assetAvailable.String()
	}
	result.AccountEquityUSD = equity
	result.AvailableFundsUSD = available
	result.RiskPercent = walletDEXRiskPercent(string(account.TotalMaintMargin), equity)
	for _, item := range positions {
		size, parseErr := parseDecimal(item.PositionAmt)
		if parseErr != nil || size.IsZero() {
			continue
		}
		side := sideFromSigned(size)
		if strings.EqualFold(item.PositionSide, "SHORT") {
			side = "short"
		} else if strings.EqualFold(item.PositionSide, "LONG") {
			side = "long"
		}
		notional, notionalErr := absoluteDecimal(item.Notional)
		if notionalErr != nil {
			mark, markErr := parseDecimal(item.MarkPrice)
			if markErr != nil {
				return Snapshot{}, fmt.Errorf("aster position %s: missing notional and mark", item.Symbol)
			}
			notional = size.Abs().Mul(mark.Abs())
		}
		mark := item.MarkPrice
		if walletDEXZeroOrEmpty(mark) && notional.IsPositive() && !size.IsZero() {
			mark = notional.Div(size.Abs()).String()
		}
		pnl, pnlErr := parseDecimal(item.UnrealizedProfit)
		if pnlErr != nil {
			return Snapshot{}, fmt.Errorf("aster position %s: invalid pnl", item.Symbol)
		}
		symbol := normalizeSymbol(item.Symbol)
		result.Positions = append(result.Positions, Position{
			Key: "aster:" + symbol + ":" + side, Kind: "cex", Exchange: "Aster",
			Symbol: symbol, Side: side, NotionalUSD: notional.String(), Size: size.Abs().String(),
			EntryPrice: item.EntryPrice, MarkPrice: mark, UnrealizedPnL: pnl.String(),
			WireSymbol: item.Symbol, ProductCategory: "perp",
		})
	}
	attachQuantities(&result)
	return result, nil
}

func (a *asterAdapter) signedGet(
	ctx context.Context,
	path, user, signer string,
	key *ecdsa.PrivateKey,
	target any,
) error {
	now := a.now()
	if now.IsZero() {
		now = time.Now()
	}
	params := map[string]string{
		"user":       user,
		"signer":     signer,
		"nonce":      strconv.FormatInt(now.UnixMicro(), 10),
		"timestamp":  strconv.FormatInt(now.UnixMilli(), 10),
		"recvWindow": "5000",
	}
	message := exchange.AsterParamString(params)
	signature, err := exchange.SignAsterV3(key, message)
	if err != nil {
		return err
	}
	rawURL := a.base + path + "?" + message + "&signature=" + signature
	return getJSON(ctx, a.client, rawURL, http.Header{"Accept": {"application/json"}}, target)
}

func asterAuth(credentials Credentials) (string, string, *ecdsa.PrivateKey, error) {
	user := strings.TrimSpace(credentials.APIKey)
	if !common.IsHexAddress(user) {
		return "", "", nil, fmt.Errorf("aster snapshot requires a wallet address")
	}
	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(credentials.APISecret), "0x"))
	if err != nil {
		return "", "", nil, fmt.Errorf("aster snapshot requires a private key")
	}
	return common.HexToAddress(user).Hex(), crypto.PubkeyToAddress(key.PublicKey).Hex(), key, nil
}

func asterUsesAPIWallet(credentials Credentials) bool {
	switch strings.TrimSpace(credentials.CredentialKind) {
	case "aster_hmac":
		return false
	case "aster_api_wallet", "":
		return common.IsHexAddress(strings.TrimSpace(credentials.APIKey))
	default:
		return false
	}
}

func (a *asterAdapter) AccountFeeRates(
	ctx context.Context,
	credentials Credentials,
) (AccountFeeRates, error) {
	var payload struct {
		MakerCommissionRate string `json:"makerCommissionRate"`
		TakerCommissionRate string `json:"takerCommissionRate"`
	}
	var err error
	if asterUsesAPIWallet(credentials) {
		user, signer, key, authErr := asterAuth(credentials)
		if authErr != nil {
			return AccountFeeRates{}, authErr
		}
		err = a.signedGetQuery(
			ctx, "/fapi/v3/commissionRate", user, signer, key,
			map[string]string{"symbol": "BTCUSDT"}, &payload,
		)
	} else {
		err = a.signedHMACGet(ctx, "/fapi/v1/commissionRate", url.Values{"symbol": {"BTCUSDT"}}, credentials, &payload)
	}
	if err != nil {
		return AccountFeeRates{}, fmt.Errorf("aster commission rate: %w", err)
	}
	contract, err := parseMarketFee(payload.MakerCommissionRate, payload.TakerCommissionRate)
	if err != nil {
		return AccountFeeRates{}, fmt.Errorf("aster commission rate: %w", err)
	}
	return AccountFeeRates{
		Source:   "venue",
		Spot:     unsupportedMarket(),
		Contract: contract,
	}, nil
}

func (a *asterAdapter) signedGetQuery(
	ctx context.Context,
	path, user, signer string,
	key *ecdsa.PrivateKey,
	extra map[string]string,
	target any,
) error {
	now := a.now()
	if now.IsZero() {
		now = time.Now()
	}
	params := map[string]string{
		"user":       user,
		"signer":     signer,
		"nonce":      strconv.FormatInt(now.UnixMicro(), 10),
		"timestamp":  strconv.FormatInt(now.UnixMilli(), 10),
		"recvWindow": "5000",
	}
	for name, value := range extra {
		params[name] = value
	}
	message := exchange.AsterParamString(params)
	signature, err := exchange.SignAsterV3(key, message)
	if err != nil {
		return err
	}
	rawURL := a.base + path + "?" + message + "&signature=" + signature
	return getJSON(ctx, a.client, rawURL, http.Header{"Accept": {"application/json"}}, target)
}

func (a *asterAdapter) signedHMACGet(
	ctx context.Context,
	path string,
	values url.Values,
	credentials Credentials,
	target any,
) error {
	if values == nil {
		values = url.Values{}
	}
	values.Set("timestamp", strconv.FormatInt(a.now().UnixMilli(), 10))
	values.Set("recvWindow", "5000")
	unsigned := values.Encode()
	rawURL := a.base + path + "?" + unsigned + "&signature=" + hmacHex256(credentials.APISecret, unsigned)
	return getJSON(ctx, a.client, rawURL, http.Header{"X-Mbx-Apikey": {credentials.APIKey}}, target)
}
