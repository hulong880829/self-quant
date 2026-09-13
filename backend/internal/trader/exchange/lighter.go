package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	lighterclient "github.com/elliottech/lighter-go/client"
	lightertypes "github.com/elliottech/lighter-go/types"
	"github.com/shopspring/decimal"
	corex "selfquant/backend/internal/exchange"
)

const (
	lighterDefaultURL = "https://mainnet.zklighter.elliot.ai"
	lighterChainID    = uint32(304)

	lighterOrderTypeLimit  = uint8(0)
	lighterOrderTypeMarket = uint8(1)
	lighterTIFIOC          = uint8(0)
	lighterTIFGTT          = uint8(1)
	lighterTIFPostOnly     = uint8(2)
)

type lighterAdapter struct {
	client *signedClient
	base   string
	now    func() time.Time

	locks   sync.Map
	circuit *venueCircuit
}

type lighterMarket struct {
	MarketID      int16
	SizeDecimals  int32
	PriceDecimals int32
}

func newLighter(client *http.Client, base string) Adapter {
	if strings.TrimSpace(base) == "" {
		base = lighterDefaultURL
	}
	base = strings.TrimRight(base, "/")
	return &lighterAdapter{
		client: newSignedClient(client, base, 120*time.Millisecond), base: base,
		now: time.Now, circuit: newVenueCircuit(),
	}
}

func (a *lighterAdapter) Capabilities(context.Context, Credentials) (Capabilities, error) {
	return Capabilities{
		Products: []string{"perpetual"}, QuoteAssets: []string{"USDC"},
		TimeInForce: []string{"GTC", "IOC", "POST_ONLY"}, PostOnly: true,
		ReduceOnly: true, MakerTwap: true, PrivateOrderStream: true, OneWayOnly: true,
		Arbitrage: true,
	}, nil
}

func (*lighterAdapter) VenueClientOrderID(value string) string {
	return strconv.FormatInt(lighterClientOrderIndex(value), 10)
}

func (a *lighterAdapter) GetBBO(ctx context.Context, instrument Instrument) (BBO, error) {
	market, err := a.market(ctx, instrument)
	if err != nil {
		return BBO{}, err
	}
	query := url.Values{
		"market_id": {strconv.FormatInt(int64(market.MarketID), 10)}, "limit": {"1"},
	}
	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Asks    []struct {
			Price string `json:"price"`
		} `json:"asks"`
		Bids []struct {
			Price string `json:"price"`
		} `json:"bids"`
	}
	_, err = a.client.do(ctx, http.MethodGet, "/api/v1/orderBookOrders?"+query.Encode(), nil, nil, &payload)
	if err != nil {
		return BBO{}, err
	}
	if payload.Code != 0 && payload.Code != 200 {
		return BBO{}, fmt.Errorf("%w: Lighter code %d: %s", ErrRejected, payload.Code, payload.Message)
	}
	if len(payload.Bids) == 0 || len(payload.Asks) == 0 {
		return BBO{}, fmt.Errorf("decode Lighter BBO: empty order book")
	}
	return bbo(payload.Bids[0].Price, payload.Asks[0].Price, a.now())
}

func (a *lighterAdapter) GetPositionMode(
	context.Context,
	Credentials,
	Instrument,
) (string, error) {
	return PositionModeOneWay, nil
}

func (a *lighterAdapter) PlaceOrder(
	ctx context.Context,
	credentials Credentials,
	request OrderRequest,
) (result Result, resultErr error) {
	if err := a.ensureIndexes(ctx, &credentials); err != nil {
		return Result{}, err
	}
	if err := a.circuit.before(credentials, a.now()); err != nil {
		return Result{}, err
	}
	defer func() { a.circuit.record(credentials, resultErr, a.now()) }()
	if err := requirePerpetual(request.Instrument); err != nil {
		return Result{}, err
	}
	if err := validateVenueOrderRules(request); err != nil {
		return Result{}, err
	}
	market, err := a.market(ctx, request.Instrument)
	if err != nil {
		return Result{}, err
	}
	unlock := a.lock(credentials)
	defer unlock()
	existing, lookupErr := a.GetOrder(ctx, credentials, QueryRequest{
		Instrument: request.Instrument, ClientOrderID: request.ClientOrderID,
	})
	if lookupErr == nil {
		return existing, nil
	}
	if !errorsIsOrderNotFound(lookupErr) {
		return Result{}, fmt.Errorf("%w: preflight Lighter client order lookup: %v", ErrUncertain, lookupErr)
	}
	signer, err := a.signer(credentials)
	if err != nil {
		return Result{}, err
	}
	baseAmount, err := scaledInteger(request.Quantity, market.SizeDecimals, 63)
	if err != nil || baseAmount <= 0 {
		return Result{}, fmt.Errorf("%w: invalid Lighter quantity: %v", ErrInvalidQuantity, err)
	}
	orderType, tif := lighterOrderTypeLimit, lighterTIFGTT
	priceText := request.Price
	if request.OrderType == "market" {
		orderType, tif = lighterOrderTypeMarket, lighterTIFIOC
		bboValue, bboErr := a.GetBBO(ctx, request.Instrument)
		if bboErr != nil {
			return Result{}, bboErr
		}
		priceText, err = lighterMarketProtectionPrice(request.Side, bboValue)
		if err != nil {
			return Result{}, err
		}
	} else {
		switch orderTimeInForce(request) {
		case "post_only":
			tif = lighterTIFPostOnly
		case "ioc":
			tif = lighterTIFIOC
		}
	}
	price, err := scaledInteger(priceText, market.PriceDecimals, 32)
	if err != nil || price <= 0 {
		return Result{}, fmt.Errorf("%w: invalid Lighter price: %v", ErrRejected, err)
	}
	clientIndex := lighterClientOrderIndex(request.ClientOrderID)
	isAsk, reduceOnly := uint8(0), uint8(0)
	if strings.EqualFold(request.Side, "sell") {
		isAsk = 1
	}
	if request.ReduceOnly {
		reduceOnly = 1
	}
	tx, err := signer.GetCreateOrderTransaction(&lightertypes.CreateOrderTxReq{
		MarketIndex: market.MarketID, ClientOrderIndex: clientIndex,
		BaseAmount: baseAmount, Price: uint32(price), IsAsk: isAsk, Type: orderType,
		TimeInForce: tif, ReduceOnly: reduceOnly,
		OrderExpiry: a.now().Add(28 * 24 * time.Hour).UnixMilli(),
	}, nil)
	if err != nil {
		return Result{}, fmt.Errorf("%w: sign Lighter order: %v", ErrRejected, err)
	}
	result, err = a.sendTx(ctx, tx.GetTxType(), tx)
	result.Reference = VenueReference{
		ClientOrderID: request.ClientOrderID, TxHash: result.Reference.TxHash,
		Nonce: tx.Nonce, AccountIndex: int64Value(credentials.AccountIndex),
		APIKeyIndex: int32Value(credentials.APIKeyIndex), ClientOrderIndex: clientIndex,
		EventAt: a.now(), ReconcileStatus: result.Status,
	}
	return result, err
}

func (a *lighterAdapter) GetOrder(
	ctx context.Context,
	credentials Credentials,
	request QueryRequest,
) (Result, error) {
	resolution, err := a.ResolveOrder(ctx, credentials, request)
	if err != nil {
		return Result{}, err
	}
	if resolution.Found {
		return resolution.Result, nil
	}
	return resolution.Result, fmt.Errorf(
		"%w: Lighter client order %d",
		ErrOrderNotFound, lighterClientOrderIndex(request.ClientOrderID),
	)
}

func (a *lighterAdapter) CancelOrder(
	ctx context.Context,
	credentials Credentials,
	request CancelRequest,
) (Result, error) {
	result, err := a.sendCancelOrder(ctx, credentials, request)
	return commandOnlyCancelResult(result, err)
}

func (a *lighterAdapter) CancelAndGetOrder(
	ctx context.Context,
	credentials Credentials,
	request CancelRequest,
) (Result, error) {
	result, err := a.sendCancelOrder(ctx, credentials, request)
	if err != nil {
		return result, err
	}
	latest, queryErr := a.GetOrder(ctx, credentials, QueryRequest{
		Instrument: request.Instrument, ClientOrderID: request.ClientOrderID,
		VenueOrderID: request.VenueOrderID,
	})
	if queryErr == nil && terminalOrderStatus(latest.Status) {
		latest.Reference.TxHash = result.Reference.TxHash
		latest.Reference.Nonce = result.Reference.Nonce
		return latest, nil
	}
	if queryErr == nil {
		queryErr = fmt.Errorf("order remains %s", latest.Status)
		latest.Reference.TxHash = result.Reference.TxHash
		latest.Reference.Nonce = result.Reference.Nonce
		result = latest
	}
	if result.Status == "unknown" {
		result.Status = "pending"
	}
	return result, fmt.Errorf(
		"%w: Lighter cancel accepted but sequencer state is not terminal: %v",
		ErrUncertain, queryErr,
	)
}

func (a *lighterAdapter) sendCancelOrder(
	ctx context.Context,
	credentials Credentials,
	request CancelRequest,
) (Result, error) {
	if err := a.ensureIndexes(ctx, &credentials); err != nil {
		return Result{}, err
	}
	market, err := a.market(ctx, request.Instrument)
	if err != nil {
		return Result{}, err
	}
	unlock := a.lock(credentials)
	defer unlock()
	signer, err := a.signer(credentials)
	if err != nil {
		return Result{}, err
	}
	clientIndex := lighterClientOrderIndex(request.ClientOrderID)
	tx, err := signer.GetCancelOrderTransaction(&lightertypes.CancelOrderTxReq{
		MarketIndex: market.MarketID, Index: clientIndex,
	}, nil)
	if err != nil {
		return Result{}, fmt.Errorf("%w: sign Lighter cancel: %v", ErrRejected, err)
	}
	result, err := a.sendTx(ctx, tx.GetTxType(), tx)
	result.Reference = VenueReference{
		ClientOrderID: request.ClientOrderID, VenueOrderID: request.VenueOrderID,
		TxHash: result.Reference.TxHash, Nonce: tx.Nonce, AccountIndex: int64Value(credentials.AccountIndex),
		APIKeyIndex: int32Value(credentials.APIKeyIndex), ClientOrderIndex: clientIndex,
		EventAt: a.now(), ReconcileStatus: result.Status,
	}
	return result, err
}

func (a *lighterAdapter) ResolveOrder(
	ctx context.Context,
	credentials Credentials,
	request QueryRequest,
) (OrderResolution, error) {
	if err := a.ensureIndexes(ctx, &credentials); err != nil {
		return OrderResolution{}, err
	}
	market, err := a.market(ctx, request.Instrument)
	if err != nil {
		return OrderResolution{}, err
	}
	history, complete, rawHistory, err := a.orderList(
		ctx, credentials, market, true,
	)
	if err != nil {
		return OrderResolution{}, err
	}
	if order, ok := matchLighterOrder(history, request); ok {
		result := order.result(rawHistory, credentials)
		return OrderResolution{
			Result: result, Found: true, Active: !terminalOrderStatus(result.Status),
		}, nil
	}
	active, _, rawActive, err := a.orderList(
		ctx, credentials, market, false,
	)
	if err != nil {
		return OrderResolution{}, err
	}
	if order, ok := matchLighterOrder(active, request); ok {
		result := order.result(rawActive, credentials)
		if result.Status == "unknown" {
			result.Status = "open"
		}
		return OrderResolution{Result: result, Found: true, Active: true}, nil
	}
	if !complete {
		return OrderResolution{}, fmt.Errorf(
			"%w: Lighter inactive order query reached its 100-order limit",
			ErrUncertain,
		)
	}
	return OrderResolution{ConfirmedAbsent: true}, nil
}

func (a *lighterAdapter) orderList(
	ctx context.Context,
	credentials Credentials,
	market lighterMarket,
	history bool,
) ([]lighterOrder, bool, []byte, error) {
	headers, err := a.authHeaders(credentials)
	if err != nil {
		return nil, false, nil, err
	}
	query := url.Values{
		"account_index": {strconv.FormatInt(int64Value(credentials.AccountIndex), 10)},
		"market_id":     {strconv.FormatInt(int64(market.MarketID), 10)},
		"market_type":   {"perp"},
	}
	path := "/api/v1/accountActiveOrders"
	if history {
		path = "/api/v1/accountInactiveOrders"
		query.Set("limit", "100")
	}
	var payload lighterOrdersResponse
	raw, err := a.client.do(
		ctx, http.MethodGet, path+"?"+query.Encode(), headers, nil, &payload,
	)
	if err != nil {
		return nil, false, raw, err
	}
	if payload.Code != 0 && payload.Code != 200 {
		return nil, false, raw, fmt.Errorf(
			"%w: Lighter code %d: %s", ErrRejected, payload.Code, payload.Message,
		)
	}
	return payload.Orders, !history || len(payload.Orders) < 100, raw, nil
}

func matchLighterOrder(orders []lighterOrder, request QueryRequest) (lighterOrder, bool) {
	clientIndex := lighterClientOrderIndex(request.ClientOrderID)
	venueID := strings.TrimSpace(request.VenueOrderID)
	for _, order := range orders {
		if venueID != "" && (order.OrderID == venueID ||
			strconv.FormatInt(order.OrderIndex, 10) == venueID) {
			return order, true
		}
		if strings.TrimSpace(request.ClientOrderID) != "" &&
			order.ClientOrderIndex == clientIndex {
			return order, true
		}
	}
	return lighterOrder{}, false
}

func (a *lighterAdapter) ListFills(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	since time.Time,
) ([]Fill, error) {
	if err := a.ensureIndexes(ctx, &credentials); err != nil {
		return nil, err
	}
	market, err := a.market(ctx, instrument)
	if err != nil {
		return nil, err
	}
	headers, err := a.authHeaders(credentials)
	if err != nil {
		return nil, err
	}
	query := url.Values{
		"market_id":     {strconv.FormatInt(int64(market.MarketID), 10)},
		"market_type":   {"perp"},
		"account_index": {strconv.FormatInt(int64Value(credentials.AccountIndex), 10)},
		"sort_by":       {"timestamp"},
		"sort_dir":      {"desc"},
		"limit":         {"100"},
	}
	if !since.IsZero() {
		query.Set("from", strconv.FormatInt(since.UnixMilli(), 10))
	}
	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Trades  []struct {
			TradeID    int64  `json:"trade_id"`
			Size       string `json:"size"`
			Price      string `json:"price"`
			AskID      int64  `json:"ask_id"`
			BidID      int64  `json:"bid_id"`
			AskAccount int64  `json:"ask_account_id"`
			BidAccount int64  `json:"bid_account_id"`
			Timestamp  int64  `json:"timestamp"`
		} `json:"trades"`
	}
	_, err = a.client.do(ctx, http.MethodGet, "/api/v1/trades?"+query.Encode(), headers, nil, &payload)
	if err != nil {
		return nil, err
	}
	if payload.Code != 0 && payload.Code != 200 {
		return nil, fmt.Errorf("%w: Lighter code %d: %s", ErrRejected, payload.Code, payload.Message)
	}
	fills := make([]Fill, 0, len(payload.Trades))
	for _, item := range payload.Trades {
		orderID := item.BidID
		if item.AskAccount == int64Value(credentials.AccountIndex) {
			orderID = item.AskID
		} else if item.BidAccount != int64Value(credentials.AccountIndex) {
			continue
		}
		fills = append(fills, Fill{
			TradeID:      strconv.FormatInt(item.TradeID, 10),
			VenueOrderID: strconv.FormatInt(orderID, 10),
			Quantity:     item.Size, Price: item.Price, ExecutedAt: time.UnixMilli(item.Timestamp),
		})
	}
	return fills, nil
}

func (a *lighterAdapter) ListPositions(
	ctx context.Context,
	credentials Credentials,
) ([]Position, error) {
	account, err := a.account(ctx, credentials)
	if err != nil {
		return nil, err
	}
	positions := make([]Position, 0, len(account.Positions))
	for _, item := range account.Positions {
		quantity, parseErr := decimal.NewFromString(item.Quantity)
		if parseErr != nil || quantity.IsZero() {
			continue
		}
		quantity = quantity.Abs()
		if item.Sign < 0 {
			quantity = quantity.Neg()
		}
		positions = append(positions, Position{
			Instrument: item.Symbol, Quantity: quantity.String(), EntryPrice: item.EntryPrice,
		})
	}
	return positions, nil
}

func (a *lighterAdapter) ListBalances(
	ctx context.Context,
	credentials Credentials,
) ([]Balance, error) {
	account, err := a.account(ctx, credentials)
	if err != nil {
		return nil, err
	}
	if len(account.Assets) == 0 {
		return []Balance{{
			Asset: "USDC", Total: account.Collateral, Available: account.AvailableBalance,
		}}, nil
	}
	balances := make([]Balance, 0, len(account.Assets))
	for _, item := range account.Assets {
		available := item.Balance
		total, totalErr := decimal.NewFromString(item.Balance)
		locked, lockedErr := decimal.NewFromString(item.LockedBalance)
		if totalErr == nil && lockedErr == nil {
			available = total.Sub(locked).String()
		}
		balances = append(balances, Balance{
			Asset: item.Symbol, Total: item.Balance, Available: available,
		})
	}
	return balances, nil
}

func (a *lighterAdapter) Health(ctx context.Context, credentials Credentials) Health {
	if a.circuit.open(credentials, a.now()) {
		return Health{Healthy: false, CheckedAt: a.now(), Message: "new order circuit open"}
	}
	_, err := a.client.do(ctx, http.MethodGet, "/api/v1/status", nil, nil, nil)
	return Health{Healthy: err == nil, CheckedAt: a.now(), Message: sanitizeVenueHealth(err)}
}

type lighterAccount struct {
	Collateral       string `json:"collateral"`
	AvailableBalance string `json:"available_balance"`
	Positions        []struct {
		Symbol     string `json:"symbol"`
		Sign       int32  `json:"sign"`
		Quantity   string `json:"position"`
		EntryPrice string `json:"avg_entry_price"`
	} `json:"positions"`
	Assets []struct {
		Symbol        string `json:"symbol"`
		Balance       string `json:"balance"`
		LockedBalance string `json:"locked_balance"`
	} `json:"assets"`
}

func (a *lighterAdapter) account(
	ctx context.Context,
	credentials Credentials,
) (lighterAccount, error) {
	if err := a.ensureIndexes(ctx, &credentials); err != nil {
		return lighterAccount{}, err
	}
	query := url.Values{
		"by": {"index"}, "value": {strconv.FormatInt(int64Value(credentials.AccountIndex), 10)},
	}
	var payload struct {
		Code     int              `json:"code"`
		Message  string           `json:"message"`
		Accounts []lighterAccount `json:"accounts"`
	}
	_, err := a.client.do(ctx, http.MethodGet, "/api/v1/account?"+query.Encode(), nil, nil, &payload)
	if err != nil {
		return lighterAccount{}, err
	}
	if payload.Code != 0 && payload.Code != 200 {
		return lighterAccount{}, fmt.Errorf(
			"%w: Lighter code %d: %s", ErrRejected, payload.Code, payload.Message,
		)
	}
	if len(payload.Accounts) == 0 {
		return lighterAccount{}, fmt.Errorf("%w: Lighter account %d", ErrOrderNotFound, int64Value(credentials.AccountIndex))
	}
	return payload.Accounts[0], nil
}

func (a *lighterAdapter) authHeaders(credentials Credentials) (http.Header, error) {
	signer, err := a.signer(credentials)
	if err != nil {
		return nil, err
	}
	token, err := signer.GetAuthToken(a.now().Add(10 * time.Minute))
	if err != nil {
		return nil, fmt.Errorf("%w: sign Lighter auth token: %v", ErrRejected, err)
	}
	return http.Header{"Authorization": {token}}, nil
}

func (a *lighterAdapter) signer(credentials Credentials) (*lighterclient.TxClient, error) {
	if credentials.AccountIndex == nil || credentials.APIKeyIndex == nil ||
		*credentials.AccountIndex < 0 || *credentials.APIKeyIndex < 0 || *credentials.APIKeyIndex > 255 ||
		strings.TrimSpace(credentials.APISecret) == "" {
		return nil, fmt.Errorf("%w: incomplete Lighter credentials", ErrRejected)
	}
	secret, err := corex.LighterTxPrivateKey(credentials.APISecret)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid Lighter API private key: %v", ErrRejected, err)
	}
	httpClient := &lighterNonceClient{adapter: a}
	signer, err := lighterclient.NewTxClient(
		httpClient, secret, *credentials.AccountIndex,
		uint8(*credentials.APIKeyIndex), lighterChainID,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid Lighter API private key: %v", ErrRejected, err)
	}
	return signer, nil
}

func (a *lighterAdapter) sendTx(ctx context.Context, txType uint8, tx interface {
	GetTxInfo() (string, error)
}) (Result, error) {
	info, err := tx.GetTxInfo()
	if err != nil {
		return Result{}, fmt.Errorf("%w: encode Lighter transaction: %v", ErrRejected, err)
	}
	form := url.Values{"tx_type": {strconv.Itoa(int(txType))}, "tx_info": {info}}
	headers := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		TxHash  string `json:"tx_hash"`
	}
	raw, err := a.client.do(ctx, http.MethodPost, "/api/v1/sendTx", headers, []byte(form.Encode()), &payload)
	result := Result{
		Status: "pending", Raw: rawMap(raw),
		Reference: VenueReference{TxHash: payload.TxHash, EventAt: a.now(), ReconcileStatus: "pending"},
	}
	if err != nil {
		return result, err
	}
	if payload.Code != 200 {
		result.Status, result.ErrorCode, result.ErrorMessage = "rejected", strconv.Itoa(payload.Code), payload.Message
		return result, fmt.Errorf("%w: Lighter code %d: %s", ErrRejected, payload.Code, payload.Message)
	}
	return result, nil
}

func (a *lighterAdapter) market(ctx context.Context, instrument Instrument) (lighterMarket, error) {
	if err := requirePerpetual(instrument); err != nil {
		return lighterMarket{}, err
	}
	if market, ok := lighterMarketFromMetadata(instrument); ok {
		return market, nil
	}
	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Markets []struct {
			Symbol                 string `json:"symbol"`
			MarketID               int16  `json:"market_id"`
			SupportedSizeDecimals  int32  `json:"supported_size_decimals"`
			SupportedPriceDecimals int32  `json:"supported_price_decimals"`
			SizeDecimals           int32  `json:"size_decimals"`
			PriceDecimals          int32  `json:"price_decimals"`
		} `json:"order_book_details"`
	}
	_, err := a.client.do(ctx, http.MethodGet, "/api/v1/orderBookDetails?filter=perp", nil, nil, &payload)
	if err != nil {
		return lighterMarket{}, err
	}
	if payload.Code != 0 && payload.Code != 200 {
		return lighterMarket{}, fmt.Errorf("%w: Lighter code %d: %s", ErrRejected, payload.Code, payload.Message)
	}
	for _, row := range payload.Markets {
		if strings.EqualFold(row.Symbol, instrument.ExchangeSymbol) {
			sizeDecimals := row.SupportedSizeDecimals
			if sizeDecimals == 0 {
				sizeDecimals = row.SizeDecimals
			}
			priceDecimals := row.SupportedPriceDecimals
			if priceDecimals == 0 {
				priceDecimals = row.PriceDecimals
			}
			return lighterMarket{
				MarketID: row.MarketID, SizeDecimals: sizeDecimals, PriceDecimals: priceDecimals,
			}, nil
		}
	}
	return lighterMarket{}, fmt.Errorf("%w: Lighter market %s", ErrOrderNotFound, instrument.ExchangeSymbol)
}

func (a *lighterAdapter) lock(credentials Credentials) func() {
	key := fmt.Sprintf("%d:%d", int64Value(credentials.AccountIndex), int32Value(credentials.APIKeyIndex))
	value, _ := a.locks.LoadOrStore(key, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

type lighterNonceClient struct {
	adapter *lighterAdapter
}

func (c *lighterNonceClient) GetNextNonce(accountIndex int64, apiKeyIndex uint8) (int64, error) {
	query := url.Values{
		"account_index": {strconv.FormatInt(accountIndex, 10)},
		"api_key_index": {strconv.Itoa(int(apiKeyIndex))},
	}
	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Nonce   int64  `json:"nonce"`
	}
	_, err := c.adapter.client.do(context.Background(), http.MethodGet, "/api/v1/nextNonce?"+query.Encode(), nil, nil, &payload)
	if err != nil {
		return 0, err
	}
	if payload.Code != 0 && payload.Code != 200 {
		return 0, fmt.Errorf("Lighter code %d: %s", payload.Code, payload.Message)
	}
	return payload.Nonce, nil
}

func (c *lighterNonceClient) GetApiKey(accountIndex int64, apiKeyIndex uint8) (string, error) {
	query := url.Values{
		"account_index": {strconv.FormatInt(accountIndex, 10)},
		"api_key_index": {strconv.Itoa(int(apiKeyIndex))},
	}
	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		APIKeys []struct {
			PublicKey string `json:"public_key"`
		} `json:"api_keys"`
	}
	_, err := c.adapter.client.do(context.Background(), http.MethodGet, "/api/v1/apikeys?"+query.Encode(), nil, nil, &payload)
	if err != nil {
		return "", err
	}
	if payload.Code != 0 && payload.Code != 200 {
		return "", fmt.Errorf("Lighter code %d: %s", payload.Code, payload.Message)
	}
	if len(payload.APIKeys) == 0 {
		return "", fmt.Errorf("Lighter API key not found")
	}
	return payload.APIKeys[0].PublicKey, nil
}

func (*lighterNonceClient) InvalidateApiKeys(int64) {}

type lighterOrdersResponse struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Orders  []lighterOrder `json:"orders"`
}

type lighterOrder struct {
	OrderIndex          int64  `json:"order_index"`
	ClientOrderIndex    int64  `json:"client_order_index"`
	OrderID             string `json:"order_id"`
	InitialBaseAmount   string `json:"initial_base_amount"`
	RemainingBaseAmount string `json:"remaining_base_amount"`
	FilledBaseAmount    string `json:"filled_base_amount"`
	FilledQuoteAmount   string `json:"filled_quote_amount"`
	Status              string `json:"status"`
	Nonce               int64  `json:"nonce"`
	UpdatedAt           int64  `json:"updated_at"`
}

func (o lighterOrder) result(raw []byte, credentials Credentials) Result {
	status := normalizeStatus(o.Status)
	average := quotientDecimal(o.FilledQuoteAmount, o.FilledBaseAmount)
	venueID := o.OrderID
	if venueID == "" && o.OrderIndex != 0 {
		venueID = strconv.FormatInt(o.OrderIndex, 10)
	}
	result := Result{
		VenueOrderID: venueID, Status: status, FilledQuantity: o.FilledBaseAmount,
		AveragePrice: average, Raw: rawMap(raw),
		Reference: VenueReference{
			VenueOrderID: venueID, Nonce: o.Nonce, AccountIndex: int64Value(credentials.AccountIndex),
			APIKeyIndex: int32Value(credentials.APIKeyIndex), ClientOrderIndex: o.ClientOrderIndex,
			EventAt: lighterTimestamp(o.UpdatedAt), ReconcileStatus: status,
		},
	}
	if strings.Contains(strings.ToLower(o.Status), "post-only") {
		result.ErrorCode = "canceled-post-only"
		result.ErrorMessage = o.Status
	}
	return result
}

func lighterMarketFromMetadata(instrument Instrument) (lighterMarket, bool) {
	marketID, marketErr := strconv.ParseInt(metadataString(instrument, "market_id"), 10, 16)
	size, sizeErr := strconv.ParseInt(firstNonEmpty(
		metadataString(instrument, "supported_size_decimals"),
		metadataString(instrument, "size_decimals"),
	), 10, 32)
	price, priceErr := strconv.ParseInt(firstNonEmpty(
		metadataString(instrument, "supported_price_decimals"),
		metadataString(instrument, "price_decimals"),
	), 10, 32)
	if marketErr != nil || sizeErr != nil || priceErr != nil {
		return lighterMarket{}, false
	}
	return lighterMarket{MarketID: int16(marketID), SizeDecimals: int32(size), PriceDecimals: int32(price)}, true
}

func scaledInteger(value string, decimals int32, bits int) (int64, error) {
	parsed, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil || parsed.IsNegative() {
		return 0, fmt.Errorf("invalid decimal")
	}
	scaled := parsed.Shift(decimals)
	if !scaled.Equal(scaled.Truncate(0)) {
		return 0, fmt.Errorf("too many decimal places")
	}
	result := scaled.IntPart()
	if bits == 32 && (result < 0 || result > int64(^uint32(0))) {
		return 0, fmt.Errorf("value exceeds uint32")
	}
	return result, nil
}

func lighterClientOrderIndex(clientID string) int64 {
	hexValue := stableHex(strings.TrimSpace(clientID), 12)
	value, _ := strconv.ParseInt(hexValue, 16, 64)
	value &= (int64(1) << 47) - 1
	if value == 0 {
		return 1
	}
	return value
}

func lighterMarketProtectionPrice(side string, book BBO) (string, error) {
	source := book.AskPrice
	multiplier := decimal.NewFromFloat(1.05)
	if strings.EqualFold(side, "sell") {
		source, multiplier = book.BidPrice, decimal.NewFromFloat(0.95)
	}
	price, err := decimal.NewFromString(source)
	if err != nil || !price.IsPositive() {
		return "", fmt.Errorf("%w: invalid Lighter BBO", ErrRejected)
	}
	return price.Mul(multiplier).String(), nil
}

func lighterTimestamp(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	if value < 1e12 {
		return time.Unix(value, 0)
	}
	return time.UnixMilli(value)
}

var _ lighterclient.MinimalHTTPClient = (*lighterNonceClient)(nil)
var _ = json.Number("")
