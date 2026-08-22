package polymarket

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	ethmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/shopspring/decimal"
)

const (
	ctfExchangeAddress     = "0xE111180000d2663C0091e4f400237545B87B996B"
	negRiskExchangeAddress = "0xe2222d279d744050d28e00520010520000310F59"
	zeroBytes32            = "0x0000000000000000000000000000000000000000000000000000000000000000"
	v2OrderType            = "Order(uint256 salt,address maker,address signer,uint256 tokenId,uint256 makerAmount,uint256 takerAmount,uint8 side,uint8 signatureType,uint256 timestamp,bytes32 metadata,bytes32 builder)"
)

type Credentials struct {
	SignerAddress string
	FunderAddress string
	PrivateKey    string
	APIKey        string
	APISecret     string
	Passphrase    string
	SignatureType int32
}

type CLOBClient struct {
	baseURL string
	http    *http.Client
	now     func() time.Time
	timeout time.Duration
}

type CLOBError struct {
	StatusCode int
	Code       string
	Message    string
}

type TransportError struct {
	Method         string
	RequestWritten bool
	Err            error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("CLOB %s transport failed: %v", e.Method, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

func (e *CLOBError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("CLOB returned %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("CLOB returned %d", e.StatusCode)
}

func NewCLOBClient(baseURL string, timeout time.Duration) *CLOBClient {
	connectTimeout := timeout / 3
	if connectTimeout <= 0 || connectTimeout > 3*time.Second {
		connectTimeout = 3 * time.Second
	}
	headerTimeout := timeout * 3 / 4
	if headerTimeout <= 0 {
		headerTimeout = timeout
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout: connectTimeout, KeepAlive: 30 * time.Second,
	}).DialContext
	transport.ResponseHeaderTimeout = headerTimeout
	transport.TLSHandshakeTimeout = connectTimeout
	return &CLOBClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout, Transport: transport},
		now:     time.Now,
		timeout: timeout,
	}
}

type bookLevel struct {
	Price string `json:"price"`
	Size  string `json:"size"`
}

type bookResponse struct {
	Bids         []bookLevel `json:"bids"`
	Asks         []bookLevel `json:"asks"`
	MinOrderSize string      `json:"min_order_size"`
	TickSize     string      `json:"tick_size"`
	NegativeRisk bool        `json:"neg_risk"`
}

func (c *CLOBClient) BestQuotes(ctx context.Context, tokenID string) (string, string, error) {
	book, err := c.orderBook(ctx, tokenID)
	if err != nil {
		return "", "", err
	}
	bid, ask := bestLevel(book.Bids, true), bestLevel(book.Asks, false)
	return bid, ask, nil
}

func (c *CLOBClient) orderBook(ctx context.Context, tokenID string) (bookResponse, error) {
	var book bookResponse
	path := "/book?token_id=" + url.QueryEscape(tokenID)
	if _, err := c.request(ctx, http.MethodGet, path, nil, nil, &book); err != nil {
		return bookResponse{}, err
	}
	return book, nil
}

func bestLevel(levels []bookLevel, highest bool) string {
	var best decimal.Decimal
	found := false
	for _, level := range levels {
		price, err := decimal.NewFromString(level.Price)
		if err != nil || !price.GreaterThan(decimal.Zero) {
			continue
		}
		if !found || highest && price.GreaterThan(best) || !highest && price.LessThan(best) {
			best, found = price, true
		}
	}
	if !found {
		return ""
	}
	return best.String()
}

func (c *CLOBClient) CollateralBalance(
	ctx context.Context,
	credentials Credentials,
) (string, error) {
	var response struct {
		Balance string `json:"balance"`
	}
	query := url.Values{
		"asset_type":     {"COLLATERAL"},
		"signature_type": {strconv.FormatInt(int64(credentials.SignatureType), 10)},
	}
	requestPath := "/balance-allowance?" + query.Encode()
	if _, err := c.request(ctx, http.MethodGet, requestPath, nil, &credentials, &response); err != nil {
		return "", err
	}
	if response.Balance == "" {
		return "0", nil
	}
	raw, err := decimal.NewFromString(response.Balance)
	if err != nil {
		return "", err
	}
	// CLOB V2 collateral balances are pUSD wei (6 decimals).
	return raw.Shift(-6).String(), nil
}

type OrderSubmission struct {
	OrderID      string
	OrderType    string
	Status       string
	FilledSize   string
	AveragePrice string
	ErrorCode    string
}

type CancelResult struct {
	OrderID string
	Status  string
	Message string
}

func (c *CLOBClient) CancelOrder(
	ctx context.Context,
	credentials Credentials,
	orderID string,
) (CancelResult, error) {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return CancelResult{}, errors.New("order ID is required")
	}
	payload := struct {
		OrderID string `json:"orderID"`
	}{OrderID: orderID}
	var response struct {
		Canceled    []string          `json:"canceled"`
		NotCanceled map[string]string `json:"not_canceled"`
	}
	if _, err := c.request(
		ctx, http.MethodDelete, "/order", payload, &credentials, &response,
	); err != nil {
		return CancelResult{}, err
	}
	for _, canceled := range response.Canceled {
		if canceled == orderID {
			return CancelResult{OrderID: orderID, Status: "canceled"}, nil
		}
	}
	if message := strings.TrimSpace(response.NotCanceled[orderID]); message != "" {
		return CancelResult{OrderID: orderID, Status: "not_canceled", Message: message},
			fmt.Errorf("cancel order: %s", message)
	}
	return CancelResult{OrderID: orderID, Status: "not_canceled"},
		errors.New("cancel order was not acknowledged by CLOB")
}

type OpenOrder struct {
	ID            string
	ConditionID   string
	TokenID       string
	MarketTitle   string
	Outcome       string
	Side          string
	Price         string
	OriginalSize  string
	MatchedSize   string
	RemainingSize string
	Status        string
	OrderType     string
	CreatedAt     time.Time
}

type Trade struct {
	ID, OrderID, Market, AssetID, Side string
	Price, Size, Fee                   string
	MatchedAt                          time.Time
}

type tradeItem struct {
	ID        string `json:"id"`
	TradeID   string `json:"trade_id"`
	OrderID   string `json:"order_id"`
	Market    string `json:"market"`
	AssetID   string `json:"asset_id"`
	Side      string `json:"side"`
	Price     string `json:"price"`
	Size      string `json:"size"`
	Fee       string `json:"fee"`
	MatchTime string `json:"match_time"`
	Timestamp int64  `json:"timestamp"`
}

type tradesResponse struct {
	NextCursor string
	Data       []tradeItem
}

func (r *tradesResponse) UnmarshalJSON(body []byte) error {
	var items []tradeItem
	if err := json.Unmarshal(body, &items); err == nil {
		r.Data = items
		return nil
	}
	var envelope struct {
		NextCursor string      `json:"next_cursor"`
		Data       []tradeItem `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	r.NextCursor, r.Data = envelope.NextCursor, envelope.Data
	return nil
}

func (c *CLOBClient) ListTrades(
	ctx context.Context,
	credentials Credentials,
	since, until time.Time,
) ([]Trade, error) {
	result := make([]Trade, 0)
	cursor := ""
	seen := make(map[string]struct{})
	for page := 0; page < 20; page++ {
		query := url.Values{}
		if cursor != "" {
			query.Set("next_cursor", cursor)
		}
		path := "/data/trades"
		if encoded := query.Encode(); encoded != "" {
			path += "?" + encoded
		}
		var response tradesResponse
		if _, err := c.request(ctx, http.MethodGet, path, nil, &credentials, &response); err != nil {
			return nil, err
		}
		for _, item := range response.Data {
			id := strings.TrimSpace(item.TradeID)
			if id == "" {
				id = strings.TrimSpace(item.ID)
			}
			matchedAt := time.Unix(item.Timestamp, 0).UTC()
			if parsed, err := time.Parse(time.RFC3339Nano, item.MatchTime); err == nil {
				matchedAt = parsed.UTC()
			}
			if id == "" || matchedAt.Before(since) || matchedAt.After(until) {
				continue
			}
			result = append(result, Trade{
				ID: id, OrderID: item.OrderID, Market: item.Market, AssetID: item.AssetID,
				Side: strings.ToLower(item.Side), Price: item.Price, Size: item.Size,
				Fee: item.Fee, MatchedAt: matchedAt,
			})
		}
		next := strings.TrimSpace(response.NextCursor)
		if next == "" || next == "LTE=" || len(response.Data) == 0 {
			break
		}
		if _, duplicate := seen[next]; duplicate {
			break
		}
		seen[next], cursor = struct{}{}, next
	}
	return result, nil
}

type openOrderItem struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	Market       string `json:"market"`
	AssetID      string `json:"asset_id"`
	Side         string `json:"side"`
	OriginalSize string `json:"original_size"`
	SizeMatched  string `json:"size_matched"`
	Price        string `json:"price"`
	Outcome      string `json:"outcome"`
	OrderType    string `json:"order_type"`
	CreatedAt    int64  `json:"created_at"`
}

type openOrdersResponse struct {
	NextCursor string
	Data       []openOrderItem
}

func (r *openOrdersResponse) UnmarshalJSON(body []byte) error {
	var items []openOrderItem
	if err := json.Unmarshal(body, &items); err == nil {
		r.Data = items
		r.NextCursor = ""
		return nil
	}
	var envelope struct {
		NextCursor string          `json:"next_cursor"`
		Data       []openOrderItem `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	r.NextCursor = envelope.NextCursor
	r.Data = envelope.Data
	return nil
}

func (c *CLOBClient) ListOpenOrders(
	ctx context.Context,
	credentials Credentials,
) ([]OpenOrder, error) {
	const maximumOrders = 500
	result := make([]OpenOrder, 0)
	cursor := ""
	seenCursors := make(map[string]struct{})
	for len(result) < maximumOrders {
		query := url.Values{}
		if cursor != "" {
			query.Set("next_cursor", cursor)
		}
		path := "/data/orders"
		if encoded := query.Encode(); encoded != "" {
			path += "?" + encoded
		}
		var response openOrdersResponse
		if _, err := c.request(
			ctx, http.MethodGet, path, nil, &credentials, &response,
		); err != nil {
			return nil, err
		}
		for _, item := range response.Data {
			status := strings.ToUpper(strings.TrimSpace(item.Status))
			if status != "ORDER_STATUS_LIVE" && status != "LIVE" {
				continue
			}
			original, originalErr := decimal.NewFromString(item.OriginalSize)
			matched, matchedErr := decimal.NewFromString(item.SizeMatched)
			if originalErr != nil || matchedErr != nil {
				continue
			}
			original = original.Shift(-6)
			matched = matched.Shift(-6)
			remaining := original.Sub(matched)
			if !remaining.GreaterThan(decimal.Zero) {
				continue
			}
			result = append(result, OpenOrder{
				ID: item.ID, ConditionID: item.Market, TokenID: item.AssetID,
				Outcome: strings.ToLower(item.Outcome),
				Side:    strings.ToLower(item.Side), Price: item.Price,
				OriginalSize:  original.String(),
				MatchedSize:   matched.String(),
				RemainingSize: remaining.String(),
				Status:        "open", OrderType: strings.ToUpper(item.OrderType),
				CreatedAt: time.Unix(item.CreatedAt, 0).UTC(),
			})
			if len(result) >= maximumOrders {
				break
			}
		}
		next := strings.TrimSpace(response.NextCursor)
		if next == "" || next == "LTE=" || len(response.Data) == 0 {
			break
		}
		if _, duplicate := seenCursors[next]; duplicate {
			break
		}
		seenCursors[next] = struct{}{}
		cursor = next
	}
	return result, nil
}

func (c *CLOBClient) PlaceOrder(
	ctx context.Context,
	credentials Credentials,
	market Market,
	tokenID, side, amount, amountUnit, executionType, limitPrice string,
) (OrderSubmission, error) {
	book, err := c.orderBook(ctx, tokenID)
	if err != nil {
		return OrderSubmission{}, err
	}
	priceValue := strings.TrimSpace(limitPrice)
	orderType := "GTC"
	if executionType == "book" {
		orderType = "FAK"
		priceValue = bestLevel(book.Asks, false)
		if side == "sell" {
			priceValue = bestLevel(book.Bids, true)
		}
	}
	if priceValue == "" {
		return OrderSubmission{}, errors.New("order book has no executable liquidity")
	}
	price, err := decimal.NewFromString(priceValue)
	if err != nil || price.LessThanOrEqual(decimal.Zero) ||
		!price.LessThan(decimal.NewFromInt(1)) {
		return OrderSubmission{}, errors.New("invalid executable price")
	}
	tickSize := strings.TrimSpace(book.TickSize)
	if tickSize == "" {
		tickSize = market.TickSize
	}
	tick, err := decimal.NewFromString(tickSize)
	if err != nil || !tick.GreaterThan(decimal.Zero) || !price.Mod(tick).IsZero() {
		return OrderSubmission{}, errors.New("price does not conform to tick size")
	}
	requested, err := decimal.NewFromString(amount)
	if err != nil || requested.LessThanOrEqual(decimal.Zero) {
		return OrderSubmission{}, errors.New("invalid order amount")
	}
	amountDecimals, ok := amountPrecision(tick.String())
	if !ok {
		return OrderSubmission{}, errors.New("unsupported tick size")
	}
	var shares, dollars decimal.Decimal
	if executionType == "limit" {
		if amountUnit != "shares" {
			return OrderSubmission{}, errors.New("limit orders require shares")
		}
		shares = requested.Truncate(2)
		if !shares.Equal(requested) {
			return OrderSubmission{}, errors.New("share amount supports at most 2 decimals")
		}
		dollars = roundedQuoteAmount(shares.Mul(price), amountDecimals)
	} else if side == "buy" {
		if amountUnit != "usd" {
			return OrderSubmission{}, errors.New("book buy orders require usd")
		}
		dollars = requested.Truncate(int32(amountDecimals))
		if !dollars.Equal(requested) {
			return OrderSubmission{}, errors.New("usd amount has excessive precision")
		}
		shares = requested.Div(price).
			RoundCeil(int32(amountDecimals + 4)).
			RoundCeil(int32(amountDecimals))
	} else {
		if amountUnit != "shares" {
			return OrderSubmission{}, errors.New("book sell orders require shares")
		}
		shares = requested.Truncate(2)
		if !shares.Equal(requested) {
			return OrderSubmission{}, errors.New("share amount supports at most 2 decimals")
		}
		dollars = roundedQuoteAmount(shares.Mul(price), amountDecimals)
	}
	if minimum, minimumErr := decimal.NewFromString(book.MinOrderSize); minimumErr == nil &&
		shares.LessThan(minimum) {
		return OrderSubmission{}, fmt.Errorf(
			"order size %s is below minimum %s", shares.String(), minimum.String(),
		)
	}
	makerAmount, takerAmount := dollars.Shift(6).Truncate(0), shares.Shift(6).Truncate(0)
	sideNumber := 0
	if side == "sell" {
		sideNumber = 1
		makerAmount, takerAmount = takerAmount, makerAmount
	}
	// The V2 wire schema requires salt to be a JSON number. Keep it within
	// JavaScript's safe-integer range because the CLOB JSON validator parses
	// numeric fields before converting them to uint256 for signature checks.
	saltValue, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 53))
	if err != nil {
		return OrderSubmission{}, err
	}
	if saltValue.Sign() == 0 {
		saltValue.SetInt64(1)
	}
	salt := saltValue.String()
	timestamp := strconv.FormatInt(c.now().UnixMilli(), 10)
	orderSigner := credentials.SignerAddress
	if credentials.SignatureType == 3 {
		orderSigner = credentials.FunderAddress
	}
	order := map[string]any{
		"salt":          salt,
		"maker":         credentials.FunderAddress,
		"signer":        orderSigner,
		"tokenId":       tokenID,
		"makerAmount":   makerAmount.StringFixed(0),
		"takerAmount":   takerAmount.StringFixed(0),
		"side":          strconv.Itoa(sideNumber),
		"signatureType": strconv.FormatInt(int64(credentials.SignatureType), 10),
		"timestamp":     timestamp,
		"metadata":      zeroBytes32,
		"builder":       zeroBytes32,
	}
	signature, err := signOrder(
		credentials.PrivateKey,
		market.NegativeRisk || book.NegativeRisk,
		credentials.SignatureType,
		order,
	)
	if err != nil {
		return OrderSubmission{}, err
	}
	wireOrder := make(map[string]any, len(order)+1)
	for key, value := range order {
		wireOrder[key] = value
	}
	wireOrder["salt"] = json.Number(salt)
	if sideNumber == 0 {
		wireOrder["side"] = "BUY"
	} else {
		wireOrder["side"] = "SELL"
	}
	wireOrder["signatureType"] = credentials.SignatureType
	wireOrder["signature"] = signature
	wireOrder["expiration"] = "0"
	payload := map[string]any{
		"order": wireOrder, "owner": credentials.APIKey, "orderType": orderType,
		"deferExec": false, "postOnly": false,
	}
	var response struct {
		Success      bool   `json:"success"`
		OrderID      string `json:"orderID"`
		Status       string `json:"status"`
		ErrorMessage string `json:"errorMsg"`
		MakingAmount string `json:"makingAmount"`
		TakingAmount string `json:"takingAmount"`
	}
	if _, err := c.request(ctx, http.MethodPost, "/order", payload, &credentials, &response); err != nil {
		return OrderSubmission{}, err
	}
	if !response.Success && response.OrderID == "" {
		return OrderSubmission{
			OrderType: orderType, Status: "rejected", ErrorCode: response.ErrorMessage,
		}, nil
	}
	status := normalizeOrderStatus(response.Status)
	filledSize := response.TakingAmount
	if side == "sell" {
		filledSize = response.MakingAmount
	}
	if value, parseErr := decimal.NewFromString(filledSize); parseErr == nil {
		filledSize = value.Shift(-6).String()
	}
	return OrderSubmission{
		OrderID: response.OrderID, OrderType: orderType, Status: status, FilledSize: filledSize,
		AveragePrice: price.String(), ErrorCode: response.ErrorMessage,
	}, nil
}

func amountPrecision(tickSize string) (int, bool) {
	switch tickSize {
	case "0.1":
		return 3, true
	case "0.01":
		return 4, true
	case "0.005", "0.001":
		return 5, true
	case "0.0025", "0.0001":
		return 6, true
	default:
		return 0, false
	}
}

func roundedQuoteAmount(value decimal.Decimal, places int) decimal.Decimal {
	return value.
		RoundCeil(int32(places + 4)).
		Truncate(int32(places))
}

func signOrder(
	privateKey string,
	negativeRisk bool,
	signatureType int32,
	order map[string]any,
) (string, error) {
	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(privateKey), "0x"))
	if err != nil {
		return "", errors.New("invalid order signing key")
	}
	exchange := ctfExchangeAddress
	if negativeRisk {
		exchange = negRiskExchangeAddress
	}
	types := apitypes.Types{
		"EIP712Domain": {
			{Name: "name", Type: "string"}, {Name: "version", Type: "string"},
			{Name: "chainId", Type: "uint256"}, {Name: "verifyingContract", Type: "address"},
		},
		"Order": {
			{Name: "salt", Type: "uint256"}, {Name: "maker", Type: "address"},
			{Name: "signer", Type: "address"}, {Name: "tokenId", Type: "uint256"},
			{Name: "makerAmount", Type: "uint256"}, {Name: "takerAmount", Type: "uint256"},
			{Name: "side", Type: "uint8"}, {Name: "signatureType", Type: "uint8"},
			{Name: "timestamp", Type: "uint256"}, {Name: "metadata", Type: "bytes32"},
			{Name: "builder", Type: "bytes32"},
		},
	}
	domain := apitypes.TypedDataDomain{
		Name: "Polymarket CTF Exchange", Version: "2",
		ChainId:           ethmath.NewHexOrDecimal256(137),
		VerifyingContract: common.HexToAddress(exchange).Hex(),
	}
	typed := apitypes.TypedData{
		Types:       types,
		PrimaryType: "Order",
		Domain:      domain,
		Message:     order,
	}
	if signatureType == 3 {
		typed.Types["TypedDataSign"] = []apitypes.Type{
			{Name: "contents", Type: "Order"},
			{Name: "name", Type: "string"},
			{Name: "version", Type: "string"},
			{Name: "chainId", Type: "uint256"},
			{Name: "verifyingContract", Type: "address"},
			{Name: "salt", Type: "bytes32"},
		}
		typed.PrimaryType = "TypedDataSign"
		typed.Message = apitypes.TypedDataMessage{
			"contents":          order,
			"name":              "DepositWallet",
			"version":           "1",
			"chainId":           "137",
			"verifyingContract": order["maker"],
			"salt":              zeroBytes32,
		}
	}
	hash, _, err := apitypes.TypedDataAndHash(typed)
	if err != nil {
		return "", fmt.Errorf("hash CLOB order: %w", err)
	}
	signature, err := crypto.Sign(hash, key)
	if err != nil {
		return "", err
	}
	signature[64] += 27
	if signatureType != 3 {
		return "0x" + hex.EncodeToString(signature), nil
	}
	exchangeTyped := apitypes.TypedData{
		Types: types, PrimaryType: "Order", Domain: domain, Message: order,
	}
	domainHash, err := exchangeTyped.HashStruct("EIP712Domain", apitypes.TypedDataMessage{
		"name": domain.Name, "version": domain.Version,
		"chainId": domain.ChainId, "verifyingContract": domain.VerifyingContract,
	})
	if err != nil {
		return "", fmt.Errorf("hash deposit domain: %w", err)
	}
	contentsHash, err := exchangeTyped.HashStruct("Order", order)
	if err != nil {
		return "", fmt.Errorf("hash deposit order: %w", err)
	}
	length := make([]byte, 2)
	binary.BigEndian.PutUint16(length, uint16(len(v2OrderType)))
	wrapped := make([]byte, 0, len(signature)+64+len(v2OrderType)+2)
	wrapped = append(wrapped, signature...)
	wrapped = append(wrapped, domainHash...)
	wrapped = append(wrapped, contentsHash...)
	wrapped = append(wrapped, []byte(v2OrderType)...)
	wrapped = append(wrapped, length...)
	return "0x" + hex.EncodeToString(wrapped), nil
}

func (c *CLOBClient) request(
	ctx context.Context,
	method, path string,
	payload any,
	credentials *Credentials,
	target any,
) (int, error) {
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			return 0, err
		}
	}
	requestContext := ctx
	cancel := func() {}
	if c.timeout > 0 {
		requestContext, cancel = context.WithTimeout(ctx, c.timeout)
	}
	defer cancel()
	var requestWritten atomic.Bool
	buildRequest := func() (*http.Request, error) {
		attemptContext := requestContext
		if method != http.MethodGet {
			attemptContext = httptrace.WithClientTrace(requestContext, &httptrace.ClientTrace{
				WroteRequest: func(httptrace.WroteRequestInfo) {
					requestWritten.Store(true)
				},
			})
		}
		request, requestErr := http.NewRequestWithContext(
			attemptContext, method, c.baseURL+path, bytes.NewReader(body),
		)
		if requestErr != nil {
			return nil, requestErr
		}
		request.Header.Set("Accept", "application/json")
		if len(body) > 0 {
			request.Header.Set("Content-Type", "application/json")
		}
		if credentials == nil {
			return request, nil
		}
		timestamp := strconv.FormatInt(c.now().Unix(), 10)
		signPath := path
		if index := strings.Index(signPath, "?"); index >= 0 {
			signPath = signPath[:index]
		}
		signature, signErr := l2Signature(
			credentials.APISecret, timestamp, method, signPath, string(body),
		)
		if signErr != nil {
			return nil, signErr
		}
		request.Header.Set("POLY_ADDRESS", credentials.SignerAddress)
		request.Header.Set("POLY_SIGNATURE", signature)
		request.Header.Set("POLY_TIMESTAMP", timestamp)
		request.Header.Set("POLY_API_KEY", credentials.APIKey)
		request.Header.Set("POLY_PASSPHRASE", credentials.Passphrase)
		return request, nil
	}
	var response *http.Response
	if method == http.MethodGet {
		response, err = doWithRetry(requestContext, c.http, buildRequest, 3)
	} else {
		request, buildErr := buildRequest()
		if buildErr != nil {
			return 0, buildErr
		}
		response, err = c.http.Do(request)
	}
	if err != nil {
		return 0, &TransportError{
			Method: method, RequestWritten: requestWritten.Load(), Err: err,
		}
	}
	defer response.Body.Close()
	logCLOBRateLimit(method, path, response)
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return response.StatusCode, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var upstream struct {
			Code         string `json:"code"`
			Error        string `json:"error"`
			ErrorMessage string `json:"errorMsg"`
			Message      string `json:"message"`
		}
		_ = json.Unmarshal(responseBody, &upstream)
		message := strings.TrimSpace(upstream.ErrorMessage)
		if message == "" {
			message = strings.TrimSpace(upstream.Error)
		}
		if message == "" {
			message = strings.TrimSpace(upstream.Message)
		}
		if message == "" {
			message = strings.TrimSpace(string(responseBody))
		}
		if len(message) > 512 {
			message = message[:512]
		}
		return response.StatusCode, &CLOBError{
			StatusCode: response.StatusCode,
			Code:       strings.TrimSpace(upstream.Code),
			Message:    message,
		}
	}
	if target != nil && len(responseBody) > 0 {
		if err := json.Unmarshal(responseBody, target); err != nil {
			return response.StatusCode, err
		}
	}
	return response.StatusCode, nil
}

func logCLOBRateLimit(method, path string, response *http.Response) {
	remaining := response.Header.Get("Poly-RateLimit-Remaining")
	reset := response.Header.Get("Poly-RateLimit-Reset")
	retryAfter := response.Header.Get("Retry-After")
	if remaining == "" && reset == "" && retryAfter == "" {
		return
	}
	if index := strings.Index(path, "?"); index >= 0 {
		path = path[:index]
	}
	slog.Debug(
		"CLOB rate limit",
		"method", method, "path", path, "status", response.StatusCode,
		"remaining", remaining, "reset", reset, "retry_after", retryAfter,
	)
}

func l2Signature(secret, timestamp, method, path, body string) (string, error) {
	normalized := strings.NewReplacer("-", "+", "_", "/").Replace(secret)
	padding := len(normalized) % 4
	if padding != 0 {
		normalized += strings.Repeat("=", 4-padding)
	}
	key, err := base64.StdEncoding.DecodeString(normalized)
	if err != nil {
		return "", errors.New("invalid CLOB API secret")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(timestamp + strings.ToUpper(method) + path + body))
	return base64.URLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func normalizeOrderStatus(value string) string {
	switch strings.ToLower(value) {
	case "matched", "filled":
		return "filled"
	case "live", "open":
		return "open"
	case "delayed", "unmatched":
		return "pending"
	case "partially_filled":
		return "partially_filled"
	case "rejected", "failed":
		return "rejected"
	default:
		return "pending"
	}
}
