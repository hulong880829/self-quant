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

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
)

type TwapScheduler struct {
	store       twapStore
	orders      orderStore
	service     *Service
	catalog     instrumentCatalog
	credentials internalCredentialProvider
	venues      *exchange.Registry
	token       string
	interval    time.Duration
	lease       time.Duration
	timeout     time.Duration
	batchSize   int
	workers     int
	logger      *slog.Logger
}

func NewTwapScheduler(
	store twapStore,
	orders orderStore,
	service *Service,
	catalog instrumentCatalog,
	credentials internalCredentialProvider,
	venues *exchange.Registry,
	token string,
	interval time.Duration,
	lease time.Duration,
	timeout time.Duration,
	batchSize int,
	workers int,
	logger *slog.Logger,
) *TwapScheduler {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	if lease <= 0 {
		lease = 30 * time.Second
	}
	if timeout <= 0 {
		timeout = 12 * time.Second
	}
	if batchSize <= 0 {
		batchSize = 20
	}
	if workers <= 0 {
		workers = 4
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &TwapScheduler{
		store: store, orders: orders, service: service, catalog: catalog,
		credentials: credentials, venues: venues, token: token, interval: interval,
		lease: lease, timeout: timeout, batchSize: batchSize, workers: workers, logger: logger,
	}
}

func (s *TwapScheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		s.runOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *TwapScheduler) runOnce(ctx context.Context) {
	jobs, err := s.store.LeaseDueTwaps(ctx, s.batchSize, s.lease)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Error("lease due twaps failed", "error", err)
		}
		return
	}
	groups := make(map[string][]TwapJob)
	for _, job := range jobs {
		key := job.OwnerUsername + ":" + strconv.FormatInt(job.TradingAccountID, 10)
		groups[key] = append(groups[key], job)
	}
	sem := make(chan struct{}, s.workers)
	var wg sync.WaitGroup
	for _, group := range groups {
		group := group
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			lock := s.service.accountLock(group[0].TradingAccountID)
			lock.Lock()
			defer lock.Unlock()
			for _, job := range group {
				s.advance(ctx, job)
			}
		}()
	}
	wg.Wait()
}

func (s *TwapScheduler) advance(ctx context.Context, job TwapJob) {
	now := time.Now().UTC()
	refreshed, err := s.store.RefreshTwapProgress(ctx, job.ID)
	if err != nil {
		s.retry(ctx, job, err)
		return
	}
	job = refreshed
	total := parsePositiveDecimal(job.TotalQuantity)
	filled := parseDecimal(job.FilledQuantity)
	if !total.IsPositive() {
		s.fail(ctx, job, "invalid persisted total quantity")
		return
	}
	if filled.GreaterThanOrEqual(total) {
		s.close(ctx, job, "completed", "")
		return
	}
	if !now.Before(job.EndAt) {
		if job.ExecutionType == "maker" && job.ActiveOrderID != "" {
			_, _ = s.cancelActive(ctx, job)
			job, _ = s.store.RefreshTwapProgress(ctx, job.ID)
			filled = parseDecimal(job.FilledQuantity)
		}
		status := "partially_completed"
		if filled.GreaterThanOrEqual(total) {
			status = "completed"
		}
		s.close(ctx, job, status, "execution window ended")
		return
	}
	if job.Status == "pending" {
		fireAt := twapSliceFireAt(
			job.ID, job.CurrentSlice, job.StartAt, job.EndAt, job.IntervalSeconds,
		)
		job, err = s.store.UpdateTwapSchedule(
			ctx, job.ID, "running", job.CurrentSlice, job.CurrentAttempt,
			fireAt, job.ActiveOrderID, "",
		)
		if err != nil {
			s.retry(ctx, job, err)
			return
		}
		_ = s.store.AppendTwapEvent(ctx, job.ID, "started", map[string]any{"firstFireAt": fireAt})
		return
	}
	if job.ActiveOrderID != "" {
		s.handleActive(ctx, job)
		return
	}
	s.submitSlice(ctx, job)
}

func (s *TwapScheduler) handleActive(ctx context.Context, job TwapJob) {
	order, err := s.orders.GetByOwner(ctx, job.OwnerUsername, job.ActiveOrderID)
	if err != nil {
		s.retry(ctx, job, err)
		return
	}
	if terminalStatus(order.Status) {
		if job.ExecutionType == "maker" &&
			(order.Status == "canceled" || order.Status == "expired") &&
			time.Now().UTC().Before(twapSliceEnd(job)) {
			s.scheduleMakerRetry(ctx, job, order, order.Status)
			return
		}
		s.advanceSlice(ctx, job, order.Status)
		return
	}
	if job.ExecutionType == "maker" {
		deadline := order.CreatedAt.Add(time.Duration(job.OrderTimeoutSeconds) * time.Second)
		sliceEnd := twapSliceEnd(job)
		if sliceEnd.Before(deadline) {
			deadline = sliceEnd
		}
		if !time.Now().UTC().Before(deadline) {
			canceled, err := s.cancelActive(ctx, job)
			if err != nil {
				s.retry(ctx, job, err)
				return
			}
			if time.Now().UTC().Before(sliceEnd) {
				s.scheduleMakerRetry(ctx, job, canceled, "maker_timeout")
				return
			}
			s.advanceSlice(ctx, job, "maker_timeout")
			return
		}
		_, _ = s.store.UpdateTwapSchedule(
			ctx, job.ID, "running", job.CurrentSlice, job.CurrentAttempt,
			deadline, job.ActiveOrderID, "",
		)
		return
	}
	_, _ = s.store.UpdateTwapSchedule(
		ctx, job.ID, "running", job.CurrentSlice, job.CurrentAttempt,
		time.Now().UTC().Add(time.Second),
		job.ActiveOrderID, "",
	)
}

func (s *TwapScheduler) submitSlice(ctx context.Context, job TwapJob) {
	now := time.Now().UTC()
	remaining := parsePositiveDecimal(job.TotalQuantity).Sub(parseDecimal(job.FilledQuantity))
	remainingSeconds := job.EndAt.Sub(now).Seconds()
	if !remaining.IsPositive() || remainingSeconds <= 0 {
		s.close(ctx, job, "partially_completed", "execution window ended")
		return
	}
	instrument, err := s.catalog.Get(ctx, job.InstrumentID)
	if err != nil {
		s.retry(ctx, job, err)
		return
	}
	qty := decimal.Zero
	if job.CurrentAttempt > 0 {
		qty, err = s.makerRetryQuantity(ctx, job, instrument)
		if err != nil {
			s.retry(ctx, job, err)
			return
		}
	} else {
		qty = calculateSliceQuantity(
			remaining, time.Duration(job.IntervalSeconds)*time.Second,
			job.EndAt.Sub(now), parsePositiveDecimal(job.MaxQuantity),
			parsePositiveDecimal(instrument.QuantityStep),
		)
	}
	if !qty.IsPositive() {
		s.advanceSlice(ctx, job, "quantity_below_step")
		return
	}
	credentials, err := s.credentials.GetInternal(
		ctx, s.token, job.OwnerUsername, job.TradingAccountID,
	)
	if err != nil {
		s.retry(ctx, job, err)
		return
	}
	adapter, ok := s.venues.Adapter(job.Exchange)
	if !ok {
		s.fail(ctx, job, "unsupported exchange")
		return
	}
	orderType, price, tif, postOnly := "market", "", "", false
	if job.ExecutionType == "maker" {
		bboCtx, cancel := context.WithTimeout(ctx, s.timeout)
		bbo, bboErr := adapter.GetBBO(bboCtx, toVenueInstrument(instrument))
		cancel()
		if bboErr != nil || bbo.Timestamp.IsZero() || time.Since(bbo.Timestamp) > 5*time.Second {
			s.advanceSlice(ctx, job, "bbo_unavailable")
			return
		}
		price, err = makerPrice(job, instrument, bbo)
		if err != nil {
			s.advanceSlice(ctx, job, "bbo_invalid")
			return
		}
		orderType, tif, postOnly = "limit", "GTC", true
	} else if job.LimitPrice != "" {
		orderType, price, tif = "limit", job.LimitPrice, "IOC"
	}
	key := fmt.Sprintf("twap:%s:%d:%d", job.ID, job.CurrentSlice, job.CurrentAttempt)
	clientID := deterministicTwapClientID(job.ID, job.CurrentSlice, job.CurrentAttempt)
	child := Order{
		IdempotencyKey: key, OwnerUsername: job.OwnerUsername,
		TradingAccountID: job.TradingAccountID, ProductName: job.ProductName,
		Exchange: job.Exchange, InstrumentID: job.InstrumentID,
		ContractType: job.ContractType, ExchangeSymbol: job.ExchangeSymbol,
		BaseAsset: job.BaseAsset, QuoteAsset: job.QuoteAsset, ClientOrderID: clientID,
		Side: job.Side, OrderType: orderType, Quantity: qty.String(), Price: price,
		RequestFingerprint: requestFingerprint(
			job.TradingAccountID, job.InstrumentID, job.Side, orderType, qty.String(), price,
		),
		TwapJobID: job.ID, TwapSliceIndex: job.CurrentSlice,
		TwapAttemptIndex: job.CurrentAttempt,
	}
	var intent Order
	var created bool
	if atomicStore, ok := s.orders.(twapChildStore); ok {
		intent, created, err = atomicStore.CreateTwapIntent(ctx, job.ID, child)
	} else {
		intent, created, err = s.orders.CreateIntent(ctx, child)
	}
	if err != nil {
		if errors.Is(err, ErrTwapNotCancelable) {
			return
		}
		s.retry(ctx, job, err)
		return
	}
	if created {
		_ = s.store.AppendTwapEvent(ctx, job.ID, "slice_submitted", map[string]any{
			"slice": job.CurrentSlice, "attempt": job.CurrentAttempt,
			"orderId": intent.ID, "quantity": qty.String(), "price": price,
		})
		if _, err := s.service.submitWithOptions(
			ctx, adapter, credentials, instrument, intent, tif, postOnly,
		); err != nil && !errors.Is(err, ErrVenueUncertain) {
			if tradingAuthorizationLost(err) {
				s.fail(ctx, job, "trading authorization lost")
				return
			}
			s.logger.Warn("twap child submission failed", "job_id", job.ID, "error", sanitizeError(err))
		}
	}
	next := time.Now().UTC().Add(time.Second)
	if job.ExecutionType == "maker" {
		next = intent.CreatedAt.Add(time.Duration(job.OrderTimeoutSeconds) * time.Second)
		if intent.CreatedAt.IsZero() {
			next = time.Now().UTC().Add(time.Duration(job.OrderTimeoutSeconds) * time.Second)
		}
		if sliceEnd := twapSliceEnd(job); sliceEnd.Before(next) {
			next = sliceEnd
		}
	}
	_, _ = s.store.UpdateTwapSchedule(
		ctx, job.ID, "running", job.CurrentSlice, job.CurrentAttempt,
		next, intent.ID, "",
	)
}

func (s *TwapScheduler) cancelActive(ctx context.Context, job TwapJob) (Order, error) {
	order, err := s.orders.GetByOwner(ctx, job.OwnerUsername, job.ActiveOrderID)
	if err != nil {
		return Order{}, err
	}
	if terminalStatus(order.Status) {
		return order, nil
	}
	credentials, err := s.credentials.GetInternal(ctx, s.token, job.OwnerUsername, job.TradingAccountID)
	if err != nil {
		return Order{}, err
	}
	instrument, err := s.catalog.Get(ctx, job.InstrumentID)
	if err != nil {
		return Order{}, err
	}
	adapter, ok := s.venues.Adapter(job.Exchange)
	if !ok {
		return Order{}, ErrUnsupportedExchange
	}
	cancelCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	result, err := adapter.CancelAndGetOrder(cancelCtx, toVenueCredentials(credentials), exchange.CancelRequest{
		Instrument: toVenueInstrument(instrument), ClientOrderID: order.ClientOrderID,
		VenueOrderID: order.VenueOrderID,
	})
	if err != nil {
		return Order{}, err
	}
	if strings.TrimSpace(result.Status) == "" {
		result.Status = "canceled"
	}
	updated, err := s.orders.UpdateResult(ctx, order.ID, VenueResult{
		VenueOrderID: result.VenueOrderID, Status: result.Status,
		FilledQuantity: result.FilledQuantity, AveragePrice: result.AveragePrice,
		ErrorCode: result.ErrorCode, ErrorMessage: result.ErrorMessage,
		Reference: result.Reference,
	})
	if err == nil {
		_ = s.store.AppendTwapEvent(ctx, job.ID, "slice_canceled", map[string]any{"orderId": order.ID})
	}
	if err == nil && !terminalStatus(updated.Status) {
		err = fmt.Errorf("%w: cancellation awaiting venue confirmation", ErrVenueUncertain)
	}
	return updated, err
}

func (s *TwapScheduler) scheduleMakerRetry(
	ctx context.Context,
	job TwapJob,
	order Order,
	reason string,
) {
	remaining := parseDecimal(order.Quantity).Sub(parseDecimal(order.FilledQuantity))
	instrument, err := s.catalog.Get(ctx, job.InstrumentID)
	if err != nil {
		s.retry(ctx, job, err)
		return
	}
	remaining = floorToStep(remaining, parsePositiveDecimal(instrument.QuantityStep))
	if !remaining.IsPositive() || !time.Now().UTC().Before(twapSliceEnd(job)) {
		s.advanceSlice(ctx, job, reason)
		return
	}
	nextAttempt := max(job.CurrentAttempt, order.TwapAttemptIndex) + 1
	_, err = s.store.UpdateTwapSchedule(
		ctx, job.ID, "running", job.CurrentSlice, nextAttempt,
		time.Now().UTC(), "", "",
	)
	if err == nil {
		_ = s.store.AppendTwapEvent(ctx, job.ID, "slice_retry_scheduled", map[string]any{
			"slice": job.CurrentSlice, "attempt": nextAttempt,
			"quantity": remaining.String(), "reason": reason,
		})
	}
}

func (s *TwapScheduler) makerRetryQuantity(
	ctx context.Context,
	job TwapJob,
	instrument Instrument,
) (decimal.Decimal, error) {
	orders, err := s.store.ListTwapOrders(ctx, job.OwnerUsername, job.ID)
	if err != nil {
		return decimal.Zero, err
	}
	return makerRetryQuantityFromOrders(job, instrument, orders)
}

func makerRetryQuantityFromOrders(
	job TwapJob,
	instrument Instrument,
	orders []Order,
) (decimal.Decimal, error) {
	previousAttempt := job.CurrentAttempt - 1
	for index := len(orders) - 1; index >= 0; index-- {
		order := orders[index]
		if order.TwapSliceIndex != job.CurrentSlice || order.TwapAttemptIndex != previousAttempt {
			continue
		}
		remaining := parseDecimal(order.Quantity).Sub(parseDecimal(order.FilledQuantity))
		return floorToStep(remaining, parsePositiveDecimal(instrument.QuantityStep)), nil
	}
	return decimal.Zero, fmt.Errorf("previous twap attempt not found")
}

func twapSliceEnd(job TwapJob) time.Time {
	sliceEnd := job.StartAt.Add(
		time.Duration(job.CurrentSlice+1) * time.Duration(job.IntervalSeconds) * time.Second,
	)
	if sliceEnd.After(job.EndAt) {
		return job.EndAt
	}
	return sliceEnd
}

func (s *TwapScheduler) advanceSlice(ctx context.Context, job TwapJob, reason string) {
	nextSlice := job.CurrentSlice + 1
	next := twapSliceFireAt(job.ID, nextSlice, job.StartAt, job.EndAt, job.IntervalSeconds)
	_, err := s.store.UpdateTwapSchedule(ctx, job.ID, "running", nextSlice, 0, next, "", "")
	if err == nil {
		_ = s.store.AppendTwapEvent(ctx, job.ID, "slice_advanced", map[string]any{
			"slice": job.CurrentSlice, "reason": reason, "nextFireAt": next,
		})
	}
}

func (s *TwapScheduler) close(ctx context.Context, job TwapJob, status, message string) {
	closed, err := s.store.CloseTwap(ctx, job.ID, status, message)
	if err != nil {
		s.retry(ctx, job, err)
		return
	}
	if closed.Status != status {
		return
	}
	_, _ = s.store.RefreshTwapProgress(ctx, closed.ID)
	_ = s.store.AppendTwapEvent(ctx, job.ID, "closed", map[string]any{
		"status": status, "reason": message,
	})
}

func (s *TwapScheduler) fail(ctx context.Context, job TwapJob, message string) {
	s.close(ctx, job, "failed", message)
}

func tradingAuthorizationLost(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unauthorized") ||
		strings.Contains(message, "private key") ||
		strings.Contains(message, "api wallet not found") ||
		strings.Contains(message, "wallet unauthorized")
}

func (s *TwapScheduler) retry(ctx context.Context, job TwapJob, err error) {
	delay := time.Second << min(job.SchedulerFailures, 6)
	if delay > time.Minute {
		delay = time.Minute
	}
	_, _ = s.store.UpdateTwapSchedule(
		ctx, job.ID, job.Status, job.CurrentSlice, job.CurrentAttempt,
		time.Now().UTC().Add(delay),
		job.ActiveOrderID, sanitizeError(err),
	)
}

func makerPrice(job TwapJob, instrument Instrument, bbo exchange.BBO) (string, error) {
	value := bbo.BidPrice
	roundUp := false
	if job.Side == "sell" {
		value = bbo.AskPrice
		roundUp = true
	}
	price, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil || !price.IsPositive() {
		return "", ErrInvalidArgument
	}
	tick := parsePositiveDecimal(instrument.PriceTick)
	if tick.IsPositive() {
		if roundUp {
			price = ceilToStep(price, tick)
		} else {
			price = floorToStep(price, tick)
		}
	}
	if limit := parsePositiveDecimal(job.LimitPrice); limit.IsPositive() {
		if job.Side == "buy" && price.GreaterThan(limit) {
			price = limit
		}
		if job.Side == "sell" && price.LessThan(limit) {
			price = limit
		}
	}
	return price.String(), nil
}

func floorToStep(value, step decimal.Decimal) decimal.Decimal {
	if !step.IsPositive() {
		return value
	}
	return value.Div(step).Floor().Mul(step)
}

func calculateSliceQuantity(
	remaining decimal.Decimal,
	interval time.Duration,
	remainingTime time.Duration,
	maxQuantity decimal.Decimal,
	step decimal.Decimal,
) decimal.Decimal {
	if !remaining.IsPositive() || interval <= 0 || remainingTime <= 0 {
		return decimal.Zero
	}
	qty := remaining.Mul(decimal.NewFromInt(interval.Nanoseconds())).
		Div(decimal.NewFromInt(remainingTime.Nanoseconds()))
	if maxQuantity.IsPositive() && qty.GreaterThan(maxQuantity) {
		qty = maxQuantity
	}
	if qty.GreaterThan(remaining) {
		qty = remaining
	}
	return floorToStep(qty, step)
}

func ceilToStep(value, step decimal.Decimal) decimal.Decimal {
	if !step.IsPositive() {
		return value
	}
	return value.Div(step).Ceil().Mul(step)
}

func parseDecimal(value string) decimal.Decimal {
	parsed, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil {
		return decimal.Zero
	}
	return parsed
}

func deterministicTwapClientID(jobID string, slice, attempt int) string {
	compact := strings.ReplaceAll(jobID, "-", "")
	if len(compact) > 12 {
		compact = compact[:12]
	}
	return fmt.Sprintf("sqt%s%05d%03d", compact, slice, attempt)
}
