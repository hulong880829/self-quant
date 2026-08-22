package trader

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/orderstream"
)

type reconcileStore interface {
	LeaseDueOrders(context.Context, int, time.Duration) ([]Order, error)
	MarkReconcileFailure(context.Context, string, time.Time) error
	UpdateResult(context.Context, string, VenueResult) (Order, error)
	AppendEvent(context.Context, string, string, map[string]any) error
	DeferReconcile(context.Context, string, time.Time) error
}

type internalCredentialProvider interface {
	GetInternal(context.Context, string, string, int64) (Credentials, error)
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
		workers: workers, logger: logger,
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
		queryCtx, cancel := context.WithTimeout(ctx, r.timeout)
		result, err := adapter.GetOrder(queryCtx, exchange.Credentials{
			APIKey: credentials.APIKey, APISecret: credentials.APISecret,
			Passphrase: credentials.Passphrase,
		}, exchange.QueryRequest{
			Instrument: toVenueInstrument(instrument), ClientOrderID: order.ClientOrderID,
			VenueOrderID: order.VenueOrderID,
		})
		cancel()
		if err != nil {
			r.fail(ctx, order, err)
			continue
		}
		result = normalizeVenueResult(order, result)
		if _, err := r.store.UpdateResult(ctx, order.ID, VenueResult{
			VenueOrderID: result.VenueOrderID, Status: result.Status,
			FilledQuantity: result.FilledQuantity, AveragePrice: result.AveragePrice,
			ErrorCode: result.ErrorCode, ErrorMessage: result.ErrorMessage,
		}); err != nil {
			r.fail(ctx, order, err)
			continue
		}
		_ = r.store.AppendEvent(ctx, order.ID, "reconcile", map[string]any{
			"status": result.Status, "filledQuantity": result.FilledQuantity,
			"venueOrderId": result.VenueOrderID,
		})
	}
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
