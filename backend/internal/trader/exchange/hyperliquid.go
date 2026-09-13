package exchange

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	hl "github.com/Simon-Busch/hyperliquid-go"
	"github.com/Simon-Busch/hyperliquid-go/stream"
	hltrade "github.com/Simon-Busch/hyperliquid-go/trade"
	hltypes "github.com/Simon-Busch/hyperliquid-go/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/shopspring/decimal"
)

const (
	hyperliquidDefaultURL = "https://api.hyperliquid.xyz"
	hyperliquidHIP3Dex    = "xyz"
)

type hyperliquidAdapter struct {
	http    *http.Client
	info    *signedClient
	base    string
	circuit *venueCircuit
	locks   sync.Map
	clients sync.Map
}

type hyperliquidClientEntry struct {
	mu     sync.Mutex
	client *hl.Client
}

func newHyperliquid(client *http.Client, base string) Adapter {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if strings.TrimSpace(base) == "" {
		base = hyperliquidDefaultURL
	}
	base = strings.TrimRight(base, "/")
	return &hyperliquidAdapter{
		http: client, info: newSignedClient(client, base, 100*time.Millisecond),
		base: base, circuit: newVenueCircuit(),
	}
}

func (a *hyperliquidAdapter) Capabilities(context.Context, Credentials) (Capabilities, error) {
	return Capabilities{
		Products: []string{"perpetual"}, QuoteAssets: []string{"USDC"},
		TimeInForce: []string{"GTC", "IOC", "POST_ONLY"}, PostOnly: true,
		ReduceOnly: true, MakerTwap: true, PrivateOrderStream: true, OneWayOnly: true,
		Arbitrage: true,
	}, nil
}

func (*hyperliquidAdapter) VenueClientOrderID(value string) string {
	return hyperliquidCloid(value)
}

func (a *hyperliquidAdapter) GetBBO(ctx context.Context, instrument Instrument) (BBO, error) {
	if err := requirePerpetual(instrument); err != nil {
		return BBO{}, err
	}
	var payload struct {
		Time   int64 `json:"time"`
		Levels [][]struct {
			Price string `json:"px"`
		} `json:"levels"`
	}
	body := compactJSON(map[string]any{"type": "l2Book", "coin": instrument.ExchangeSymbol})
	_, err := a.info.do(ctx, http.MethodPost, "/info", nil, body, &payload)
	if err != nil {
		return BBO{}, err
	}
	if len(payload.Levels) < 2 || len(payload.Levels[0]) == 0 || len(payload.Levels[1]) == 0 {
		return BBO{}, fmt.Errorf("decode Hyperliquid BBO: empty levels")
	}
	return bbo(payload.Levels[0][0].Price, payload.Levels[1][0].Price, time.UnixMilli(payload.Time))
}

func (a *hyperliquidAdapter) GetPositionMode(
	context.Context,
	Credentials,
	Instrument,
) (string, error) {
	return PositionModeOneWay, nil
}

func (a *hyperliquidAdapter) PlaceOrder(
	ctx context.Context,
	credentials Credentials,
	request OrderRequest,
) (result Result, resultErr error) {
	if err := a.circuit.before(credentials, time.Now()); err != nil {
		return Result{}, err
	}
	defer func() { a.circuit.record(credentials, resultErr, time.Now()) }()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := requirePerpetual(request.Instrument); err != nil {
		return Result{}, err
	}
	if err := validateVenueOrderRules(request); err != nil {
		return Result{}, err
	}
	cloid := hyperliquidCloid(request.ClientOrderID)
	unlock := a.lock(credentials, cloid)
	defer unlock()
	existing, lookupErr := a.GetOrder(ctx, credentials, QueryRequest{
		Instrument: request.Instrument, ClientOrderID: request.ClientOrderID,
	})
	if lookupErr == nil {
		return existing, nil
	}
	if !errorsIsOrderNotFound(lookupErr) {
		return Result{}, fmt.Errorf(
			"%w: preflight Hyperliquid client order lookup: %v",
			ErrUncertain, lookupErr,
		)
	}
	if err := a.info.wait(ctx); err != nil {
		return Result{}, err
	}
	size, err := strconv.ParseFloat(request.Quantity, 64)
	if err != nil || size <= 0 {
		return Result{}, fmt.Errorf("%w: invalid Hyperliquid quantity", ErrInvalidQuantity)
	}
	price := float64(0)
	if request.OrderType == "limit" {
		price, err = strconv.ParseFloat(request.Price, 64)
		if err != nil || price <= 0 {
			return Result{}, fmt.Errorf("%w: invalid Hyperliquid price", ErrRejected)
		}
	}
	side := hltypes.Buy
	if strings.EqualFold(request.Side, "sell") {
		side = hltypes.Sell
	}
	options := []hltrade.PlaceOpt{hltrade.WithCloid(cloid)}
	if request.ReduceOnly {
		options = append(options, hltrade.WithReduceOnly())
	}
	client, releaseClient, err := a.client(
		credentials, request.Instrument.ExchangeSymbol,
	)
	if err != nil {
		return Result{}, err
	}
	coin, err := hyperliquidTradeSymbol(client, request.Instrument.ExchangeSymbol)
	if err != nil {
		releaseClient()
		return Result{}, err
	}
	var venue hltypes.Result
	func() {
		defer releaseClient()
		if err = a.ensureStream(ctx, client); err != nil {
			return
		}
		switch {
		case request.OrderType == "market":
			venue, err = client.Trade.PlaceMarketWS(ctx, coin, side, size, options...)
		case orderTimeInForce(request) == "post_only":
			venue, err = client.Trade.PlaceALOWS(ctx, coin, side, size, price, options...)
		case orderTimeInForce(request) == "ioc":
			venue, err = client.Trade.PlaceIOCWS(ctx, coin, side, size, price, options...)
		default:
			venue, err = client.Trade.PlaceGTCWS(ctx, coin, side, size, price, options...)
		}
	}()
	if err != nil {
		classified := classifyHyperliquidError(err)
		result = hyperliquidPlacementFailureResult(
			venue, cloid, request.ClientOrderID, err, classified,
		)
		if errors.Is(classified, ErrRateLimited) {
			a.info.postpone(time.Second)
		}
		return result, classified
	}
	return hyperliquidPlacementOutcome(request, venue, cloid, request.ClientOrderID)
}

func (a *hyperliquidAdapter) GetOrder(
	ctx context.Context,
	credentials Credentials,
	request QueryRequest,
) (Result, error) {
	if err := requirePerpetual(request.Instrument); err != nil {
		return Result{}, err
	}
	oid := any(hyperliquidCloid(request.ClientOrderID))
	if strings.TrimSpace(request.VenueOrderID) != "" {
		parsed, err := strconv.ParseInt(request.VenueOrderID, 10, 64)
		if err != nil {
			return Result{}, fmt.Errorf("%w: invalid Hyperliquid order id", ErrRejected)
		}
		oid = parsed
	}
	var payload hyperliquidOrderStatus
	body := compactJSON(map[string]any{
		"type": "orderStatus", "user": hyperliquidUser(credentials), "oid": oid,
	})
	raw, err := a.info.do(ctx, http.MethodPost, "/info", nil, body, &payload)
	if err != nil {
		return unknownQueryError(Result{}, err)
	}
	if payload.Status != "order" {
		return unknownQueryError(
			Result{Raw: rawMap(raw)},
			fmt.Errorf("%w: Hyperliquid order not found", ErrOrderNotFound),
		)
	}
	return payload.result(raw), nil
}

func (a *hyperliquidAdapter) CancelOrder(
	ctx context.Context,
	credentials Credentials,
	request CancelRequest,
) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := a.info.wait(ctx); err != nil {
		return Result{}, err
	}
	client, releaseClient, err := a.client(
		credentials, request.Instrument.ExchangeSymbol,
	)
	if err != nil {
		return Result{}, err
	}
	coin, err := hyperliquidTradeSymbol(client, request.Instrument.ExchangeSymbol)
	if err != nil {
		releaseClient()
		return Result{}, err
	}
	var status, venueID string
	func() {
		defer releaseClient()
		if strings.TrimSpace(request.VenueOrderID) != "" {
			oid, parseErr := strconv.ParseInt(request.VenueOrderID, 10, 64)
			if parseErr != nil {
				err = fmt.Errorf("%w: invalid Hyperliquid order id", ErrRejected)
				return
			}
			cancel, cancelErr := client.Trade.Cancel(coin, oid)
			err, status, venueID = cancelErr, cancel.Status, request.VenueOrderID
			if cancel.Error != "" {
				err = hyperliquidCancelError(cancel.Error)
			}
		} else {
			cloid := hyperliquidCloid(request.ClientOrderID)
			cancel, cancelErr := client.Trade.CancelByCloid(coin, cloid)
			err, status = cancelErr, cancel.Status
			if cancel.Error != "" {
				err = hyperliquidCancelError(cancel.Error)
			}
		}
	}()
	accepted := Result{
		VenueOrderID: venueID, Status: "pending", LocalCommandAck: true,
		Reference: VenueReference{
			ClientOrderID: request.ClientOrderID, VenueOrderID: venueID,
			Cloid:           hyperliquidCloid(request.ClientOrderID),
			ReconcileStatus: normalizeStatus(status),
		},
	}
	if err != nil {
		classified := classifyHyperliquidError(err)
		if errors.Is(classified, ErrRateLimited) {
			a.info.postpone(time.Second)
		}
		if strings.Contains(strings.ToLower(err.Error()), "invalid hyperliquid order id") {
			return Result{}, classified
		}
		return accepted, classified
	}
	if accepted.Status == "unknown" {
		accepted.Status = "pending"
	}
	return accepted, nil
}

func (a *hyperliquidAdapter) CancelAndGetOrder(
	ctx context.Context,
	credentials Credentials,
	request CancelRequest,
) (Result, error) {
	return a.CancelOrder(ctx, credentials, request)
}

func (a *hyperliquidAdapter) ResolveOrder(
	ctx context.Context,
	credentials Credentials,
	request QueryRequest,
) (OrderResolution, error) {
	if err := requireOrderLookupID(request); err != nil {
		return OrderResolution{}, err
	}
	return exactOrderResolution(a.GetOrder(ctx, credentials, request))
}

func (a *hyperliquidAdapter) ListFills(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	since time.Time,
) ([]Fill, error) {
	if err := requirePerpetual(instrument); err != nil {
		return nil, err
	}
	var payload []struct {
		Coin  string `json:"coin"`
		OID   int64  `json:"oid"`
		TID   int64  `json:"tid"`
		Price string `json:"px"`
		Size  string `json:"sz"`
		Time  int64  `json:"time"`
	}
	body := compactJSON(map[string]any{
		"type": "userFillsByTime", "user": hyperliquidUser(credentials),
		"startTime": max(int64(0), since.UnixMilli()),
	})
	if _, err := a.info.do(ctx, http.MethodPost, "/info", nil, body, &payload); err != nil {
		return nil, err
	}
	fills := make([]Fill, 0, len(payload))
	for _, item := range payload {
		if !strings.EqualFold(item.Coin, instrument.ExchangeSymbol) {
			continue
		}
		fills = append(fills, Fill{
			TradeID: strconv.FormatInt(item.TID, 10), VenueOrderID: strconv.FormatInt(item.OID, 10),
			Quantity: item.Size, Price: item.Price, ExecutedAt: time.UnixMilli(item.Time),
		})
	}
	return fills, nil
}

func (a *hyperliquidAdapter) ListPositions(
	ctx context.Context,
	credentials Credentials,
) ([]Position, error) {
	state, err := a.clearinghouseState(ctx, credentials)
	if err != nil {
		return nil, err
	}
	positions := make([]Position, 0, len(state.AssetPositions))
	for _, item := range state.AssetPositions {
		if strings.TrimSpace(item.Position.Size) == "" || item.Position.Size == "0" {
			continue
		}
		positions = append(positions, Position{
			Instrument: item.Position.Coin, Quantity: item.Position.Size,
			EntryPrice: item.Position.EntryPrice,
		})
	}
	return positions, nil
}

func (a *hyperliquidAdapter) ListBalances(
	ctx context.Context,
	credentials Credentials,
) ([]Balance, error) {
	state, err := a.clearinghouseState(ctx, credentials)
	if err != nil {
		return nil, err
	}
	return []Balance{{
		Asset: "USDC", Total: state.MarginSummary.AccountValue, Available: state.Withdrawable,
	}}, nil
}

func (a *hyperliquidAdapter) Health(ctx context.Context, credentials Credentials) Health {
	if a.circuit.open(credentials, time.Now()) {
		return Health{Healthy: false, CheckedAt: time.Now(), Message: "new order circuit open"}
	}
	_, err := a.info.do(ctx, http.MethodPost, "/info", nil, compactJSON(map[string]string{"type": "meta"}), nil)
	return Health{Healthy: err == nil, CheckedAt: time.Now(), Message: sanitizeVenueHealth(err)}
}

type hyperliquidClearinghouseState struct {
	MarginSummary struct {
		AccountValue string `json:"accountValue"`
	} `json:"marginSummary"`
	Withdrawable   string `json:"withdrawable"`
	AssetPositions []struct {
		Position struct {
			Coin       string `json:"coin"`
			Size       string `json:"szi"`
			EntryPrice string `json:"entryPx"`
		} `json:"position"`
	} `json:"assetPositions"`
}

func (a *hyperliquidAdapter) clearinghouseState(
	ctx context.Context,
	credentials Credentials,
) (hyperliquidClearinghouseState, error) {
	user := hyperliquidUser(credentials)
	state, err := a.fetchClearinghouseState(ctx, user, "")
	if err != nil {
		return hyperliquidClearinghouseState{}, err
	}
	xyz, xyzErr := a.fetchClearinghouseState(ctx, user, hyperliquidHIP3Dex)
	if xyzErr != nil {
		slog.Default().Warn("hyperliquid xyz clearinghouse skipped", "err", xyzErr)
		return state, nil
	}
	return mergeHyperliquidClearinghouse(state, xyz), nil
}

func (a *hyperliquidAdapter) fetchClearinghouseState(
	ctx context.Context,
	user, dex string,
) (hyperliquidClearinghouseState, error) {
	payload := map[string]any{"type": "clearinghouseState", "user": user}
	if dex != "" {
		payload["dex"] = dex
	}
	var state hyperliquidClearinghouseState
	_, err := a.info.do(ctx, http.MethodPost, "/info", nil, compactJSON(payload), &state)
	return state, err
}

func mergeHyperliquidClearinghouse(
	base, extra hyperliquidClearinghouseState,
) hyperliquidClearinghouseState {
	base.AssetPositions = append(base.AssetPositions, extra.AssetPositions...)
	base.MarginSummary.AccountValue = addHyperliquidAmount(
		base.MarginSummary.AccountValue, extra.MarginSummary.AccountValue,
	)
	base.Withdrawable = addHyperliquidAmount(base.Withdrawable, extra.Withdrawable)
	return base
}

func hyperliquidUser(credentials Credentials) string {
	return firstNonEmpty(
		strings.TrimSpace(credentials.VaultAddress),
		strings.TrimSpace(credentials.SigningAddress),
	)
}

func (a *hyperliquidAdapter) client(
	credentials Credentials,
	symbol string,
) (*hl.Client, func(), error) {
	key, err := parseHyperliquidKey(credentials.APISecret)
	if err != nil {
		return nil, nil, err
	}
	account := strings.TrimSpace(credentials.APIKey)
	if account == "" {
		account = strings.TrimSpace(credentials.SigningAddress)
	}
	if account == "" {
		return nil, nil, fmt.Errorf(
			"%w: Hyperliquid owner address is required", ErrRejected,
		)
	}
	vault := strings.TrimSpace(credentials.VaultAddress)
	dex := hyperliquidDexFromSymbol(symbol)
	identity := stableHex(strings.Join([]string{
		strings.ToLower(account),
		strings.ToLower(vault),
		strings.ToLower(crypto.PubkeyToAddress(key.PublicKey).Hex()),
		strings.ToLower(dex),
	}, "\x00"), 32)
	value, _ := a.clients.LoadOrStore(identity, &hyperliquidClientEntry{})
	entry := value.(*hyperliquidClientEntry)
	entry.mu.Lock()
	release := entry.mu.Unlock

	created := false
	if entry.client == nil {
		entry.client, err = a.newClient(key, account, vault, dex)
		created = true
	}
	if err != nil {
		release()
		return nil, nil, err
	}
	if !hyperliquidClientSupportsSymbol(entry.client, symbol) && !created {
		replacement, newErr := a.newClient(key, account, vault, dex)
		if newErr == nil {
			old := entry.client
			entry.client = replacement
			closeHyperliquidStream(old)
		}
	}
	if !hyperliquidClientSupportsSymbol(entry.client, symbol) {
		release()
		return nil, nil, fmt.Errorf(
			"%w: Hyperliquid symbol %q is missing from metadata",
			ErrRejected, strings.TrimSpace(symbol),
		)
	}
	return entry.client, release, nil
}

func (a *hyperliquidAdapter) newClient(
	key *ecdsa.PrivateKey,
	account, vault, dex string,
) (client *hl.Client, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			client = nil
			err = fmt.Errorf("%w: Hyperliquid client init: %v", ErrUncertain, recovered)
		}
	}()
	options := []hl.Option{
		hl.WithBaseURL(a.base), hl.WithPrivateKey(key), hl.WithAccount(account),
		hl.WithHTTPClient(a.http),
	}
	if vault != "" {
		options = append(options, hl.WithVault(vault))
	}
	if dex != "" {
		options = append(options, hl.WithBuilderDex(dex))
	}
	return hl.New(options...)
}

func (a *hyperliquidAdapter) WarmOrderTransport(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	client, release, err := a.client(credentials, instrument.ExchangeSymbol)
	if err != nil {
		return err
	}
	defer release()
	if err := a.ensureStream(ctx, client); err != nil {
		return classifyHyperliquidError(err)
	}
	return nil
}

func (a *hyperliquidAdapter) Close() error {
	a.clients.Range(func(_, value any) bool {
		entry := value.(*hyperliquidClientEntry)
		entry.mu.Lock()
		closeHyperliquidStream(entry.client)
		entry.client = nil
		entry.mu.Unlock()
		return true
	})
	return nil
}

func (a *hyperliquidAdapter) ensureStream(ctx context.Context, client *hl.Client) error {
	if client == nil || client.Stream == nil {
		return stream.ErrNotConnected
	}
	return client.Stream.Connect(ctx)
}

func closeHyperliquidStream(client *hl.Client) {
	if client != nil && client.Stream != nil {
		_ = client.Stream.Close()
	}
}

func hyperliquidDexFromSymbol(symbol string) string {
	symbol = strings.TrimSpace(symbol)
	index := strings.Index(symbol, ":")
	if index <= 0 {
		return ""
	}
	return symbol[:index]
}

func hyperliquidSymbolLookupNames(symbol string) []string {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return nil
	}
	names := []string{symbol}
	seen := map[string]struct{}{symbol: {}}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, exists := seen[name]; exists {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	dex := hyperliquidDexFromSymbol(symbol)
	if dex == "" {
		return names
	}
	bare := strings.TrimPrefix(symbol, dex+":")
	add(dex + ":" + bare)
	add(bare)
	return names
}

func hyperliquidClientCoin(client *hl.Client, symbol string) (string, bool) {
	if client == nil || client.Info == nil {
		return "", false
	}
	names := hyperliquidSymbolLookupNames(symbol)
	if dex := strings.TrimSpace(client.Info.PerpDexName()); dex != "" &&
		hyperliquidDexFromSymbol(symbol) == "" {
		prefixed := dex + ":" + strings.TrimSpace(symbol)
		already := false
		for _, name := range names {
			if name == prefixed {
				already = true
				break
			}
		}
		if !already {
			names = append(names, prefixed)
		}
	}
	for _, candidate := range names {
		canonical, ok := client.Info.NameToCoinMap()[candidate]
		if !ok {
			continue
		}
		if _, ok := client.Info.CoinToAssetMap()[canonical]; ok {
			return canonical, true
		}
	}
	return "", false
}

func hyperliquidClientSupportsSymbol(client *hl.Client, symbol string) bool {
	if client == nil || client.Trade == nil {
		return false
	}
	_, ok := hyperliquidClientCoin(client, symbol)
	return ok
}

func hyperliquidTradeSymbol(client *hl.Client, symbol string) (string, error) {
	coin, ok := hyperliquidClientCoin(client, symbol)
	if !ok {
		return "", fmt.Errorf(
			"%w: Hyperliquid symbol %q is missing from metadata",
			ErrRejected, strings.TrimSpace(symbol),
		)
	}
	return coin, nil
}

func addHyperliquidAmount(left, right string) string {
	sum := decimal.Zero
	for _, value := range []string{left, right} {
		parsed, err := decimal.NewFromString(strings.TrimSpace(value))
		if err != nil {
			continue
		}
		sum = sum.Add(parsed)
	}
	return sum.String()
}

func parseHyperliquidKey(value string) (*ecdsa.PrivateKey, error) {
	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(value), "0x"))
	if err != nil {
		return nil, fmt.Errorf("%w: invalid Hyperliquid agent key", ErrRejected)
	}
	return key, nil
}

type hyperliquidOrder struct {
	Coin      string  `json:"coin"`
	OID       int64   `json:"oid"`
	Cloid     *string `json:"cloid"`
	Sz        string  `json:"sz"`
	OrigSz    string  `json:"origSz"`
	Timestamp int64   `json:"timestamp"`
}

type hyperliquidOrderStatus struct {
	Status string `json:"status"`
	Order  struct {
		Order           hyperliquidOrder `json:"order"`
		Status          string           `json:"status"`
		StatusTimestamp int64            `json:"statusTimestamp"`
	} `json:"order"`
}

func (p hyperliquidOrderStatus) result(raw []byte) Result {
	return p.Order.Order.result(p.Order.Status, p.Order.StatusTimestamp, raw)
}

func (o hyperliquidOrder) result(status string, eventAt int64, raw []byte) Result {
	cloid := ""
	if o.Cloid != nil {
		cloid = *o.Cloid
	}
	status = normalizeStatus(status)
	return Result{
		VenueOrderID: strconv.FormatInt(o.OID, 10), Status: status,
		FilledQuantity: quotientDifference(o.OrigSz, o.Sz), Raw: rawMap(raw),
		Reference: VenueReference{
			VenueOrderID: strconv.FormatInt(o.OID, 10), Cloid: cloid,
			EventAt: time.UnixMilli(eventAt), ReconcileStatus: status,
		},
	}
}

func hyperliquidPlacementResult(venue hltypes.Result, cloid, clientID string) Result {
	status := normalizeStatus(venue.Status)
	if strings.EqualFold(venue.Status, "resting") {
		status = "open"
	}
	result := Result{
		Status: status, AveragePrice: venue.AvgPx, FilledQuantity: venue.TotalSz,
		ErrorMessage: venue.Error, LocalCommandAck: true,
		Reference: VenueReference{
			ClientOrderID: clientID, Cloid: cloid, ReconcileStatus: status,
		},
	}
	if venue.OID != 0 {
		result.VenueOrderID = strconv.FormatInt(venue.OID, 10)
		result.Reference.VenueOrderID = result.VenueOrderID
	}
	return result
}

func hyperliquidPlacementOutcome(
	request OrderRequest,
	venue hltypes.Result,
	cloid, clientID string,
) (Result, error) {
	result := hyperliquidPlacementResult(venue, cloid, clientID)
	if hyperliquidIOCNoMatch(request, venue) {
		result.Status = "canceled"
		result.FilledQuantity = "0"
		result.ErrorMessage = ""
		result.Reference.ReconcileStatus = "canceled"
		return result, nil
	}
	if venue.Error != "" {
		result.Status, result.ErrorMessage = "rejected", venue.Error
		result.Reference.ReconcileStatus = "rejected"
		return result, fmt.Errorf("%w: %s", ErrRejected, venue.Error)
	}
	return result, nil
}

func hyperliquidIOCNoMatch(request OrderRequest, venue hltypes.Result) bool {
	if !strings.EqualFold(orderTimeInForce(request), "ioc") ||
		!hyperliquidPlacementZeroFill(venue.TotalSz) {
		return false
	}
	if strings.EqualFold(
		strings.Trim(strings.TrimSpace(venue.Status), `"`),
		"iocCancelRejected",
	) {
		return true
	}
	return hyperliquidIOCNoMatchMessage(venue.Error)
}

func hyperliquidIOCNoMatchMessage(value string) bool {
	const reason = "Order could not immediately match against any resting orders"
	message := strings.TrimSuffix(strings.TrimSpace(value), ".")
	if strings.EqualFold(message, reason) {
		return true
	}
	prefix := reason + ". asset="
	if len(message) <= len(prefix) ||
		!strings.EqualFold(message[:len(prefix)], prefix) {
		return false
	}
	assetID := message[len(prefix):]
	for _, char := range assetID {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func hyperliquidPlacementZeroFill(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	parsed, err := strconv.ParseFloat(value, 64)
	return err == nil && parsed == 0
}

func hyperliquidPlacementFailureResult(
	venue hltypes.Result,
	cloid, clientID string,
	err, classified error,
) Result {
	result := hyperliquidPlacementResult(venue, cloid, clientID)
	if errors.Is(classified, ErrRejected) {
		result.Status = "rejected"
		result.FilledQuantity = "0"
		result.ErrorMessage = err.Error()
		result.Reference.ReconcileStatus = "rejected"
	}
	return result
}

func hyperliquidCloid(clientID string) string {
	return "0x" + stableHex(strings.TrimSpace(clientID), 32)
}

func (a *hyperliquidAdapter) lock(credentials Credentials, cloid string) func() {
	key := strings.ToLower(hyperliquidUser(credentials)) + ":" + strings.ToLower(cloid)
	value, _ := a.locks.LoadOrStore(key, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

func classifyHyperliquidError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrAmbiguousCancel) || errors.Is(err, ErrRejected) ||
		errors.Is(err, ErrRateLimited) || errors.Is(err, ErrUncertain) ||
		errors.Is(err, ErrOrderNotFound) {
		return err
	}
	if errors.Is(err, stream.ErrNotConnected) ||
		errors.Is(err, stream.ErrConnectionLost) ||
		errors.Is(err, stream.ErrRequestTimeout) ||
		errors.Is(err, stream.ErrRequestCanceled) ||
		errors.Is(err, stream.ErrBadResponse) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) {
		return fmt.Errorf("%w: Hyperliquid transport: %v", ErrUncertain, err)
	}
	var validationErr *hltypes.ValidationError
	if errors.As(err, &validationErr) {
		return fmt.Errorf("%w: Hyperliquid validation: %v", ErrRejected, err)
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "429") || strings.Contains(lower, "rate limit"):
		return fmt.Errorf("%w: Hyperliquid rate limited", ErrRateLimited)
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded") ||
		strings.Contains(lower, "context canceled") ||
		strings.Contains(lower, "not connected") ||
		strings.Contains(lower, "connection") || strings.Contains(lower, "eof"):
		return fmt.Errorf("%w: Hyperliquid transport: %v", ErrUncertain, err)
	case hyperliquidAmbiguousCancel(err.Error()):
		return fmt.Errorf("%w: %v", ErrAmbiguousCancel, err)
	default:
		return fmt.Errorf("%w: Hyperliquid: %v", ErrRejected, err)
	}
}

func hyperliquidCancelError(message string) error {
	if hyperliquidAmbiguousCancel(message) {
		return fmt.Errorf("%w: %s", ErrAmbiguousCancel, message)
	}
	return fmt.Errorf("%w: %s", ErrRejected, message)
}

func hyperliquidAmbiguousCancel(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if strings.Contains(lower, "missingorder") {
		return true
	}
	return strings.Contains(lower, "never placed") &&
		(strings.Contains(lower, "already canceled") || strings.Contains(lower, "already cancelled")) &&
		strings.Contains(lower, "filled")
}

func quotientDifference(total, remaining string) string {
	totalDecimal, totalErr := canonicalDecimal(total, false)
	remainingDecimal, remainingErr := canonicalDecimal(remaining, false)
	if totalErr != nil || remainingErr != nil {
		return ""
	}
	totalValue, err := decimal.NewFromString(totalDecimal)
	if err != nil {
		return ""
	}
	remainingValue, err := decimal.NewFromString(remainingDecimal)
	if err != nil {
		return ""
	}
	return totalValue.Sub(remainingValue).String()
}
