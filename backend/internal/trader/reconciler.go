package trader

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/orderstream"
)

type reconcileStore interface {
	LeaseDueOrders(context.Context, int, time.Duration) ([]Order, error)
	MarkReconcileFailure(context.Context, string, time.Time) error
	ConfirmOrderAbsence(context.Context, string, time.Time, time.Time) (Order, bool, error)
	UpdateResultWithFillDelta(context.Context, string, VenueResult) (Order, bool, error)
	AppendEvent(context.Context, string, string, map[string]any) error
	DeferReconcile(context.Context, string, time.Time) error
	RecomputeArbitrageBasePositionsForExecution(
		context.Context, string,
	) (ArbitrageCombination, error)
}

type internalCredentialProvider interface {
	GetInternal(context.Context, string, string, int64) (Credentials, error)
}

type reconcileStreamStore interface {
	ApplyStreamUpdate(context.Context, string, StreamUpdate) (Order, error)
}

type Reconciler struct {
	store        reconcileStore
	catalog      instrumentCatalog
	credentials  internalCredentialProvider
	venues       *exchange.Registry
	token        string
	interval     time.Duration
	timeout      time.Duration
	batchSize    int
	workers      int
	logger       *slog.Logger
	orderStreams *orderstream.Manager
	streamAudit  time.Duration
	watchMu      sync.Mutex
	watches      map[string]struct{}
}

func (r *Reconciler) ConfigureOrderStreams(
	manager *orderstream.Manager,
	auditInterval time.Duration,
) {
	r.orderStreams = manager
	if auditInterval <= 0 {
		auditInterval = 30 * time.Second
	}
	r.streamAudit = auditInterval
}

func NewReconciler(
	store reconcileStore,
	catalog instrumentCatalog,
	credentials internalCredentialProvider,
	venues *exchange.Registry,
	token string,
	interval time.Duration,
	timeout time.Duration,
	batchSize int,
	workers int,
	logger *slog.Logger,
) *Reconciler {
	if interval <= 0 {
		interval = time.Second
	}
	if timeout <= 0 {
		timeout = 12 * time.Second
	}
	if batchSize <= 0 {
		batchSize = 20
	}
	if workers <= 0 {
		workers = 3
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{
		store: store, catalog: catalog, credentials: credentials, venues: venues,
		token: token, interval: interval, timeout: timeout, batchSize: batchSize,
		workers: workers, logger: logger, watches: make(map[string]struct{}),
	}
}

func (r *Reconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		r.runOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Reconciler) runOnce(ctx context.Context) {
	orders, err := r.store.LeaseDueOrders(ctx, r.batchSize, 2*r.timeout)
	if err != nil {
		if ctx.Err() == nil {
			r.logger.Error("lease trader reconcile orders failed", "error", err)
		}
		return
	}
	groups := make(map[string][]Order)
	for _, order := range orders {
		key := order.OwnerUsername + ":" + strconv.FormatInt(order.TradingAccountID, 10)
		groups[key] = append(groups[key], order)
	}
	sem := make(chan struct{}, r.workers)
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
			r.reconcileAccount(ctx, group)
		}()
	}
	wg.Wait()
}

func (r *Reconciler) reconcileAccount(ctx context.Context, orders []Order) {
	if len(orders) == 0 {
		return
	}
	first := orders[0]
	value, _ := accountLocks.LoadOrStore(first.TradingAccountID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	credentials, err := r.credentials.GetInternal(
		ctx, r.token, first.OwnerUsername, first.TradingAccountID,
	)
	if err != nil {
		for _, order := range orders {
			r.fail(ctx, order, err)
		}
		return
	}
	adapter, ok := r.venues.Adapter(credentials.Exchange)
	if !ok {
		for _, order := range orders {
			r.fail(ctx, order, ErrUnsupportedExchange)
		}
		return
	}
	for _, order := range orders {
		if ctx.Err() != nil {
			return
		}
		if order.ArbitrageExecutionID != "" && r.orderStreams != nil &&
			!order.LastStreamEventAt.IsZero() &&
			time.Since(order.LastStreamEventAt) < r.streamAudit &&
			r.orderStreams.Healthy(orderstream.Key{
				Account: strconv.FormatInt(order.TradingAccountID, 10),
				Venue:   order.Exchange, Product: order.ContractType,
			}) {
			_ = r.store.DeferReconcile(ctx, order.ID, time.Now().Add(r.streamAudit))
			continue
		}
		instrument, err := r.catalog.Get(ctx, order.InstrumentID)
		if err != nil {
			r.fail(ctx, order, err)
			continue
		}
		r.ensureOrderStreamWatch(ctx, adapter, credentials, instrument, order)
		queryCtx, cancel := context.WithTimeout(ctx, r.timeout)
		resolution, err := resolveVenueOrder(queryCtx, adapter, toVenueCredentials(credentials), exchange.QueryRequest{
			Instrument: toVenueInstrument(instrument), ClientOrderID: order.ClientOrderID,
			VenueOrderID: order.VenueOrderID, CreatedAt: order.CreatedAt,
		})
		cancel()
		if err != nil {
			result := resolution.Result
			if hasVenueResult(result) {
				result.Status = "unknown"
				result = normalizeVenueResult(order, result)
				if updated, filledChanged, persistErr := r.store.UpdateResultWithFillDelta(ctx, order.ID, VenueResult{
					VenueOrderID: result.VenueOrderID, Status: result.Status,
					FilledQuantity: result.FilledQuantity, AveragePrice: result.AveragePrice,
					ErrorCode: result.ErrorCode, ErrorMessage: result.ErrorMessage,
					Reference: result.Reference,
				}); persistErr == nil && filledChanged && updated.ArbitrageExecutionID != "" {
					_, _ = r.store.RecomputeArbitrageBasePositionsForExecution(
						ctx, updated.ArbitrageExecutionID,
					)
				}
			}
			r.fail(ctx, order, err)
			continue
		}
		if resolution.ConfirmedAbsent {
			if terminalStatus(order.Status) {
				result := exchange.Result{
					VenueOrderID:   order.VenueOrderID,
					Status:         order.Status,
					FilledQuantity: order.FilledQuantity,
					AveragePrice:   order.AveragePrice,
					ErrorCode:      order.ErrorCode,
					ErrorMessage:   order.ErrorMessage,
				}
				if _, _, persistErr := r.store.UpdateResultWithFillDelta(ctx, order.ID, VenueResult{
					VenueOrderID: result.VenueOrderID, Status: result.Status,
					FilledQuantity: result.FilledQuantity, AveragePrice: result.AveragePrice,
					ErrorCode: result.ErrorCode, ErrorMessage: result.ErrorMessage,
				}); persistErr != nil {
					r.fail(ctx, order, persistErr)
				}
				continue
			}
			if strings.TrimSpace(order.VenueOrderID) != "" {
				r.fail(ctx, order, exchange.ErrOrderNotFound)
				continue
			}
			delay := time.Second << min(order.AbsenceConfirmations+1, 6)
			if delay > time.Minute {
				delay = time.Minute
			}
			updatedOrder, converted, confirmErr := r.store.ConfirmOrderAbsence(
				ctx, order.ID, order.UpdatedAt, time.Now().UTC().Add(delay),
			)
			if confirmErr != nil {
				r.fail(ctx, order, confirmErr)
				continue
			}
			if converted {
				_ = r.store.AppendEvent(ctx, order.ID, "reconcile", map[string]any{
					"status": updatedOrder.Status, "filledQuantity": updatedOrder.FilledQuantity,
					"errorCode": updatedOrder.ErrorCode,
				})
				if updatedOrder.ArbitrageExecutionID != "" {
					if !parseDecimal(order.FilledQuantity).Equal(parseDecimal(updatedOrder.FilledQuantity)) {
						if _, err := r.store.RecomputeArbitrageBasePositionsForExecution(
							ctx, updatedOrder.ArbitrageExecutionID,
						); err != nil {
							r.fail(ctx, order, err)
							continue
						}
					}
					if finalizer, ok := r.store.(interface {
						FinalizeConfirmedAbsentZeroFillExecution(
							context.Context, string,
						) (confirmedAbsentFinalizeResult, error)
					}); ok {
						_, _ = finalizer.FinalizeConfirmedAbsentZeroFillExecution(
							ctx, updatedOrder.ArbitrageExecutionID,
						)
					}
				}
				continue
			}
			if updatedOrder.AbsenceConfirmations == order.AbsenceConfirmations {
				_ = r.store.DeferReconcile(ctx, order.ID, time.Now().UTC().Add(2*time.Second))
				continue
			}
			_ = r.store.AppendEvent(ctx, order.ID, "reconcile_absent", map[string]any{
				"absenceConfirmations": updatedOrder.AbsenceConfirmations,
			})
			continue
		}
		result := resolution.Result
		result = normalizeVenueResult(order, result)
		updatedOrder, filledChanged, err := r.store.UpdateResultWithFillDelta(ctx, order.ID, VenueResult{
			VenueOrderID: result.VenueOrderID, Status: result.Status,
			FilledQuantity: result.FilledQuantity, AveragePrice: result.AveragePrice,
			ErrorCode: result.ErrorCode, ErrorMessage: result.ErrorMessage,
			Reference: result.Reference,
		})
		if err != nil {
			r.fail(ctx, order, err)
			continue
		}
		if filledChanged && updatedOrder.ArbitrageExecutionID != "" {
			if _, err := r.store.RecomputeArbitrageBasePositionsForExecution(
				ctx, updatedOrder.ArbitrageExecutionID,
			); err != nil {
				r.fail(ctx, order, err)
				continue
			}
		}
		_ = r.store.AppendEvent(ctx, order.ID, "reconcile", map[string]any{
			"status": result.Status, "filledQuantity": result.FilledQuantity,
			"venueOrderId": result.VenueOrderID,
		})
	}
}

func (r *Reconciler) ensureOrderStreamWatch(
	ctx context.Context,
	adapter exchange.Adapter,
	credentials Credentials,
	instrument Instrument,
	order Order,
) {
	if r.orderStreams == nil || terminalStatus(order.Status) {
		return
	}
	provider, ok := adapter.(exchange.CapabilityProvider)
	if !ok {
		return
	}
	capabilities, err := provider.Capabilities(ctx, toVenueCredentials(credentials))
	if err != nil || !capabilities.PrivateOrderStream {
		return
	}
	store, ok := r.store.(reconcileStreamStore)
	if !ok {
		return
	}
	r.watchMu.Lock()
	if _, exists := r.watches[order.ID]; exists {
		r.watchMu.Unlock()
		return
	}
	r.watches[order.ID] = struct{}{}
	r.watchMu.Unlock()

	subscription, err := r.orderStreams.Subscribe(ctx, orderstream.Key{
		Account: strconv.FormatInt(order.TradingAccountID, 10),
		Venue:   order.Exchange, Product: order.ContractType,
	}, toOrderStreamCredentials(credentials))
	if err != nil {
		r.releaseWatch(order.ID)
		r.logger.Warn("trader private order stream unavailable",
			"order_id", order.ID, "exchange", order.Exchange, "error", sanitizeError(err))
		return
	}
	clientOrderID := order.ClientOrderID
	if encoder, ok := adapter.(exchange.ClientOrderIDEncoder); ok {
		clientOrderID = encoder.VenueClientOrderID(clientOrderID)
	}
	watch, err := subscription.WatchOrder(ctx, clientOrderID)
	if err != nil {
		_ = subscription.Close()
		r.releaseWatch(order.ID)
		return
	}
	go r.consumeOrderStream(ctx, store, subscription, watch, instrument, order)
}

func (r *Reconciler) consumeOrderStream(
	ctx context.Context,
	store reconcileStreamStore,
	subscription *orderstream.Subscription,
	watch *orderstream.OrderWatch,
	instrument Instrument,
	order Order,
) {
	defer func() {
		_ = watch.Close()
		_ = subscription.Close()
		r.releaseWatch(order.ID)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-watch.Updates():
			if !ok {
				return
			}
			if update.Type == orderstream.UpdateConnection {
				continue
			}
			updated, err := r.applyPrivateStreamUpdate(
				ctx, store, instrument, order, update,
			)
			if errors.Is(err, errHyperliquidFillUnknown) {
				continue
			}
			if err != nil {
				r.logger.Warn("apply trader private stream update failed",
					"order_id", order.ID, "error", sanitizeError(err))
				continue
			}
			_ = r.store.AppendEvent(ctx, order.ID, "venue_stream", map[string]any{
				"status": updated.Status, "venueOrderId": updated.VenueOrderID,
				"filledQuantity": updated.FilledQuantity, "tradeId": update.TradeID,
			})
			if terminalStatus(updated.Status) {
				return
			}
		}
	}
}

func (r *Reconciler) applyPrivateStreamUpdate(
	ctx context.Context,
	store reconcileStreamStore,
	instrument Instrument,
	order Order,
	update orderstream.Update,
) (Order, error) {
	status := string(update.Status)
	if update.Status == orderstream.StatusNew {
		status = "open"
	}
	cumulative, last := update.CumulativeFilled, update.LastFilled
	var err error
	if cumulative != "" {
		cumulative, err = exchange.FromVenueQuantity(toVenueInstrument(instrument), cumulative)
		if err != nil {
			return order, err
		}
	}
	if last != "" {
		last, err = exchange.FromVenueQuantity(toVenueInstrument(instrument), last)
		if err != nil {
			return order, err
		}
	}
	if hyperliquidTerminalFillUnknown(order.Exchange, orderstream.Update{
		Type: update.Type, Status: update.Status, CumulativeFilled: cumulative,
	}) {
		return order, errHyperliquidFillUnknown
	}
	streamUpdate := StreamUpdate{
		Result: VenueResult{
			VenueOrderID: update.VenueOrderID, Status: status,
			FilledQuantity: cumulative, AveragePrice: update.AveragePrice,
			ErrorCode: update.ErrorCode, ErrorMessage: update.ErrorMessage,
			Reference: exchange.VenueReference{
				ClientOrderID: update.ClientOrderID, VenueOrderID: update.VenueOrderID,
				EventAt: update.EventTime, ReconcileStatus: status,
			},
		},
		EventAt: update.EventTime, ReceivedAt: time.Now(),
		SkipVenueWatermark: streamFillSkipsVenueWatermark(
			order.Exchange, update.Type, cumulative,
		),
	}
	fillQuantity := parseDecimal(last).Abs()
	if update.TradeID != "" && fillQuantity.IsPositive() {
		fillPrice := update.LastPrice
		if fillPrice == "" {
			fillPrice = update.AveragePrice
		}
		streamUpdate.Fills = []OrderFill{{
			TradeID: update.TradeID, Quantity: fillQuantity.String(),
			Price: fillPrice, ExecutedAt: update.EventTime,
		}}
	}
	return store.ApplyStreamUpdate(ctx, order.ID, streamUpdate)
}

func (r *Reconciler) releaseWatch(orderID string) {
	r.watchMu.Lock()
	delete(r.watches, orderID)
	r.watchMu.Unlock()
}

func (r *Reconciler) fail(ctx context.Context, order Order, err error) {
	failures := order.ReconcileFailures + 1
	delay := time.Second << min(failures, 6)
	if delay > time.Minute {
		delay = time.Minute
	}
	_ = r.store.MarkReconcileFailure(ctx, order.ID, time.Now().UTC().Add(delay))
	_ = r.store.AppendEvent(ctx, order.ID, "reconcile_failed", map[string]any{
		"error": sanitizeError(err), "retryIn": delay.String(),
	})
}
