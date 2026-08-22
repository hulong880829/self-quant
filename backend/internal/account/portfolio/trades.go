package portfolio

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const maximumTradePages = 20

func normalizedFill(
	id, orderID, symbol, side, price, quantity, quote, fee, feeCurrency string,
	tradedAt time.Time,
) (TradeFill, bool) {
	priceValue, priceErr := decimal.NewFromString(strings.TrimSpace(price))
	quantityValue, quantityErr := decimal.NewFromString(strings.TrimSpace(quantity))
	if strings.TrimSpace(id) == "" || priceErr != nil || quantityErr != nil ||
		!priceValue.IsPositive() || quantityValue.IsZero() || tradedAt.IsZero() {
		return TradeFill{}, false
	}
	quantityValue = quantityValue.Abs()
	quoteValue, quoteErr := decimal.NewFromString(strings.TrimSpace(quote))
	if quoteErr != nil || quoteValue.IsZero() {
		quoteValue = priceValue.Mul(quantityValue)
	}
	return TradeFill{
		ExternalTradeID: id, OrderID: orderID, Symbol: normalizeSymbol(symbol),
		Side: strings.ToLower(strings.TrimSpace(side)), Price: priceValue.String(),
		Quantity: quantityValue.String(), QuoteNotionalUSD: quoteValue.Abs().String(),
		Fee: strings.TrimSpace(fee), FeeCurrency: strings.ToUpper(strings.TrimSpace(feeCurrency)),
		TradedAt: tradedAt.UTC(),
	}, true
}

func queryWindow(query TradeQuery) (int64, int64) {
	until := query.Until
	if until.IsZero() {
		until = time.Now()
	}
	since := query.Since
	if since.IsZero() {
		since = until.Add(-24 * time.Hour)
	}
	return since.UnixMilli(), until.UnixMilli()
}

func (a *binanceAdapter) TradeFills(
	ctx context.Context,
	credentials Credentials,
	query TradeQuery,
) ([]TradeFill, error) {
	start, end := queryWindow(query)
	result := make([]TradeFill, 0)
	for _, endpoint := range []string{
		"/papi/v1/margin/myTrades",
		"/papi/v1/um/userTrades",
		"/papi/v1/cm/userTrades",
	} {
		fromID := ""
		scope := strings.TrimPrefix(strings.TrimSuffix(endpoint, "/userTrades"), "/papi/v1/")
		scope = strings.TrimSuffix(scope, "/myTrades")
		for page := 0; page < maximumTradePages; page++ {
			var items []struct {
				ID, OrderID                  int64
				Symbol, Price, Qty, QuoteQty string
				Commission, CommissionAsset  string
				Time                         int64
				IsBuyer                      bool
				Buyer                        bool
				Side                         string
			}
			path := endpoint + "?startTime=" + strconv.FormatInt(start, 10) +
				"&endTime=" + strconv.FormatInt(end, 10) + "&limit=1000"
			if fromID != "" {
				path += "&fromId=" + fromID
			}
			if err := a.signedGetPath(ctx, path, credentials, &items); err != nil {
				return nil, fmt.Errorf("binance trades %s: %w", endpoint, err)
			}
			for _, item := range items {
				side := strings.ToLower(item.Side)
				if side == "" {
					side = "sell"
					if item.IsBuyer || item.Buyer {
						side = "buy"
					}
				}
				fill, ok := normalizedFill(
					scope+":"+strconv.FormatInt(item.ID, 10), strconv.FormatInt(item.OrderID, 10),
					item.Symbol, side, item.Price, item.Qty, item.QuoteQty,
					item.Commission, item.CommissionAsset, time.UnixMilli(item.Time),
				)
				if ok {
					result = append(result, fill)
				}
			}
			if len(items) < 1000 {
				break
			}
			fromID = strconv.FormatInt(items[len(items)-1].ID+1, 10)
		}
	}
	return result, nil
}

func (a *binanceAdapter) signedGetPath(
	ctx context.Context,
	path string,
	credentials Credentials,
	target any,
) error {
	parts := strings.SplitN(path, "?", 2)
	query := url.Values{}
	if len(parts) == 2 {
		parsed, err := url.ParseQuery(parts[1])
		if err != nil {
			return err
		}
		query = parsed
	}
	query.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	query.Set("recvWindow", "5000")
	unsigned := query.Encode()
	rawURL := a.base + parts[0] + "?" + unsigned + "&signature=" +
		hmacHex256(credentials.APISecret, unsigned)
	return getJSON(ctx, a.client, rawURL,
		mapHeader("X-MBX-APIKEY", credentials.APIKey), target)
}

func (a *okxAdapter) TradeFills(
	ctx context.Context,
	credentials Credentials,
	query TradeQuery,
) ([]TradeFill, error) {
	start, end := queryWindow(query)
	result := make([]TradeFill, 0)
	for _, instType := range []string{"SPOT", "MARGIN", "SWAP", "FUTURES", "OPTION"} {
		after := ""
		for page := 0; page < maximumTradePages; page++ {
			values := url.Values{
				"instType": {instType}, "begin": {strconv.FormatInt(start, 10)},
				"end": {strconv.FormatInt(end, 10)}, "limit": {"100"},
			}
			if after != "" {
				values.Set("after", after)
			}
			var response struct {
				Code, Msg string
				Data      []struct {
					TradeID, OrdID, InstID, Side, FillPx, FillSz string
					FillFee, FillFeeCcy, Ts                      string
				}
			}
			path := "/api/v5/trade/fills-history?" + values.Encode()
			if err := a.get(ctx, path, credentials, &response); err != nil {
				return nil, fmt.Errorf("okx fills: %w", err)
			}
			if response.Code != "0" {
				return nil, fmt.Errorf("okx fills rejected: %s", response.Msg)
			}
			for _, item := range response.Data {
				millis, _ := strconv.ParseInt(item.Ts, 10, 64)
				fill, ok := normalizedFill(
					strings.ToLower(instType)+":"+item.TradeID,
					item.OrdID, item.InstID, item.Side, item.FillPx,
					item.FillSz, "", item.FillFee, item.FillFeeCcy, time.UnixMilli(millis),
				)
				if ok {
					result = append(result, fill)
				}
			}
			if len(response.Data) < 100 {
				break
			}
			next := response.Data[len(response.Data)-1].TradeID
			if next == "" || next == after {
				break
			}
			after = next
		}
	}
	return result, nil
}

func (a *bitgetAdapter) TradeFills(
	ctx context.Context,
	credentials Credentials,
	query TradeQuery,
) ([]TradeFill, error) {
	start, end := queryWindow(query)
	result := make([]TradeFill, 0)
	for _, category := range []string{"SPOT", "USDT-FUTURES", "USDC-FUTURES", "COIN-FUTURES"} {
		cursor := ""
		for page := 0; page < maximumTradePages; page++ {
			values := url.Values{
				"category": {category}, "startTime": {strconv.FormatInt(start, 10)},
				"endTime": {strconv.FormatInt(end, 10)}, "limit": {"100"},
			}
			if cursor != "" {
				values.Set("cursor", cursor)
			}
			var response struct {
				Code, Msg string
				Data      struct {
					EndID, NextCursor string
					List              []struct {
						TradeID, OrderID, Symbol, Side, Price, Size string
						Amount, Fee, FeeCoin, CTime                 string
					}
				}
			}
			path := "/api/v3/trade/fills?" + values.Encode()
			if err := a.get(ctx, path, credentials, &response); err != nil {
				return nil, fmt.Errorf("bitget fills: %w", err)
			}
			if response.Code != "00000" {
				return nil, fmt.Errorf("bitget fills rejected: %s", response.Msg)
			}
			for _, item := range response.Data.List {
				millis, _ := strconv.ParseInt(item.CTime, 10, 64)
				fill, ok := normalizedFill(
					strings.ToLower(category)+":"+item.TradeID,
					item.OrderID, item.Symbol, item.Side, item.Price,
					item.Size, item.Amount, item.Fee, item.FeeCoin, time.UnixMilli(millis),
				)
				if ok {
					result = append(result, fill)
				}
			}
			next := response.Data.NextCursor
			if next == "" {
				next = response.Data.EndID
			}
			if len(response.Data.List) == 0 || next == "" || next == cursor {
				break
			}
			cursor = next
		}
	}
	return result, nil
}

func (a *bybitAdapter) TradeFills(
	ctx context.Context,
	credentials Credentials,
	query TradeQuery,
) ([]TradeFill, error) {
	start, end := queryWindow(query)
	result := make([]TradeFill, 0)
	for _, category := range []string{"spot", "linear", "inverse", "option"} {
		cursor := ""
		for page := 0; page < maximumTradePages; page++ {
			values := url.Values{
				"category": {category}, "startTime": {strconv.FormatInt(start, 10)},
				"endTime": {strconv.FormatInt(end, 10)}, "limit": {"100"},
			}
			if cursor != "" {
				values.Set("cursor", cursor)
			}
			var response struct {
				RetCode int
				RetMsg  string
				Result  struct {
					NextPageCursor string
					List           []struct {
						ExecID, OrderID, Symbol, Side, ExecPrice, ExecQty string
						ExecValue, ExecFee, FeeCurrency, ExecTime         string
					}
				}
			}
			path := "/v5/execution/list?" + values.Encode()
			if err := a.get(ctx, path, credentials, &response); err != nil {
				return nil, fmt.Errorf("bybit executions: %w", err)
			}
			if response.RetCode != 0 {
				return nil, fmt.Errorf("bybit executions rejected: %s", response.RetMsg)
			}
			for _, item := range response.Result.List {
				millis, _ := strconv.ParseInt(item.ExecTime, 10, 64)
				fill, ok := normalizedFill(
					category+":"+item.ExecID, item.OrderID, item.Symbol, item.Side, item.ExecPrice,
					item.ExecQty, item.ExecValue, item.ExecFee, item.FeeCurrency,
					time.UnixMilli(millis),
				)
				if ok {
					result = append(result, fill)
				}
			}
			next := response.Result.NextPageCursor
			if len(response.Result.List) == 0 || next == "" || next == cursor {
				break
			}
			cursor = next
		}
	}
	return result, nil
}

func (a *gateAdapter) TradeFills(
	ctx context.Context,
	credentials Credentials,
	query TradeQuery,
) ([]TradeFill, error) {
	startMillis, endMillis := queryWindow(query)
	start, end := startMillis/1000, endMillis/1000
	result := make([]TradeFill, 0)
	type gateTrade struct {
		ID           string `json:"id"`
		OrderID      string `json:"order_id"`
		CurrencyPair string `json:"currency_pair"`
		Contract     string `json:"contract"`
		Side         string `json:"side"`
		Price        string `json:"price"`
		Amount       string `json:"amount"`
		Size         string `json:"size"`
		Fee          string `json:"fee"`
		FeeCurrency  string `json:"fee_currency"`
		CreateTimeMs string `json:"create_time_ms"`
		CreateTime   string `json:"create_time"`
	}
	targets := []string{"/api/v4/spot/my_trades"}
	for _, settle := range []string{"usdt", "btc"} {
		targets = append(targets, "/api/v4/futures/"+settle+"/my_trades_timerange")
	}
	for _, endpoint := range targets {
		for page := 1; page <= maximumTradePages; page++ {
			values := url.Values{
				"from": {strconv.FormatInt(start, 10)}, "to": {strconv.FormatInt(end, 10)},
				"limit": {"1000"}, "page": {strconv.Itoa(page)},
			}
			var items []gateTrade
			if err := a.get(ctx, endpoint+"?"+values.Encode(), credentials, &items); err != nil {
				if isGateMissingFuturesAccount(err) {
					break
				}
				return nil, fmt.Errorf("gate fills %s: %w", endpoint, err)
			}
			for _, item := range items {
				timestamp := item.CreateTimeMs
				if timestamp == "" {
					timestamp = item.CreateTime
				}
				parsed, _ := decimal.NewFromString(timestamp)
				millis := parsed.Mul(decimal.NewFromInt(1000)).IntPart()
				if strings.Contains(timestamp, ".") {
					millis = parsed.Mul(decimal.NewFromInt(1000)).IntPart()
				} else if parsed.GreaterThan(decimal.NewFromInt(1_000_000_000_000)) {
					millis = parsed.IntPart()
				}
				symbol, quantity := item.CurrencyPair, item.Amount
				if symbol == "" {
					symbol, quantity = item.Contract, item.Size
				}
				fill, ok := normalizedFill(
					strings.TrimPrefix(endpoint, "/api/v4/")+":"+item.ID,
					item.OrderID, symbol, item.Side, item.Price, quantity,
					"", item.Fee, item.FeeCurrency, time.UnixMilli(millis),
				)
				if ok {
					result = append(result, fill)
				}
			}
			if len(items) < 1000 {
				break
			}
		}
	}
	return result, nil
}

func mapHeader(key, value string) map[string][]string {
	return map[string][]string{key: {value}}
}
