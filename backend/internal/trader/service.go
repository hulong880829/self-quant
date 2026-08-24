package trader

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
)

type Service struct {
	store       orderStore
	twaps       twapStore
	arbitrage   arbitrageStore
	catalog     instrumentCatalog
	credentials credentialProvider
	venues      *exchange.Registry
	timeout     time.Duration
	logger      *slog.Logger
}

var accountLocks sync.Map

func (s *Service) ConfigureTwap(store twapStore) {
	s.twaps = store
}

func (s *Service) ConfigureArbitrage(store arbitrageStore) {
	s.arbitrage = store
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
	account, err := s.credentials.Get(ctx, token, accountID)
	if err != nil {
		return nil, err
	}
	return s.catalog.List(ctx, account.Exchange, contractType)
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
	if err := validateInstrumentRules(instrument, normalized.OrderType, normalized.Quantity, normalized.Price); err != nil {
		return Order{}, err
	}
	adapter, ok := s.venues.Adapter(account.Exchange)
	if !ok {
		return Order{}, ErrUnsupportedExchange
	}
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
	result, err := adapter.GetOrder(queryCtx, exchange.Credentials{
		APIKey: account.APIKey, APISecret: account.APISecret, Passphrase: account.Passphrase,
	}, exchange.QueryRequest{
		Instrument:    toVenueInstrument(instrument),
		ClientOrderID: latest.ClientOrderID,
		VenueOrderID:  latest.VenueOrderID,
	})
	if err != nil {
		_ = s.store.AppendEvent(ctx, latest.ID, "reconcile_failed", map[string]any{
			"error": sanitizeError(err),
		})
		return latest, nil
	}
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
	if _, err := s.credentials.Get(ctx, token, accountID); err != nil {
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
	result, err := adapter.CancelOrder(submitCtx, exchange.Credentials{
		APIKey: account.APIKey, APISecret: account.APISecret, Passphrase: account.Passphrase,
	}, exchange.CancelRequest{
		Instrument:    toVenueInstrument(instrument),
		ClientOrderID: order.ClientOrderID,
		VenueOrderID:  order.VenueOrderID,
	})
	if err != nil {
		_ = s.store.AppendEvent(ctx, order.ID, "cancel_failed", map[string]any{"error": sanitizeError(err)})
		return order, err
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

func (s *Service) submitWithOptions(
	ctx context.Context,
	adapter exchange.Adapter,
	account Credentials,
	instrument Instrument,
	intent Order,
	timeInForce string,
	postOnly bool,
) (Order, error) {
	_ = s.store.AppendEvent(ctx, intent.ID, "submitted", map[string]any{
		"exchange": account.Exchange, "symbol": instrument.ExchangeSymbol,
		"clientOrderId": intent.ClientOrderID,
	})
	submitCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	result, err := adapter.PlaceOrder(submitCtx, exchange.Credentials{
		APIKey: account.APIKey, APISecret: account.APISecret, Passphrase: account.Passphrase,
	}, exchange.OrderRequest{
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
			updated, persistErr := s.persistResult(ctx, intent.ID, "uncertain", exchange.Result{
				Status: "unknown", ErrorCode: "venue_uncertain", ErrorMessage: sanitizeError(err),
			})
			if persistErr != nil {
				return Order{}, persistErr
			}
			return updated, fmt.Errorf(
				"%w: place order: %w; recover order: %w",
				ErrVenueUncertain, err, recoverErr,
			)
		}
		updated, persistErr := s.persistResult(ctx, intent.ID, "reject", exchange.Result{
			Status: "rejected", ErrorCode: "venue_rejected", ErrorMessage: sanitizeError(err),
		})
		if persistErr != nil {
			return Order{}, persistErr
		}
		return updated, wrapVenueError(err)
	}
	return s.persistResult(ctx, intent.ID, "accepted", normalizeVenueResult(intent, result))
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
	result, err := adapter.GetOrder(queryCtx, exchange.Credentials{
		APIKey: account.APIKey, APISecret: account.APISecret, Passphrase: account.Passphrase,
	}, exchange.QueryRequest{
		Instrument:    toVenueInstrument(instrument),
		ClientOrderID: intent.ClientOrderID,
		VenueOrderID:  intent.VenueOrderID,
	})
	if err != nil {
		return Order{}, fmt.Errorf("%w: update result: %w", ErrPersistence, err)
	}
	return s.persistResult(ctx, intent.ID, "reconcile", normalizeVenueResult(intent, result))
}

func (s *Service) persistResult(
	ctx context.Context,
	orderID string,
	eventType string,
	result exchange.Result,
) (Order, error) {
	updated, err := s.store.UpdateResult(ctx, orderID, VenueResult{
		VenueOrderID:   result.VenueOrderID,
		Status:         result.Status,
		FilledQuantity: result.FilledQuantity,
		AveragePrice:   result.AveragePrice,
		ErrorCode:      result.ErrorCode,
		ErrorMessage:   result.ErrorMessage,
	})
	if err != nil {
		return Order{}, err
	}
	payload := map[string]any{
		"status": result.Status, "venueOrderId": result.VenueOrderID,
		"filledQuantity": result.FilledQuantity, "averagePrice": result.AveragePrice,
		"errorCode": result.ErrorCode,
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
	return updated, nil
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

func toVenueInstrument(instrument Instrument) exchange.Instrument {
	return exchange.Instrument{
		Exchange: instrument.Exchange, ContractType: instrument.ContractType,
		ExchangeSymbol: instrument.ExchangeSymbol, BaseAsset: instrument.BaseAsset,
		QuoteAsset: instrument.QuoteAsset, SettleAsset: instrument.SettleAsset,
		ContractSize: instrument.ContractSize, Metadata: instrument.Metadata,
	}
}

func uncertain(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, exchange.ErrUncertain)
}

func wrapVenueError(err error) error {
	switch {
	case errors.Is(err, exchange.ErrRateLimited):
		return fmt.Errorf("%w: %w", ErrVenueRateLimited, err)
	case errors.Is(err, exchange.ErrRejected):
		return fmt.Errorf("%w: %w", ErrVenueRejected, err)
	default:
		return fmt.Errorf("%w: %w", ErrVenueUnavailable, err)
	}
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
