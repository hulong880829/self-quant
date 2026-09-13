package portfolio

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

type gateAdapter struct {
	client *http.Client
	base   string
}

func newGate(client *http.Client, base string) Adapter {
	if base == "" {
		base = "https://api.gateio.ws"
	}
	return &gateAdapter{client: client, base: strings.TrimRight(base, "/")}
}

func (a *gateAdapter) get(ctx context.Context, path string, credentials Credentials, target any) error {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	pathOnly, query := path, ""
	if parts := strings.SplitN(path, "?", 2); len(parts) == 2 {
		pathOnly, query = parts[0], parts[1]
	}
	emptyHash := sha512.Sum512(nil)
	payload := http.MethodGet + "\n" + pathOnly + "\n" + query + "\n" + hex.EncodeToString(emptyHash[:]) + "\n" + ts
	headers := http.Header{
		"Key": {credentials.APIKey}, "Timestamp": {ts},
		"Sign":                {hmacHex512(credentials.APISecret, payload)},
		"X-Gate-Size-Decimal": {"1"},
	}
	return getJSON(ctx, a.client, a.base+path, headers, target)
}

func (a *gateAdapter) Snapshot(ctx context.Context, credentials Credentials) (Snapshot, error) {
	var account struct {
		AvailableMargin        string `json:"available_margin"`
		TotalAvailableMargin   string `json:"total_available_margin"`
		TotalMarginBalance     string `json:"total_margin_balance"`
		UnifiedAccountEquity   string `json:"unified_account_total_equity"`
		TotalMaintenanceMargin string `json:"total_maintenance_margin"`
		Balances               map[string]struct {
			Equity    flexDecimalString `json:"equity"`
			Available flexDecimalString `json:"available"`
			Borrowed  flexDecimalString `json:"borrowed"`
			TotalLiab flexDecimalString `json:"total_liab"`
		} `json:"balances"`
	}
	if err := a.get(ctx, "/api/v4/unified/accounts", credentials, &account); err != nil {
		return Snapshot{}, fmt.Errorf("gate unified account: %w", err)
	}
	available := account.TotalAvailableMargin
	if available == "" || available == "0" {
		available = account.AvailableMargin
	}
	equity := account.TotalMarginBalance
	if equity == "" || equity == "0" {
		equity = account.UnifiedAccountEquity
	}
	result := Snapshot{
		AccountEquityUSD: equity, AvailableFundsUSD: available,
		RiskPercent:  riskPercent(account.TotalMaintenanceMargin, equity),
		SpotBalances: make(map[string]string), UpdatedAt: time.Now().UTC(),
	}
	for asset, balance := range account.Balances {
		net, netErr := parseDecimal(string(balance.Equity))
		if netErr != nil {
			net, netErr = parseDecimal(string(balance.Available))
			if netErr != nil {
				net = decimal.Zero
			}
			borrowed, borrowErr := parseDecimal(string(balance.Borrowed))
			if borrowErr != nil {
				borrowed, borrowErr = parseDecimal(string(balance.TotalLiab))
			}
			if borrowErr == nil {
				net = net.Sub(borrowed)
			}
		}
		setSpotBalance(result.SpotBalances, asset, net)
	}
	for _, settle := range []string{"usdt", "btc"} {
		var positions []struct {
			Contract      string            `json:"contract"`
			Size          flexDecimalString `json:"size"`
			EntryPrice    flexDecimalString `json:"entry_price"`
			MarkPrice     flexDecimalString `json:"mark_price"`
			UnrealisedPnl flexDecimalString `json:"unrealised_pnl"`
			Value         flexDecimalString `json:"value"`
		}
		path := "/api/v4/futures/" + settle + "/positions"
		if err := a.get(ctx, path, credentials, &positions); err != nil {
			if isGateMissingFuturesAccount(err) {
				continue
			}
			return Snapshot{}, fmt.Errorf("gate positions: %w", err)
		}
		for _, item := range positions {
			size, err := parseDecimal(string(item.Size))
			if err != nil || size.IsZero() {
				continue
			}
			notional, err := absoluteDecimal(string(item.Value))
			if err != nil {
				mark, markErr := absoluteDecimal(string(item.MarkPrice))
				if markErr != nil {
					continue
				}
				notional = size.Abs().Mul(mark)
			}
			side := sideFromSigned(size)
			symbol := normalizeSymbol(item.Contract)
			result.Positions = append(result.Positions, Position{
				Key: "gate:" + symbol + ":" + side, Kind: "cex", Exchange: "Gate", Symbol: symbol, Side: side,
				NotionalUSD: notional.String(), Size: size.Abs().String(), EntryPrice: string(item.EntryPrice),
				MarkPrice: string(item.MarkPrice), UnrealizedPnL: string(item.UnrealisedPnl),
				WireSymbol: item.Contract, ProductCategory: settle,
			})
		}
	}
	attachQuantities(&result)
	return result, nil
}

func isGateMissingFuturesAccount(err error) bool {
	var upstream *upstreamHTTPError
	if !errors.As(err, &upstream) || upstream.StatusCode != http.StatusBadRequest {
		return false
	}
	var payload struct {
		Label   string `json:"label"`
		Message string `json:"message"`
	}
	if json.Unmarshal(upstream.Body, &payload) != nil {
		return false
	}
	return payload.Label == "USER_NOT_FOUND" &&
		strings.Contains(strings.ToLower(payload.Message), "create futures account")
}

func (a *gateAdapter) AccountFeeRates(
	ctx context.Context,
	credentials Credentials,
) (AccountFeeRates, error) {
	var payload struct {
		MakerFee        string `json:"maker_fee"`
		TakerFee        string `json:"taker_fee"`
		GTDiscount      bool   `json:"gt_discount"`
		GTMakerFee      string `json:"gt_maker_fee"`
		GTTakerFee      string `json:"gt_taker_fee"`
		FuturesMakerFee string `json:"futures_maker_fee"`
		FuturesTakerFee string `json:"futures_taker_fee"`
	}
	if err := a.get(ctx, "/api/v4/wallet/fee?currency_pair=BTC_USDT&settle=usdt", credentials, &payload); err != nil {
		return AccountFeeRates{}, fmt.Errorf("gate wallet fee: %w", err)
	}
	maker, taker := payload.MakerFee, payload.TakerFee
	if payload.GTDiscount {
		maker = firstNonEmptyFee(payload.GTMakerFee, payload.MakerFee)
		taker = firstNonEmptyFee(payload.GTTakerFee, payload.TakerFee)
	}
	spot, err := parseMarketFee(maker, taker)
	if err != nil {
		return AccountFeeRates{}, fmt.Errorf("gate spot fee: %w", err)
	}
	contract, err := parseMarketFee(payload.FuturesMakerFee, payload.FuturesTakerFee)
	if err != nil {
		return AccountFeeRates{}, fmt.Errorf("gate contract fee: %w", err)
	}
	return AccountFeeRates{Source: "venue", Spot: spot, Contract: contract}, nil
}
