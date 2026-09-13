package trader

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/orderstream"
)

type Service struct {
	store               orderStore
	twaps               twapStore
	arbitrage           arbitrageStore
	catalog             instrumentCatalog
	credentials         credentialProvider
	venues              *exchange.Registry
	portfolios          portfolioSnapshotProvider
	orderStreams        *orderstream.Manager
	arbitrageValuations arbitrageValuationProvider
	arbitrageExchanges  map[string]bool
	timeout             time.Duration
	logger              *slog.Logger
}

var accountLocks sync.Map

func (s *Service) ConfigureTwap(store twapStore) {
	s.twaps = store
}

func (s *Service) ConfigureArbitrage(store arbitrageStore) {
	s.arbitrage = store
}

func (s *Service) ConfigureArbitragePositionSnapshots(
	portfolios portfolioSnapshotProvider,
) {
	s.portfolios = portfolios
}

func (s *Service) ConfigureArbitrageValuations(
	provider arbitrageValuationProvider,
) {
	s.arbitrageValuations = provider
}

func (s *Service) ConfigureArbitrageExchanges(enabled map[string]bool) {
	s.arbitrageExchanges = copyExchangeSet(enabled)
}

func (s *Service) ConfigureOrderStreams(manager *orderstream.Manager) {
	s.orderStreams = manager
}

func NewService(
	store orderStore,
	catalog instrumentCatalog,
	credentials credentialProvider,
	venues *exchange.Registry,
	timeout time.Duration,
	logger *slog.Logger,
) *Service {
	if timeout <= 0 {
		timeout = 12 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		store: store, catalog: catalog, credentials: credentials,
		venues: venues, timeout: timeout, logger: logger,
	}
}

func (s *Service) ListInstruments(
	ctx context.Context,
	token string,
	accountID int64,
	contractType string,
) ([]Instrument, error) {
	if strings.TrimSpace(token) == "" || accountID <= 0 {
		return nil, ErrInvalidArgument
	}
	contractType = strings.ToLower(strings.TrimSpace(contractType))
	if contractType != "spot" && contractType != "perpetual" {
		return nil, ErrInvalidArgument
	}
	account, err := s.credentials.Meta(ctx, token, accountID)
	if err != nil {
		return nil, err
	}
	return s.catalog.List(ctx, account.Exchange, contractType)
}

func (s *Service) GetVenueCapabilities(
	ctx context.Context,
	token string,
	accountID int64,
) (VenueCapabilities, error) {
	if strings.TrimSpace(token) == "" || accountID <= 0 {
		return VenueCapabilities{}, ErrInvalidArgument
	}
	account, err := s.credentials.Meta(ctx, token, accountID)
	if err != nil {
		return VenueCapabilities{}, err
	}
	fallback := staticVenueCapabilities(account.Exchange)
	adapter, ok := s.venues.Adapter(account.Exchange)
	if !ok {
		return fallback, nil
	}
	capabilities, err := venueCapabilities(ctx, adapter, Credentials{Exchange: account.Exchange})
	if err != nil {
		return fallback, nil
	}
	return VenueCapabilities{
		Products: capabilities.Products, QuoteAssets: capabilities.QuoteAssets,
		TimeInForce: capabilities.TimeInForce, PostOnly: capabilities.PostOnly,
		ReduceOnly: capabilities.ReduceOnly, MakerTwap: capabilities.MakerTwap,
		PrivateOrderStream: capabilities.PrivateOrderStream, OneWayOnly: capabilities.OneWayOnly,
	}, nil
}

func (s *Service) PlaceOrder(
	ctx context.Context,
	input PlaceOrderInput,
) (result Order, resultErr error) {
	exchangeName := ""
	defer func() {
		if resultErr == nil {
			return
		}
		s.logger.Error(
			"trader place order failed",
			"error_class", classifyOrderError(resultErr),
			"error", sanitizeError(resultErr),
			"account_id", input.TradingAccountID,
			"instrument_id", input.InstrumentID,
			"exchange", exchangeName,
			"order_id", result.ID,
		)
	}()
	normalized, err := normalizePlaceInput(input)
	if err != nil {
		return Order{}, err
	}
	owner, err := s.credentials.Owner(ctx, normalized.Token)
	if err != nil {
		return Order{}, err
	}
	account, err := s.credentials.Get(ctx, normalized.Token, normalized.TradingAccountID)
	if err != nil {
		return Order{}, err
	}
	exchangeName = account.Exchange
	instrument, err := s.catalog.Get(ctx, normalized.InstrumentID)
	if err != nil {
		return Order{}, err
	}
	if instrument.Exchange != account.Exchange {
		return Order{}, ErrInvalidArgument
	}
	adapter, ok := s.venues.Adapter(account.Exchange)
	if !ok {
		return Order{}, ErrUnsupportedExchange
	}
	capabilities, err := venueCapabilities(ctx, adapter, account)
	if err != nil {
		return Order{}, err
	}
	if !supportsCapability(capabilities.Products, instrument.ContractType) ||
		(len(capabilities.QuoteAssets) > 0 &&
			!supportsCapability(capabilities.QuoteAssets, instrument.QuoteAsset)) {
		return Order{}, ErrInvalidArgument
	}
	referencePrice := normalized.Price
	if normalized.OrderType == "market" {
		bboCtx, cancel := context.WithTimeout(ctx, s.timeout)
		bbo, bboErr := adapter.GetBBO(bboCtx, toVenueInstrument(instrument))
		cancel()
		if bboErr != nil {
			return Order{}, fmt.Errorf("%w: load validation price: %v", ErrVenueUnavailable, bboErr)
		}
		referencePrice = bbo.AskPrice
		if normalized.Side == "sell" {
			referencePrice = bbo.BidPrice
		}
	}
	quantity, price, err := prepareOrder(
		instrument, normalized.OrderType, normalized.Quantity,
		normalized.Price, referencePrice,
	)
	if err != nil {
		return Order{}, err
	}
	normalized.Quantity, normalized.Price = quantity, price
	intent, created, err := s.store.CreateIntent(ctx, Order{
		IdempotencyKey:   normalized.IdempotencyKey,
		OwnerUsername:    owner,
		TradingAccountID: account.TradingAccountID,
		ProductName:      account.ProductName,
		Exchange:         account.Exchange,
		InstrumentID:     instrument.ID,
		ContractType:     instrument.ContractType,
		ExchangeSymbol:   instrument.ExchangeSymbol,
		BaseAsset:        instrument.BaseAsset,
		QuoteAsset:       instrument.QuoteAsset,
		Side:             normalized.Side,
		OrderType:        normalized.OrderType,
		Quantity:         normalized.Quantity,
		Price:            normalized.Price,
		RequestFingerprint: requestFingerprint(
			account.TradingAccountID, instrument.ID, normalized.Side,
			normalized.OrderType, normalized.Quantity, normalized.Price,
		),
	})
	if err != nil {
		return Order{}, fmt.Errorf("%w: create intent: %w", ErrPersistence, err)
	}
	if !created {
		if intent.RequestFingerprint != requestFingerprint(
			account.TradingAccountID, instrument.ID, normalized.Side,
			normalized.OrderType, normalized.Quantity, normalized.Price,
		) {
			return Order{}, ErrIdempotencyConflict
		}
		return intent, nil
	}
	lock := s.accountLock(account.TradingAccountID)
	lock.Lock()
	defer lock.Unlock()
	return s.submit(ctx, adapter, account, instrument, intent)
}

func (s *Service) GetOrder(ctx context.Context, token, orderID string) (Order, error) {
	if strings.TrimSpace(token) == "" || strings.TrimSpace(orderID) == "" {
		return Order{}, ErrInvalidArgument
	}
	owner, err := s.credentials.Owner(ctx, token)
	if err != nil {
		return Order{}, err
	}
	order, err := s.store.GetByOwner(ctx, owner, orderID)
	if err != nil {
		return Order{}, err
	}
	if terminalStatus(order.Status) {
		return order, nil
	}
	account, err := s.credentials.Get(ctx, token, order.TradingAccountID)
	if err != nil {
		return Order{}, err
	}
	instrument, err := s.catalog.Get(ctx, order.InstrumentID)
	if err != nil {
		return Order{}, err
	}
	adapter, ok := s.venues.Adapter(account.Exchange)
	if !ok {
		return order, nil
	}
	lock := s.accountLock(account.TradingAccountID)
	lock.Lock()
	defer lock.Unlock()
	latest, err := s.store.GetByOwner(ctx, owner, orderID)
	if err != nil {
		return Order{}, err
	}
	if terminalStatus(latest.Status) {
		return latest, nil
	}
	queryCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	resolution, err := resolveVenueOrder(queryCtx, adapter, toVenueCredentials(account), exchange.QueryRequest{
		Instrument:    toVenueInstrument(instrument),
		ClientOrderID: latest.ClientOrderID,
		VenueOrderID:  latest.VenueOrderID,
		CreatedAt:     latest.CreatedAt,
	})
	if err != nil {
		result := resolution.Result
		if errors.Is(err, exchange.ErrRejected) ||
			errors.Is(err, exchange.ErrOrderNotFound) {
			eventType := "reconcile_failed"
			if errors.Is(err, exchange.ErrOrderNotFound) {
				eventType = "reconcile_absent"
			}
			if hasVenueResult(result) {
				result.Status = "unknown"
				result = orderErrorResult(
					result, "unknown", "venue_uncertain", sanitizeError(err),
				)
				updated, persistErr := s.persistResult(ctx, latest.ID, "reconcile_uncertain", result)
				if persistErr != nil {
					return Order{}, persistErr
				}
				return updated, wrapQueryError(err)
			}
			_ = s.store.AppendEvent(ctx, latest.ID, eventType, map[string]any{
				"error": sanitizeError(err),
			})
			return latest, wrapQueryError(err)
		}
		if hasVenueResult(result) {
			result.Status = "unknown"
			result = orderErrorResult(
				result, "unknown", "venue_uncertain", sanitizeError(err),
			)
			updated, persistErr := s.persistResult(ctx, latest.ID, "reconcile_uncertain", result)
			if persistErr != nil {
				return Order{}, persistErr
			}
			return updated, wrapQueryError(err)
		}
		_ = s.store.AppendEvent(ctx, latest.ID, "reconcile_failed", map[string]any{
			"error": sanitizeError(err),
		})
		return latest, wrapQueryError(err)
	}
	if resolution.ConfirmedAbsent {
		err = fmt.Errorf(
			"%w: venue history and active orders do not contain order %s",
			exchange.ErrOrderNotFound, latest.ID,
		)
		_ = s.store.AppendEvent(ctx, latest.ID, "reconcile_absent", map[string]any{
			"error": sanitizeError(err),
		})
		return latest, wrapQueryError(err)
	}
	result := resolution.Result
	return s.persistResult(ctx, latest.ID, "reconcile", normalizeVenueResult(latest, result))
}

func (s *Service) ListOrders(
	ctx context.Context,
	token string,
	accountID int64,
	view string,
	limit int,
	cursor string,
) ([]Order, string, error) {
	if strings.TrimSpace(token) == "" || accountID <= 0 {
		return nil, "", ErrInvalidArgument
	}
	view = strings.ToLower(strings.TrimSpace(view))
	if view == "" {
		view = "open"
	}
	if view != "open" && view != "history" {
		return nil, "", ErrInvalidArgument
	}
	cursor = strings.TrimSpace(cursor)
	if cursor != "" {
		if _, err := uuid.Parse(cursor); err != nil {
			return nil, "", ErrInvalidArgument
		}
	}
	owner, err := s.credentials.Owner(ctx, token)
	if err != nil {
		return nil, "", err
	}
	if _, err := s.credentials.Meta(ctx, token, accountID); err != nil {
		return nil, "", err
	}
	return s.store.ListByOwnerAccount(ctx, owner, accountID, view, limit, cursor)
}

func (s *Service) CancelOrder(ctx context.Context, token, orderID string) (Order, error) {
	if strings.TrimSpace(token) == "" || strings.TrimSpace(orderID) == "" {
		return Order{}, ErrInvalidArgument
	}
	owner, err := s.credentials.Owner(ctx, token)
	if err != nil {
		return Order{}, err
	}
	order, err := s.store.GetByOwner(ctx, owner, orderID)
	if err != nil {
		return Order{}, err
	}
	if terminalStatus(order.Status) {
		return Order{}, ErrOrderNotCancelable
	}
	if !cancelableStatus(order.Status) {
		return Order{}, ErrOrderNotCancelable
	}
	account, err := s.credentials.Get(ctx, token, order.TradingAccountID)
	if err != nil {
		return Order{}, err
	}
	instrument, err := s.catalog.Get(ctx, order.InstrumentID)
	if err != nil {
		return Order{}, err
	}
	adapter, ok := s.venues.Adapter(account.Exchange)
	if !ok {
		return Order{}, ErrUnsupportedExchange
	}
	lock := s.accountLock(account.TradingAccountID)
	lock.Lock()
	defer lock.Unlock()
	submitCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	result, err := adapter.CancelAndGetOrder(submitCtx, toVenueCredentials(account), exchange.CancelRequest{
		Instrument:    toVenueInstrument(instrument),
		ClientOrderID: order.ClientOrderID,
		VenueOrderID:  order.VenueOrderID,
	})
	if err != nil {
		_ = s.store.AppendEvent(ctx, order.ID, "cancel_failed", map[string]any{"error": sanitizeError(err)})
		if hasVenueResult(result) {
			result.Status = "unknown"
			result = orderErrorResult(
				result, "unknown", "venue_uncertain", sanitizeError(err),
			)
			updated, persistErr := s.persistResult(ctx, order.ID, "cancel_uncertain", result)
			if persistErr != nil {
				return Order{}, persistErr
			}
			return updated, wrapQueryError(err)
		}
		return order, wrapQueryError(err)
	}
	updated, err := s.persistResult(ctx, order.ID, "cancel", normalizeVenueResult(order, result))
	if err != nil {
		return Order{}, err
	}
	return updated, nil
}

func (s *Service) submit(
	ctx context.Context,
	adapter exchange.Adapter,
	account Credentials,
	instrument Instrument,
	intent Order,
) (Order, error) {
	return s.submitWithOptions(ctx, adapter, account, instrument, intent, "", false)
}

func (s *Service) submitPrepared(
	ctx context.Context,
	adapter exchange.Adapter,
	account Credentials,
	instrument Instrument,
	intent Order,
	timeInForce string,
	postOnly bool,
) (Order, error) {
	return s.placeSubmittedOrder(ctx, adapter, account, instrument, intent, timeInForce, postOnly, true)
}

func (s *Service) submitWithOptions(
	ctx context.Context,
	adapter exchange.Adapter,
	account Credentials,
	instrument Instrument,
	intent Order,
	timeInForce string,
	postOnly bool,
) (Order, error) {
	return s.placeSubmittedOrder(ctx, adapter, account, instrument, intent, timeInForce, postOnly, false)
}

func (s *Service) placeSubmittedOrder(
	ctx context.Context,
	adapter exchange.Adapter,
	account Credentials,
	instrument Instrument,
	intent Order,
	timeInForce string,
	postOnly bool,
	skipSubmitted bool,
) (Order, error) {
	if !skipSubmitted {
		_ = s.store.AppendEvent(ctx, intent.ID, "submitted", map[string]any{
			"exchange": account.Exchange, "symbol": instrument.ExchangeSymbol,
			"clientOrderId": intent.ClientOrderID,
		})
	}
	submitCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	result, err := adapter.PlaceOrder(submitCtx, toVenueCredentials(account), exchange.OrderRequest{
		Instrument:    toVenueInstrument(instrument),
		ClientOrderID: intent.ClientOrderID,
		Side:          intent.Side,
		OrderType:     intent.OrderType,
		Quantity:      intent.Quantity,
		Price:         intent.Price,
		TimeInForce:   timeInForce,
		PostOnly:      postOnly,
		ReduceOnly:    intent.ReduceOnly,
	})
	if err != nil {
		if uncertain(err) {
			recovered, recoverErr := s.recover(ctx, adapter, account, instrument, intent)
			if recoverErr == nil {
				return recovered, nil
			}
			result = orderErrorResult(
				result, "unknown", "venue_uncertain", sanitizeError(err),
			)
			updated, persistErr := s.persistResult(ctx, intent.ID, "uncertain", result)
			if persistErr != nil {
				return Order{}, persistErr
			}
			return updated, fmt.Errorf(
				"%w: place order: %w; recover order: %w",
				ErrVenueUncertain, err, recoverErr,
			)
		}
		errorCode := "venue_rejected"
		if errors.Is(err, exchange.ErrInvalidQuantity) {
			errorCode = "invalid_quantity"
		}
		result = orderErrorResult(
			result, "rejected", errorCode, sanitizeError(err),
		)
		updated, persistErr := s.persistResult(ctx, intent.ID, "reject", result)
		if persistErr != nil {
			return Order{}, persistErr
		}
		return updated, wrapVenueError(err)
	}
	result = normalizeVenueResult(intent, result)
	eventType := "accepted"
	if result.Status == "pending" {
		eventType = "venue_submitted"
	}
	return s.persistResult(ctx, intent.ID, eventType, result)
}

func (s *Service) recover(
	ctx context.Context,
	adapter exchange.Adapter,
	account Credentials,
	instrument Instrument,
	intent Order,
) (Order, error) {
	queryCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	resolution, err := resolveVenueOrder(queryCtx, adapter, toVenueCredentials(account), exchange.QueryRequest{
		Instrument:    toVenueInstrument(instrument),
		ClientOrderID: intent.ClientOrderID,
		VenueOrderID:  intent.VenueOrderID,
		CreatedAt:     intent.CreatedAt,
	})
	if err != nil {
		result := resolution.Result
		if errors.Is(err, exchange.ErrRejected) ||
			errors.Is(err, exchange.ErrOrderNotFound) {
			eventType := "reconcile_failed"
			if errors.Is(err, exchange.ErrOrderNotFound) {
				eventType = "reconcile_absent"
			}
			if hasVenueResult(result) {
				result.Status = "unknown"
				result = orderErrorResult(
					result, "unknown", "venue_uncertain", sanitizeError(err),
				)
				updated, persistErr := s.persistResult(ctx, intent.ID, "reconcile_uncertain", result)
				if persistErr != nil {
					return Order{}, persistErr
				}
				return updated, wrapQueryError(err)
			}
			_ = s.store.AppendEvent(ctx, intent.ID, eventType, map[string]any{
				"error": sanitizeError(err),
			})
			return intent, wrapQueryError(err)
		}
		if hasVenueResult(result) {
			result.Status = "unknown"
			result = orderErrorResult(
				result, "unknown", "venue_uncertain", sanitizeError(err),
			)
			updated, persistErr := s.persistResult(ctx, intent.ID, "reconcile_uncertain", result)
			if persistErr != nil {
				return Order{}, persistErr
			}
			return updated, wrapQueryError(err)
		}
		return Order{}, fmt.Errorf("%w: update result: %w", ErrVenueUncertain, err)
	}
	if resolution.ConfirmedAbsent {
		err = fmt.Errorf(
			"%w: venue history and active orders do not contain order %s",
			exchange.ErrOrderNotFound, intent.ID,
		)
		_ = s.store.AppendEvent(ctx, intent.ID, "reconcile_absent", map[string]any{
			"error": sanitizeError(err),
		})
		return intent, wrapQueryError(err)
	}
	result := resolution.Result
	return s.persistResult(ctx, intent.ID, "reconcile", normalizeVenueResult(intent, result))
}

func (s *Service) persistResult(
	ctx context.Context,
	orderID string,
	eventType string,
	result exchange.Result,
) (Order, error) {
	updated, err := s.store.UpdateResult(ctx, orderID, VenueResult{
		VenueOrderID:    result.VenueOrderID,
		Status:          result.Status,
		FilledQuantity:  result.FilledQuantity,
		AveragePrice:    result.AveragePrice,
		ErrorCode:       result.ErrorCode,
		ErrorMessage:    result.ErrorMessage,
		Reference:       result.Reference,
		LocalCommandAck: result.LocalCommandAck,
	})
	if err != nil {
		return Order{}, err
	}
	payload := map[string]any{
		"status": result.Status, "venueOrderId": result.VenueOrderID,
		"filledQuantity": result.FilledQuantity, "averagePrice": result.AveragePrice,
		"errorCode": result.ErrorCode, "errorMessage": result.ErrorMessage,
	}
	if result.Raw != nil {
		payload["raw"] = result.Raw
	}
	_ = s.store.AppendEvent(ctx, orderID, eventType, payload)
	s.logger.Info(
		"trader order updated",
		"order_id", updated.ID, "account_id", updated.TradingAccountID,
		"exchange", updated.Exchange, "status", updated.Status,
		"venue_order_id", updated.VenueOrderID,
	)
	s.bindHyperliquidOrderIDs(updated)
	return updated, nil
}

func (s *Service) bindHyperliquidOrderIDs(order Order) {
	if s == nil || s.orderStreams == nil ||
		!strings.EqualFold(strings.TrimSpace(order.Exchange), "hyperliquid") {
		return
	}
	clientOrderID := strings.TrimSpace(order.ClientOrderID)
	venueOrderID := strings.TrimSpace(order.VenueOrderID)
	if clientOrderID == "" || venueOrderID == "" {
		return
	}
	if adapter, ok := s.venues.Adapter(order.Exchange); ok {
		if encoder, ok := adapter.(exchange.ClientOrderIDEncoder); ok {
			clientOrderID = encoder.VenueClientOrderID(clientOrderID)
		}
	}
	s.orderStreams.BindClientVenueIDs(orderstream.Key{
		Account: strconv.FormatInt(order.TradingAccountID, 10),
		Venue:   order.Exchange,
		Product: order.ContractType,
	}, clientOrderID, venueOrderID)
}

func (s *Service) accountLock(accountID int64) *sync.Mutex {
	value, _ := accountLocks.LoadOrStore(accountID, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func normalizeVenueResult(order Order, result exchange.Result) exchange.Result {
	filled, filledErr := decimal.NewFromString(strings.TrimSpace(result.FilledQuantity))
	quantity, quantityErr := decimal.NewFromString(strings.TrimSpace(order.Quantity))
	if filledErr == nil && quantityErr == nil && quantity.IsPositive() && filled.GreaterThanOrEqual(quantity) {
		result.Status = "filled"
		result.FilledQuantity = quantity.String()
	}
	if strings.TrimSpace(result.Status) == "" {
		result.Status = order.Status
	}
	return result
}

func venueCapabilities(
	ctx context.Context,
	adapter exchange.Adapter,
	account Credentials,
) (exchange.Capabilities, error) {
	capabilities := staticExchangeCapabilities(account.Exchange)
	if provider, ok := adapter.(exchange.CapabilityProvider); ok {
		return provider.Capabilities(ctx, toVenueCredentials(account))
	}
	return capabilities, nil
}

func staticVenueCapabilities(exchangeName string) VenueCapabilities {
	capabilities := staticExchangeCapabilities(exchangeName)
	return VenueCapabilities{
		Products: capabilities.Products, QuoteAssets: capabilities.QuoteAssets,
		TimeInForce: capabilities.TimeInForce, PostOnly: capabilities.PostOnly,
		ReduceOnly: capabilities.ReduceOnly, MakerTwap: capabilities.MakerTwap,
		PrivateOrderStream: capabilities.PrivateOrderStream, OneWayOnly: capabilities.OneWayOnly,
	}
}

func staticExchangeCapabilities(exchangeName string) exchange.Capabilities {
	switch strings.ToLower(strings.TrimSpace(exchangeName)) {
	case "hyperliquid", "aster", "lighter":
		return exchange.Capabilities{
			Products: []string{"perpetual"}, TimeInForce: []string{"GTC", "IOC", "POST_ONLY"},
			PostOnly: true, ReduceOnly: true, MakerTwap: true, OneWayOnly: true,
			PrivateOrderStream: true, Arbitrage: true,
		}
	default:
		return exchange.Capabilities{
			Products: []string{"spot", "perpetual"}, TimeInForce: []string{"GTC", "IOC", "POST_ONLY"},
			PostOnly: true, ReduceOnly: true, MakerTwap: true, Arbitrage: true,
		}
	}
}

func copyExchangeSet(values map[string]bool) map[string]bool {
	if values == nil {
		return nil
	}
	result := make(map[string]bool, len(values))
	for name, enabled := range values {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" && enabled {
			result[name] = true
		}
	}
	return result
}

func exchangeEnabled(values map[string]bool, exchangeName string) bool {
	return values == nil || values[strings.ToLower(strings.TrimSpace(exchangeName))]
}

func supportsCapability(values []string, expected string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(expected)) {
			return true
		}
	}
	return false
}

func toVenueInstrument(instrument Instrument) exchange.Instrument {
	return exchange.Instrument{
		Exchange: instrument.Exchange, ContractType: instrument.ContractType,
		ExchangeSymbol: instrument.ExchangeSymbol, BaseAsset: instrument.BaseAsset,
		QuoteAsset: instrument.QuoteAsset, SettleAsset: instrument.SettleAsset,
		ContractSize: instrument.ContractSize, PriceTick: instrument.PriceTick,
		QuantityStep: instrument.QuantityStep,
		MinQuantity:  instrument.MinQuantity, MinNotional: instrument.MinNotional,
		MinQuantityStatus:        instrument.MinQuantityStatus,
		MinNotionalStatus:        instrument.MinNotionalStatus,
		MaxQuantity:              instrument.MaxQuantity,
		MarketQuantityStep:       instrument.MarketQuantityStep,
		MarketMinQuantity:        instrument.MarketMinQuantity,
		MarketMaxQuantity:        instrument.MarketMaxQuantity,
		MarketMinNotional:        instrument.MarketMinNotional,
		MaxQuantityStatus:        instrument.MaxQuantityStatus,
		MarketQuantityStepStatus: instrument.MarketQuantityStepStatus,
		MarketMinQuantityStatus:  instrument.MarketMinQuantityStatus,
		MarketMaxQuantityStatus:  instrument.MarketMaxQuantityStatus,
		MarketMinNotionalStatus:  instrument.MarketMinNotionalStatus,
		Metadata:                 instrument.Metadata,
	}
}

func toVenueCredentials(account Credentials) exchange.Credentials {
	return exchange.Credentials{
		APIKey: account.APIKey, APISecret: account.APISecret, Passphrase: account.Passphrase,
		CredentialKind: account.CredentialKind, SigningAddress: account.SigningAddress,
		VaultAddress: account.VaultAddress, AccountIndex: account.AccountIndex,
		APIKeyIndex: account.APIKeyIndex,
	}
}

func uncertain(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, exchange.ErrUncertain)
}

func hasVenueResult(result exchange.Result) bool {
	return strings.TrimSpace(result.Status) != "" ||
		strings.TrimSpace(result.ErrorCode) != "" ||
		strings.TrimSpace(result.ErrorMessage) != "" ||
		strings.TrimSpace(result.VenueOrderID) != ""
}

func wrapQueryError(err error) error {
	switch {
	case errors.Is(err, exchange.ErrRateLimited):
		return fmt.Errorf("%w: %w", ErrVenueRateLimited, err)
	case errors.Is(err, exchange.ErrRejected),
		errors.Is(err, exchange.ErrOrderNotFound):
		return fmt.Errorf("%w: %w", ErrVenueRejected, err)
	default:
		return fmt.Errorf("%w: %w", ErrVenueUncertain, err)
	}
}

func wrapVenueError(err error) error {
	switch {
	case errors.Is(err, exchange.ErrRateLimited):
		return fmt.Errorf("%w: %w", ErrVenueRateLimited, err)
	case errors.Is(err, exchange.ErrRejected),
		errors.Is(err, exchange.ErrOrderNotFound):
		return fmt.Errorf("%w: %w", ErrVenueRejected, err)
	case errors.Is(err, exchange.ErrAmbiguousCancel),
		errors.Is(err, exchange.ErrUncertain):
		return fmt.Errorf("%w: %w", ErrVenueUncertain, err)
	case errors.Is(err, exchange.ErrInvalidQuantity):
		return fmt.Errorf("%w: %w", ErrInvalidArgument, err)
	default:
		return fmt.Errorf("%w: %w", ErrVenueUnavailable, err)
	}
}

func orderErrorResult(
	result exchange.Result,
	status string,
	errorCode string,
	errorMessage string,
) exchange.Result {
	if strings.TrimSpace(result.Status) == "" {
		result.Status = status
	}
	if strings.TrimSpace(result.ErrorCode) == "" {
		result.ErrorCode = errorCode
	}
	if strings.TrimSpace(result.ErrorMessage) == "" {
		result.ErrorMessage = errorMessage
	}
	return result
}

func classifyOrderError(err error) string {
	switch {
	case errors.Is(err, ErrPersistence):
		return "persistence"
	case errors.Is(err, ErrVenueRateLimited):
		return "venue_rate_limited"
	case errors.Is(err, ErrVenueRejected):
		return "venue_rejected"
	case errors.Is(err, ErrVenueUncertain):
		return "venue_uncertain"
	case errors.Is(err, ErrVenueUnavailable):
		return "venue_unavailable"
	case errors.Is(err, ErrInvalidArgument):
		return "validation"
	default:
		return "internal"
	}
}

func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	lower := strings.ToLower(message)
	for _, secret := range []string{"apikey", "api-key", "secret", "passphrase", "signature"} {
		if strings.Contains(lower, secret) {
			return "venue request failed"
		}
	}
	return truncateMessage(message)
}
