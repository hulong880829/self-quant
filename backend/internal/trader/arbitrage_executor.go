package trader

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/account/portfolio"
	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
	"selfquant/backend/internal/trader/orderstream"
)

type ArbitrageExecutor struct {
	store               arbitrageStore
	orders              arbitrageOrderStore
	service             *Service
	catalog             instrumentCatalog
	credentials         internalCredentialProvider
	venues              *exchange.Registry
	market              *marketdata.Manager
	portfolios          portfolioSnapshotProvider
	orderStreams        *orderstream.Manager
	token               string
	observerInterval    time.Duration
	repriceTicks        int
	iocBps              int
	iocRetries          int
	hedgeEmergencyAfter time.Duration
	timeout             time.Duration
	signalBBOStale      time.Duration
	streamAudit         time.Duration
	logger              *slog.Logger
	mu                  sync.Mutex
	active              map[string]*arbitrageRun
	spotSnapshots       map[string]map[int64]portfolio.Snapshot
	streamEvents        atomic.Uint64
	restFallbacks       atomic.Uint64
	lastStreamLagMS     atomic.Uint64
	lastHedgeLatency    atomic.Uint64
}

type ArbitrageExecutorStats struct {
	StreamEvents       uint64
	RESTFallbacks      uint64
	LastStreamLagMS    uint64
	LastHedgeLatencyMS uint64
}

type arbitrageExecutionLeg struct {
	leg         ArbitrageLeg
	instrument  Instrument
	credentials Credentials
}

func (e *ArbitrageExecutor) Stats() ArbitrageExecutorStats {
	return ArbitrageExecutorStats{
		StreamEvents: e.streamEvents.Load(), RESTFallbacks: e.restFallbacks.Load(),
		LastStreamLagMS:    e.lastStreamLagMS.Load(),
		LastHedgeLatencyMS: e.lastHedgeLatency.Load(),
	}
}

func (e *ArbitrageExecutor) ConfigureOrderStreams(
	manager *orderstream.Manager,
	auditInterval time.Duration,
) {
	e.orderStreams = manager
	if auditInterval <= 0 {
		auditInterval = 30 * time.Second
	}
	e.streamAudit = auditInterval
}

func (e *ArbitrageExecutor) ConfigurePortfolios(portfolios portfolioSnapshotProvider) {
	e.portfolios = portfolios
}

func (e *ArbitrageExecutor) ConfigureHedgeEmergencyAfter(after time.Duration) {
	if after < 0 {
		after = 0
	}
	e.hedgeEmergencyAfter = after
}

func (e *ArbitrageExecutor) ConfigureSignalBBOStale(d time.Duration) {
	if d > 0 {
		e.signalBBOStale = d
	}
}

type hedgeTiming struct {
	dispatchStartedAt time.Time
	emergencyAt       time.Time
}

type hedgeIOCAdmission struct {
	fastPath       bool
	makerFilled    string
	hedgeFilled    string
	aggregateCarry bool
	expectedCarry  string
}

type arbitrageRun struct {
	combinationID string
	cancel        context.CancelFunc
	done          chan struct{}
}

type makerOutcome string

const (
	makerTerminal        makerOutcome = "terminal"
	makerReprice         makerOutcome = "reprice"
	makerOpportunityGone makerOutcome = "opportunity_gone"
)

func makerPostOnlyReprice(instrument Instrument, order Order) bool {
	if parseDecimal(order.FilledQuantity).IsPositive() {
		return false
	}
	exchangeName := strings.ToLower(strings.TrimSpace(instrument.Exchange))
	code := strings.TrimSpace(order.ErrorCode)
	message := strings.TrimSpace(order.ErrorMessage)
	switch exchangeName {
	case "gate":
		return order.Status == "rejected" &&
			(code == "POC_FILL_IMMEDIATELY" || code == "ORDER_POC_IMMEDIATE")
	case "binance":
		if instrument.ContractType == "spot" {
			return order.Status == "rejected" && code == "-2010" &&
				message == "Order would immediately match and take."
		}
		return order.Status == "rejected" && code == "-5022"
	case "bybit":
		return order.Status == "rejected" &&
			code == "EC_PostOnlyWillTakeLiquidity"
	case "okx":
		return order.Status == "canceled" && code == "31"
	case "bitget":
		return order.Status == "canceled" &&
			code == "post_only_fill_cancel"
	default:
		return false
	}
}

func NewArbitrageExecutor(
	store arbitrageStore,
	orders arbitrageOrderStore,
	service *Service,
	catalog instrumentCatalog,
	credentials internalCredentialProvider,
	venues *exchange.Registry,
	market *marketdata.Manager,
	token string,
	observerInterval time.Duration,
	repriceTicks, iocBps, iocRetries int,
	timeout time.Duration,
	logger *slog.Logger,
) *ArbitrageExecutor {
	if logger == nil {
		logger = slog.Default()
	}
	return &ArbitrageExecutor{
		store: store, orders: orders, service: service, catalog: catalog,
		credentials: credentials, venues: venues, market: market, token: token,
		observerInterval: observerInterval, repriceTicks: repriceTicks,
		iocBps: iocBps, iocRetries: iocRetries, timeout: timeout,
		signalBBOStale: defaultSignalBBOStale,
		logger:         logger, active: make(map[string]*arbitrageRun),
		spotSnapshots: make(map[string]map[int64]portfolio.Snapshot),
	}
}

func (e *ArbitrageExecutor) Execute(
	parent context.Context,
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	bboA, bboB marketdata.BBO,
	permit *executionWorkPermit,
) {
	if permit == nil {
		permit = &executionWorkPermit{}
	}
	ctx, cancel := context.WithCancel(parent)
	run := &arbitrageRun{combinationID: combination.ID, cancel: cancel, done: make(chan struct{})}
	e.mu.Lock()
	e.active[execution.ID] = run
	e.mu.Unlock()
	defer func() {
		cancel()
		close(run.done)
		e.mu.Lock()
		delete(e.active, execution.ID)
		delete(e.spotSnapshots, execution.ID)
		e.mu.Unlock()
		permit.Close()
	}()

	started := time.Now()
	var err error
	switch combination.ExecutionMode {
	case "maker_then_hedge":
		err = e.executeMakerThenHedge(ctx, combination, &execution, bboA, bboB, permit)
	case "simultaneous_market":
		err = e.executeSimultaneousMarket(ctx, combination, &execution, bboA, bboB)
	default:
		err = fmt.Errorf("%w: unsupported arbitrage execution mode %q",
			ErrInvalidArgument, combination.ExecutionMode)
	}
	if err != nil {
		if errors.Is(err, ErrArbitrageReconciling) || errors.Is(err, errLastClipDustHandoff) {
			return
		}
		execution.ErrorMessage = sanitizeError(err)
		if errors.Is(err, ErrVenueRejected) {
			_ = e.store.AppendArbitrageEvent(
				context.Background(), combination.ID, execution.ID, "venue_rejected",
				map[string]any{"message": execution.ErrorMessage},
			)
		}
		if errorsIsCanceled(err) && !executionHasExposure(execution) {
			execution.Status = "canceled"
			_, _ = e.store.UpdateArbitrageExecution(context.Background(), execution)
			e.clearUnchangedArbitrageFailure(combination)
			return
		}
		closingClose := closingArbitrageExecution(combination, execution)
		if !executionHasExposure(execution) && !closingClose &&
			(errors.Is(err, ErrRiskLimit) || errors.Is(err, ErrOrderBelowMinimum)) {
			execution.Status = "canceled"
			execution.ErrorMessage = ""
			_, _ = e.store.UpdateArbitrageExecution(context.Background(), execution)
			e.clearUnchangedArbitrageFailure(combination)
			e.logger.Info("arbitrage execution skipped without exposure",
				"combination_id", combination.ID, "execution_id", execution.ID,
				"direction", execution.Direction, "reason", sanitizeError(err))
			return
		}
		if executionHasExposure(execution) {
			if execution.Status != "hedging" && execution.Status != "maker_open" &&
				execution.Status != "reconciling" {
				if execution.HedgeOrderID != "" {
					execution.Status = "hedging"
				} else {
					execution.Status = "reconciling"
				}
			}
			_, _ = e.store.UpdateArbitrageExecution(context.Background(), execution)
			if errors.Is(err, ErrArbitrageUnsafeHedge) {
				combination.PositionUncertain = true
				combination.RuntimeState = "position_uncertain"
				combination.ErrorMessage = execution.ErrorMessage
				_, _ = e.store.UpdateArbitrageCombinationRuntime(
					context.Background(), combination,
				)
				e.logger.Error("arbitrage execution requires position verification",
					"combination_id", combination.ID, "execution_id", execution.ID,
					"direction", execution.Direction, "error", sanitizeError(err))
				return
			}
			if closingClose {
				if failures, ok := e.store.(arbitrageCloseFailureStore); ok {
					_, _ = failures.RecordArbitrageCloseFailure(
						context.Background(), combination.ID, "close_recovery",
						execution.ErrorMessage,
					)
				}
			} else {
				_, _ = e.store.RecordArbitrageFailure(
					context.Background(), combination.ID, execution.ErrorMessage,
				)
			}
			e.logger.Error("arbitrage execution needs recovery",
				"combination_id", combination.ID, "execution_id", execution.ID,
				"direction", execution.Direction, "error", sanitizeError(err))
			return
		}
		execution.Status = "failed"
		_, _ = e.store.UpdateArbitrageExecution(context.Background(), execution)
		if closingClose {
			failureKey := "close_failed"
			if arbitrageDeterministicCloseReject(err) {
				failureKey = "close_rejected"
			}
			if failures, ok := e.store.(arbitrageCloseFailureStore); ok {
				_, _ = failures.RecordArbitrageCloseFailure(
					context.Background(), combination.ID, failureKey,
					execution.ErrorMessage,
				)
			}
			e.logger.Error("arbitrage close execution failed",
				"combination_id", combination.ID, "execution_id", execution.ID,
				"direction", execution.Direction, "error", sanitizeError(err))
			return
		}
		_, _ = e.store.RecordArbitrageFailure(
			context.Background(), combination.ID, execution.ErrorMessage,
		)
		e.logger.Error("arbitrage execution failed",
			"combination_id", combination.ID, "execution_id", execution.ID,
			"direction", execution.Direction, "error", sanitizeError(err))
		return
	}
	e.logger.Info("arbitrage execution complete",
		"combination_id", combination.ID, "execution_id", execution.ID,
		"direction", execution.Direction, "latency_ms", time.Since(started).Milliseconds(),
		"delta_notional", execution.DeltaNotional)
}

func closingArbitrageExecution(
	combination ArbitrageCombination,
	execution ArbitrageExecution,
) bool {
	return arbitrageFlattening(combination) &&
		strings.EqualFold(strings.TrimSpace(execution.PositionEffect), "close")
}

func arbitrageDeterministicCloseReject(err error) bool {
	return errors.Is(err, ErrRiskLimit) ||
		errors.Is(err, ErrOrderBelowMinimum) ||
		errors.Is(err, ErrVenueRejected) ||
		errors.Is(err, exchange.ErrRejected)
}

func (e *ArbitrageExecutor) clearUnchangedArbitrageFailure(
	combination ArbitrageCombination,
) {
	failures, ok := e.store.(arbitrageFailureStore)
	if !ok || combination.ErrorMessage == "" {
		return
	}
	_, _ = failures.ClearArbitrageFailureIfUnchanged(
		context.Background(), combination.ID, combination.ErrorMessage,
	)
}

func (e *ArbitrageExecutor) CloseCombination(
	ctx context.Context,
	combination ArbitrageCombination,
) error {
	e.mu.Lock()
	runs := make([]*arbitrageRun, 0)
	for _, run := range e.active {
		if run.combinationID == combination.ID {
			runs = append(runs, run)
			run.cancel()
		}
	}
	e.mu.Unlock()
	deadline := time.NewTimer(e.timeout)
	defer deadline.Stop()
	for _, run := range runs {
		select {
		case <-run.done:
		case <-deadline.C:
			return ErrVenueUncertain
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if latest, err := e.store.GetArbitrageCombinationByOwner(
		ctx, combination.OwnerUsername, combination.ID,
	); err == nil {
		combination = latest
	}
	orders, ordersErr := e.store.ListArbitrageOrders(
		ctx, combination.OwnerUsername, combination.ID,
	)
	if ordersErr != nil {
		return ordersErr
	}
	activeOrders := make([]Order, 0, len(orders))
	for _, order := range orders {
		if terminalStatus(order.Status) {
			continue
		}
		activeOrders = append(activeOrders, order)
	}
	var instrumentA, instrumentB Instrument
	var credentialsA, credentialsB Credentials
	var adapterA, adapterB exchange.Adapter
	if len(activeOrders) > 0 {
		var dependencyErr error
		instrumentA, credentialsA, adapterA, dependencyErr = e.executionDependencies(
			ctx, combination.OwnerUsername, combination.LegA,
		)
		if dependencyErr != nil {
			return dependencyErr
		}
		instrumentB, credentialsB, adapterB, dependencyErr = e.executionDependencies(
			ctx, combination.OwnerUsername, combination.LegB,
		)
		if dependencyErr != nil {
			return dependencyErr
		}
	}
	unresolvedOrders := make([]Order, 0)
	allConfirmedAbsent := true
	for _, order := range activeOrders {
		instrument, credentials, adapter := instrumentA, credentialsA, adapterA
		if order.ArbitrageLeg == "b" {
			instrument, credentials, adapter = instrumentB, credentialsB, adapterB
		}
		resolved, closeErr := e.closeArbitrageOrder(
			ctx, credentials, instrument, adapter, order,
		)
		switch {
		case closeErr == nil:
		case errors.Is(closeErr, ErrCloseOrderOpen):
			return closeErr
		case errors.Is(closeErr, ErrCloseOrderUncertain):
			unresolvedOrders = append(unresolvedOrders, resolved.order)
			allConfirmedAbsent = allConfirmedAbsent && resolved.confirmedAbsent
		default:
			return closeErr
		}
	}
	if len(unresolvedOrders) > 0 {
		uncertainAttempts := closeUncertainAttempts(combination)
		if !allConfirmedAbsent && uncertainAttempts < 10 {
			return fmt.Errorf(
				"%w: %d order(s) could not be confirmed",
				ErrCloseOrderUncertain, len(unresolvedOrders),
			)
		}
		for _, order := range unresolvedOrders {
			message := fmt.Sprintf(
				"%s order %s final order state could not be reconstructed during close",
				order.Exchange, closeOrderReference(order),
			)
			if _, err := e.orders.UpdateResult(ctx, order.ID, VenueResult{
				VenueOrderID: order.VenueOrderID, Status: "canceled",
				FilledQuantity: order.FilledQuantity, AveragePrice: order.AveragePrice,
				ErrorCode: "close_state_unresolved", ErrorMessage: message,
			}); err != nil {
				return err
			}
		}
		combination.PositionUncertain = true
		combination.RuntimeState = "position_uncertain"
		combination.ErrorMessage = fmt.Sprintf(
			"%s order %s final order state could not be reconstructed during close",
			unresolvedOrders[0].Exchange,
			closeOrderReference(unresolvedOrders[0]),
		)
	}
	executions, executionsErr := e.store.ListArbitrageExecutions(
		ctx, combination.ID, 100,
	)
	if executionsErr != nil {
		return executionsErr
	}
	finalizedExecutions := 0
	for _, execution := range executions {
		if terminalArbitrageExecutionStatus(execution.Status) {
			continue
		}
		execution.Status = "completed"
		execution.ErrorMessage = ""
		if _, updateErr := e.store.UpdateArbitrageExecution(ctx, execution); updateErr != nil {
			return updateErr
		}
		finalizedExecutions++
	}
	refreshed, refreshErr := e.store.RecomputeArbitrageBasePositions(
		ctx, combination.ID,
	)
	if refreshErr != nil {
		return refreshErr
	}
	combination = refreshed
	if len(unresolvedOrders) > 0 {
		combination.PositionUncertain = true
		combination.RuntimeState = "position_uncertain"
		combination.ErrorMessage = fmt.Sprintf(
			"%s order %s final order state could not be reconstructed during close",
			unresolvedOrders[0].Exchange,
			closeOrderReference(unresolvedOrders[0]),
		)
	}
	placeA, placeB, err := e.closeFlattenOwnedDecision(ctx, combination)
	if err != nil {
		return err
	}
	combination.Status = "closed"
	combination.MarketDataStale = true
	if combination.PositionUncertain {
		combination.RuntimeState = "position_uncertain"
	} else {
		combination.RuntimeState = "monitoring"
		combination.ErrorMessage = ""
	}
	if combination.ErrorMessage == "closing drain left non-zero base carry" {
		combination.ErrorMessage = ""
	}
	_, err = e.store.UpdateArbitrageCombinationRuntime(ctx, combination)
	if err != nil {
		return err
	}
	_ = e.store.AppendArbitrageEvent(ctx, combination.ID, "", "closed", map[string]any{
		"activeRuns":          len(runs),
		"drainedOrders":       len(activeOrders),
		"finalizedExecutions": finalizedExecutions,
		"legABasePosition":    combination.LegABasePosition,
		"legBBasePosition":    combination.LegBBasePosition,
		"carryBaseQuantity":   combination.CarryBaseQuantity,
		"unresolvedOrderIds":  closeOrderIDs(unresolvedOrders),
		"attempts":            closeUncertainAttempts(combination),
	})
	e.placeCloseFlattenOwnedLegs(ctx, combination, placeA, placeB)
	return nil
}

func (e *ArbitrageExecutor) closeFlattenOwnedDecision(
	ctx context.Context,
	combination ArbitrageCombination,
) (decimal.Decimal, decimal.Decimal, error) {
	if combinationLooksFlat(combination) {
		return decimal.Zero, decimal.Zero, nil
	}
	if e.catalog == nil {
		return decimal.Zero, decimal.Zero, fmt.Errorf(
			"%w: close flatten instruments are unavailable",
			ErrArbitrageNotClosable,
		)
	}
	instrumentA, errA := e.catalog.Get(ctx, combination.LegA.InstrumentID)
	instrumentB, errB := e.catalog.Get(ctx, combination.LegB.InstrumentID)
	if errA != nil || errB != nil {
		return decimal.Zero, decimal.Zero, fmt.Errorf(
			"%w: close flatten instruments are unavailable",
			ErrArbitrageNotClosable,
		)
	}
	orders, err := e.store.ListArbitrageOrders(
		ctx, combination.OwnerUsername, combination.ID,
	)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	now := time.Now().UTC()
	var bboA, bboB marketdata.BBO
	if e.market != nil {
		if latest, errA := e.latestForLeg(combination.LegA); errA == nil {
			bboA = latest
		}
		if latest, errB := e.latestForLeg(combination.LegB); errB == nil {
			bboB = latest
		}
	}
	tinyIn := closeFlattenTinyOwnedInput{
		Combination:    combination,
		InstrumentA:    instrumentA,
		InstrumentB:    instrumentB,
		BBOA:           bboA,
		BBOB:           bboB,
		Now:            now,
		LatestOrderAt:  latestOrderUpdatedAt(orders),
		RequireOrders:  true,
		SignalBBOStale: e.signalBBOStale,
	}
	if closeFlattenTinyOwnedWaitingSnapshot(tinyIn) {
		return decimal.Zero, decimal.Zero, ErrArbitrageCloseWaitingSnapshot
	}
	if !closeFlattenTinyOwnedPositions(tinyIn) {
		return decimal.Zero, decimal.Zero, fmt.Errorf(
			"%w: combination-owned leftover is not a tiny close flatten",
			ErrArbitrageNotClosable,
		)
	}
	return closeFlattenOwnedPlaceBase(combination, instrumentA, "a"),
		closeFlattenOwnedPlaceBase(combination, instrumentB, "b"),
		nil
}

func (e *ArbitrageExecutor) placeCloseFlattenOwnedLegs(
	ctx context.Context,
	combination ArbitrageCombination,
	baseA, baseB decimal.Decimal,
) {
	if combination.PositionUncertain {
		return
	}
	if e.catalog == nil || e.credentials == nil || e.venues == nil {
		return
	}
	if !baseA.IsZero() {
		instrumentA, credentialsA, adapterA, err := e.executionDependencies(
			ctx, combination.OwnerUsername, combination.LegA,
		)
		if err != nil {
			e.logger.Info("close flatten skipped leg",
				"combination_id", combination.ID, "leg", "a", "error", err)
		} else {
			e.placeCloseFlattenLeg(
				ctx, combination, "a", combination.LegA, instrumentA, credentialsA, adapterA,
				baseA,
			)
		}
	}
	if !baseB.IsZero() {
		instrumentB, credentialsB, adapterB, err := e.executionDependencies(
			ctx, combination.OwnerUsername, combination.LegB,
		)
		if err != nil {
			e.logger.Info("close flatten skipped leg",
				"combination_id", combination.ID, "leg", "b", "error", err)
		} else {
			e.placeCloseFlattenLeg(
				ctx, combination, "b", combination.LegB, instrumentB, credentialsB, adapterB,
				baseB,
			)
		}
	}
}

func (e *ArbitrageExecutor) placeCloseFlattenLeg(
	ctx context.Context,
	combination ArbitrageCombination,
	legName string,
	leg ArbitrageLeg,
	instrument Instrument,
	credentials Credentials,
	adapter exchange.Adapter,
	base decimal.Decimal,
) {
	quantity := closeFlattenQuantity(instrument, base)
	if !quantity.IsPositive() {
		return
	}
	venueInstrument := toVenueInstrument(instrument)
	if _, err := exchange.ToVenueQuantity(venueInstrument, quantity.String()); err != nil {
		e.logger.Info("close flatten skipped leg",
			"combination_id", combination.ID, "leg", legName,
			"exchange", instrument.Exchange, "quantity", quantity.String(),
			"reason", "invalid_venue_quantity", "error", err)
		return
	}
	timeout := e.timeout
	if timeout <= 0 {
		timeout = 12 * time.Second
	}
	placeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request := exchange.OrderRequest{
		Instrument:    venueInstrument,
		ClientOrderID: clientOrderID(),
		Side:          closeFlattenSide(base),
		OrderType:     "market",
		Quantity:      quantity.String(),
		ReduceOnly:    true,
	}
	accountID := credentials.TradingAccountID
	if accountID == 0 {
		accountID = leg.TradingAccountID
	}
	if e.service != nil {
		lock := e.service.accountLock(accountID)
		lock.Lock()
		defer lock.Unlock()
	}
	result, err := adapter.PlaceOrder(placeCtx, toVenueCredentials(credentials), request)
	if err != nil {
		e.logger.Info("close flatten place failed",
			"combination_id", combination.ID, "leg", legName,
			"exchange", instrument.Exchange, "side", request.Side,
			"quantity", request.Quantity, "error", err)
		return
	}
	e.logger.Info("close flatten placed",
		"combination_id", combination.ID, "leg", legName,
		"exchange", instrument.Exchange, "side", request.Side,
		"quantity", request.Quantity, "status", result.Status,
		"venue_order_id", result.VenueOrderID)
}

type closeOrderResolution struct {
	order           Order
	confirmedAbsent bool
}

func (e *ArbitrageExecutor) closeArbitrageOrder(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	adapter exchange.Adapter,
	order Order,
) (closeOrderResolution, error) {
	if terminalStatus(order.Status) {
		return closeOrderResolution{order: order}, nil
	}
	if order.Status == "unknown" {
		queried, err := e.queryInternal(ctx, credentials, instrument, adapter, order)
		if err != nil {
			if errors.Is(err, exchange.ErrOrderNotFound) {
				resolved, resolveErr := e.resolveMissingCloseOrder(
					ctx, credentials, instrument, adapter, order,
				)
				if errors.Is(resolveErr, ErrCloseOrderOpen) &&
					cancelableStatus(resolved.order.Status) &&
					resolved.order.Status != "unknown" {
					order = resolved.order
				} else {
					return resolved, resolveErr
				}
			} else {
				return closeOrderResolution{order: order}, fmt.Errorf(
					"%w: query %s order %s: %v",
					ErrCloseOrderUncertain, order.Exchange, order.ID, err,
				)
			}
		} else {
			order = queried
		}
		if terminalStatus(order.Status) {
			return closeOrderResolution{order: order}, nil
		}
		if order.Status == "unknown" {
			return closeOrderResolution{order: order}, fmt.Errorf(
				"%w: %s order %s remains unknown",
				ErrCloseOrderUncertain, order.Exchange, order.ID,
			)
		}
	}
	if !cancelableStatus(order.Status) {
		return closeOrderResolution{order: order}, fmt.Errorf(
			"%w: %s order %s has status %s",
			ErrCloseOrderUncertain, order.Exchange, order.ID, order.Status,
		)
	}
	canceled, err := e.cancelInternal(ctx, credentials, instrument, adapter, order)
	if terminalStatus(canceled.Status) {
		return closeOrderResolution{order: canceled}, nil
	}
	if errors.Is(err, exchange.ErrOrderNotFound) {
		resolution, resolveErr := e.resolveMissingCloseOrder(
			ctx, credentials, instrument, adapter, order,
		)
		if resolveErr == nil || resolution.confirmedAbsent {
			return resolution, resolveErr
		}
		if terminalStatus(resolution.order.Status) {
			return resolution, nil
		}
		if cancelableStatus(resolution.order.Status) &&
			resolution.order.Status != "unknown" {
			return resolution, fmt.Errorf(
				"%w: %s order %s remains %s after cancel failed",
				ErrCloseOrderOpen, order.Exchange, order.ID, resolution.order.Status,
			)
		}
		return resolution, resolveErr
	}
	queried, queryErr := e.queryInternal(ctx, credentials, instrument, adapter, canceled)
	if queryErr != nil {
		if errors.Is(queryErr, exchange.ErrOrderNotFound) {
			return e.resolveMissingCloseOrder(ctx, credentials, instrument, adapter, canceled)
		}
		return closeOrderResolution{order: canceled}, fmt.Errorf(
			"%w: confirm canceled %s order %s: %v",
			ErrCloseOrderUncertain, order.Exchange, order.ID, queryErr,
		)
	}
	if terminalStatus(queried.Status) {
		return closeOrderResolution{order: queried}, nil
	}
	if cancelableStatus(queried.Status) && queried.Status != "unknown" {
		return closeOrderResolution{order: queried}, fmt.Errorf(
			"%w: %s order %s remains %s",
			ErrCloseOrderOpen, order.Exchange, order.ID, queried.Status,
		)
	}
	return closeOrderResolution{order: queried}, fmt.Errorf(
		"%w: %s order %s remains %s",
		ErrCloseOrderUncertain, order.Exchange, order.ID, queried.Status,
	)
}

func (e *ArbitrageExecutor) resolveMissingCloseOrder(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	adapter exchange.Adapter,
	order Order,
) (closeOrderResolution, error) {
	resolver, ok := adapter.(exchange.OrderResolver)
	if !ok {
		return closeOrderResolution{order: order}, fmt.Errorf(
			"%w: %s order %s was not found",
			ErrCloseOrderUncertain, order.Exchange, order.ID,
		)
	}
	resolution, err := resolver.ResolveOrder(ctx, toVenueCredentials(credentials), exchange.QueryRequest{
		Instrument: toVenueInstrument(instrument), ClientOrderID: order.ClientOrderID,
		VenueOrderID: order.VenueOrderID, CreatedAt: order.CreatedAt,
	})
	if err != nil {
		return closeOrderResolution{order: order}, fmt.Errorf(
			"%w: resolve %s order %s: %v",
			ErrCloseOrderUncertain, order.Exchange, order.ID, err,
		)
	}
	if resolution.Found {
		updated, persistErr := e.service.persistResult(
			ctx, order.ID, "arbitrage_close_resolve",
			normalizeVenueResult(order, resolution.Result),
		)
		if persistErr != nil {
			return closeOrderResolution{order: order}, persistErr
		}
		if terminalStatus(updated.Status) {
			return closeOrderResolution{order: updated}, nil
		}
		return closeOrderResolution{order: updated}, fmt.Errorf(
			"%w: %s order %s resolved as %s",
			ErrCloseOrderOpen, order.Exchange, order.ID, updated.Status,
		)
	}
	if resolution.ConfirmedAbsent {
		return closeOrderResolution{
				order: order, confirmedAbsent: true,
			}, fmt.Errorf(
				"%w: %s order %s absent from history and active orders",
				ErrCloseOrderUncertain, order.Exchange, order.ID,
			)
	}
	return closeOrderResolution{order: order}, fmt.Errorf(
		"%w: %s order %s could not be resolved",
		ErrCloseOrderUncertain, order.Exchange, order.ID,
	)
}

func closeOrderIDs(orders []Order) []string {
	ids := make([]string, 0, len(orders))
	for _, order := range orders {
		ids = append(ids, order.ID)
	}
	return ids
}

func closeUncertainAttempts(combination ArbitrageCombination) int {
	if combination.LastFailureKey == "close_order_uncertain" {
		return combination.RepeatedFailureCount + 1
	}
	return 1
}

func closeOrderReference(order Order) string {
	for _, value := range []string{order.VenueOrderID, order.ClientOrderID, order.ID} {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "unknown"
}

func terminalArbitrageExecutionStatus(status string) bool {
	switch status {
	case "completed", "failed", "canceled", "dry_run":
		return true
	default:
		return false
	}
}

func (e *ArbitrageExecutor) RecoverWithoutMarketData(
	ctx context.Context,
	combination ArbitrageCombination,
	execution ArbitrageExecution,
) error {
	if handoffErr := e.maybeHandoffLastClipHedgeMinNotional(
		ctx, combination, &execution, errors.New(execution.ErrorMessage),
	); handoffErr != nil {
		if errors.Is(handoffErr, errLastClipDustHandoff) {
			return nil
		}
		return handoffErr
	}
	instrumentA, credentialsA, adapterA, err := e.executionDependencies(
		ctx, combination.OwnerUsername, combination.LegA,
	)
	if err != nil {
		return err
	}
	instrumentB, credentialsB, adapterB, err := e.executionDependencies(
		ctx, combination.OwnerUsername, combination.LegB,
	)
	if err != nil {
		return err
	}
	seenConfirmedAbsent := false
	for _, orderID := range []string{execution.MakerOrderID, execution.HedgeOrderID} {
		if orderID == "" {
			continue
		}
		order, getErr := e.orders.GetByOwner(
			ctx, combination.OwnerUsername, orderID,
		)
		if getErr != nil {
			return getErr
		}
		instrument, credentials, adapter := instrumentA, credentialsA, adapterA
		if order.ArbitrageLeg == "b" {
			instrument, credentials, adapter = instrumentB, credentialsB, adapterB
		}
		if confirmedAbsentReliableZeroFill(order) {
			seenConfirmedAbsent = true
			continue
		}
		alreadyConfirming := execution.Status == "maker_canceling" ||
			execution.Status == "reconciling" ||
			combination.RuntimeState == "maker_canceling" ||
			combination.RuntimeState == "reconciling"
		if order.ArbitrageRole == "maker" && !terminalStatus(order.Status) {
			if alreadyConfirming {
				continue
			}
			order, getErr = e.cancelInternal(
				ctx, credentials, instrument, adapter, order,
			)
			if !terminalStatus(order.Status) {
				shouldQuery := getErr == nil ||
					errors.Is(getErr, exchange.ErrAmbiguousCancel) ||
					errors.Is(getErr, exchange.ErrUncertain) ||
					errors.Is(getErr, exchange.ErrOrderNotFound) ||
					errors.Is(getErr, context.DeadlineExceeded)
				if shouldQuery {
					queried, queryErr := e.queryInternal(
						ctx, credentials, instrument, adapter, order,
					)
					if queryErr == nil {
						order = queried
					}
				}
			}
		}
		if order.ArbitrageRole == "maker" && !terminalStatus(order.Status) {
			continue
		}
		if order.ArbitrageRole != "maker" {
			order, getErr = e.settleCanceledOrder(
				ctx, credentials, instrument, adapter, order, orderObservation{},
			)
			if getErr != nil || !terminalStatus(order.Status) {
				if getErr != nil {
					return getErr
				}
				return ErrVenueUncertain
			}
		}
		if !terminalStatus(order.Status) {
			continue
		}
		if confirmedAbsentReliableZeroFill(order) {
			seenConfirmedAbsent = true
		}
	}
	if seenConfirmedAbsent {
		result, finErr := e.store.FinalizeConfirmedAbsentZeroFillExecution(
			ctx, execution.ID,
		)
		if finErr != nil {
			return finErr
		}
		switch result {
		case confirmedAbsentFinalizeRecovered, confirmedAbsentFinalizeCanceledExec:
			return nil
		}
		if _, activeErr := e.store.GetActiveArbitrageExecution(
			ctx, combination.ID,
		); errors.Is(activeErr, ErrNotFound) {
			return nil
		}
	}
	refreshed, err := e.refreshArbitragePositions(ctx, combination, &execution)
	if err != nil {
		return err
	}
	execution.Status = "reconciling"
	execution.ErrorMessage = ""
	if _, err := e.store.UpdateArbitrageExecution(ctx, execution); err != nil {
		if errors.Is(err, ErrArbitrageExecutionTerminal) {
			return nil
		}
		return err
	}
	refreshed.RuntimeState = "reconciling"
	refreshed.ErrorMessage = ""
	_, err = e.store.UpdateArbitrageCombinationRuntime(ctx, refreshed)
	return err
}

func (e *ArbitrageExecutor) executeMakerThenHedge(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	bboA, bboB marketdata.BBO,
	permit *executionWorkPermit,
) error {
	aSide, bSide, ok := arbitrageLegSides(execution.Direction)
	if !ok {
		return fmt.Errorf("%w: unsupported arbitrage direction %q for maker_then_hedge",
			ErrInvalidArgument, execution.Direction)
	}
	makerLeg, hedgeLeg := combination.LegA, combination.LegB
	makerSide := aSide
	hedgeSide := bSide
	makerLegName, hedgeLegName := "a", "b"
	makerBBO := bboA
	if combination.MakerLeg == "b" {
		makerLeg, hedgeLeg = combination.LegB, combination.LegA
		makerSide = bSide
		hedgeSide = aSide
		makerLegName, hedgeLegName = "b", "a"
		makerBBO = bboB
	}
	makerInstrument, makerCredentials, makerAdapter, err := e.executionDependencies(
		ctx, combination.OwnerUsername, makerLeg,
	)
	if err != nil {
		return err
	}
	hedgeInstrument, hedgeCredentials, hedgeAdapter, err := e.executionDependencies(
		ctx, combination.OwnerUsername, hedgeLeg,
	)
	if err != nil {
		return err
	}
	if err := warmArbitrageOrderTransport(
		ctx, makerAdapter, makerCredentials, makerInstrument,
	); err != nil {
		return err
	}
	if err := warmArbitrageOrderTransport(
		ctx, hedgeAdapter, hedgeCredentials, hedgeInstrument,
	); err != nil {
		return err
	}
	makerOrders := e.subscribeOrders(ctx, makerCredentials, makerInstrument)
	if makerOrders != nil {
		defer makerOrders.Close()
	}
	hedgeOrders := e.subscribeOrders(ctx, hedgeCredentials, hedgeInstrument)
	if hedgeOrders != nil {
		defer hedgeOrders.Close()
	}

	latest, refreshErr := e.refreshArbitragePositions(ctx, combination, execution)
	if refreshErr != nil {
		return refreshErr
	}
	combination = latest
	if err := e.prefetchReduceSpotSnapshots(
		ctx, combination, *execution,
		arbitrageExecutionLeg{
			leg: makerLeg, instrument: makerInstrument, credentials: makerCredentials,
		},
		arbitrageExecutionLeg{
			leg: hedgeLeg, instrument: hedgeInstrument, credentials: hedgeCredentials,
		},
	); err != nil {
		return err
	}
	enteredDust := strings.EqualFold(
		strings.TrimSpace(combination.RuntimeState), "hedge_deferred_dust",
	)
	startCarry := parseDecimal(combination.CarryBaseQuantity)
	hedgeBBO := bboB
	if combination.MakerLeg == "b" {
		hedgeBBO = bboA
	}
	if (execution.Status == "hedging" || execution.Status == "reconciling") &&
		execution.HedgeOrderID != "" {
		recovering, getErr := e.orders.GetByOwner(
			ctx, combination.OwnerUsername, execution.HedgeOrderID,
		)
		if getErr != nil {
			return getErr
		}
		watch := e.watchOrder(ctx, hedgeOrders, hedgeAdapter, recovering)
		recovering, observeErr := e.observeToTerminal(
			ctx, hedgeCredentials, hedgeInstrument, hedgeAdapter, recovering,
			orderObservation{subscription: hedgeOrders, watch: watch},
		)
		if observeErr != nil || !terminalStatus(recovering.Status) {
			execution.Status = "reconciling"
			_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
			return ErrVenueUncertain
		}
		if handoffErr := e.maybeHandoffLastClipHedgeMinNotional(
			ctx, combination, execution, errors.New(recovering.ErrorMessage),
		); handoffErr != nil {
			return handoffErr
		}
		combination, refreshErr = e.refreshArbitragePositions(ctx, combination, execution)
		if refreshErr != nil {
			return refreshErr
		}
	}
	if carry := parseDecimal(combination.CarryBaseQuantity); !carry.IsZero() &&
		execution.MakerOrderID == "" {
		skipStandalone := false
		if enteredDust {
			eligible, eligErr := e.carryHedgeExecutableAtBBO(
				combination, *execution, hedgeInstrument, hedgeBBO,
			)
			if eligErr != nil {
				return eligErr
			}
			if !eligible {
				skipStandalone = true
			}
		}
		if lastCloseClipMustMatchFills(combination, *execution) {
			ready, payload := lastCloseStandaloneCarryReady(
				combination, *execution, hedgeLeg, hedgeInstrument, hedgeBBO,
			)
			if !ready {
				skipStandalone = true
				_ = e.store.AppendArbitrageEvent(
					ctx, combination.ID, execution.ID, lastCloseCarryDeferredEvent, payload,
				)
			}
		}
		if !skipStandalone {
			hedged, hedgeErr := e.hedgeCarry(
				ctx, &combination, execution, hedgeLeg, hedgeInstrument,
				hedgeCredentials, hedgeAdapter, hedgeOrders, time.Now(),
			)
			if hedgeErr != nil {
				return hedgeErr
			}
			if hedged {
				mark := parsePositiveDecimal(bboA.AskPrice).
					Add(parsePositiveDecimal(bboB.BidPrice)).
					Div(decimal.NewFromInt(2))
				return e.completeExecution(ctx, combination, execution, mark)
			}
		}
	}

	price := makerBBO.BidPrice
	if makerSide == "sell" {
		price = makerBBO.AskPrice
	}
	price = passiveMakerPrice(price, makerSide, parsePositiveDecimal(makerInstrument.PriceTick))
	priceValue := parsePositiveDecimal(price)
	step := commonQuantityStep(
		parsePositiveDecimal(makerInstrument.QuantityStep),
		parsePositiveDecimal(hedgeInstrument.QuantityStep),
	)
	if !step.IsPositive() {
		return ErrInstrumentUnavailable
	}
	var stored Order
	created := false
	var qty decimal.Decimal
	if execution.MakerOrderID != "" {
		stored, err = e.orders.GetByOwner(
			ctx, combination.OwnerUsername, execution.MakerOrderID,
		)
		if err != nil {
			return err
		}
		price = stored.Price
		priceValue = parsePositiveDecimal(price)
		qty = parsePositiveDecimal(stored.Quantity)
	} else {
		qty = parsePositiveDecimal(execution.TargetBaseQuantity)
		if !qty.IsPositive() {
			qty = baseQuantityForNotional(
				executionNotional(combination, *execution), priceValue, step,
			)
		}
		roundBaseCap := qty
		if !parsePositiveDecimal(execution.TargetBaseQuantity).IsPositive() {
			roundBaseCap = executionNotional(combination, *execution).Div(priceValue)
		}
		dustNow := strings.EqualFold(
			strings.TrimSpace(combination.RuntimeState), "hedge_deferred_dust",
		)
		if !(dustNow && carrySameSignAsMaker(
			parseDecimal(combination.CarryBaseQuantity), makerSide,
		)) {
			qty = makerQuantityWithinCarryBudget(
				qty,
				parseDecimal(combination.CarryBaseQuantity),
				makerSide,
				roundBaseCap,
				step,
			)
		}
		if execution.ReduceOnly &&
			strings.EqualFold(strings.TrimSpace(execution.PositionEffect), "close") {
			qty = decimal.Min(qty, arbitrageCloseableBase(combination))
			qty = decimal.Min(
				qty,
				arbitrageReduceLegAvailable(combination, makerLegName, makerSide),
			)
			qty = decimal.Min(
				qty,
				arbitrageReduceLegAvailable(combination, hedgeLegName, hedgeSide),
			)
			if makerInstrument.ContractType == "spot" {
				snapshot, snapshotErr := e.bidSpotSnapshot(
					ctx, execution.ID, makerLeg, makerCredentials,
				)
				if snapshotErr != nil {
					return fmt.Errorf("%w: read spot balance: %v",
						ErrVenueUnavailable, snapshotErr)
				}
				qty = floorToStep(decimal.Min(
					qty,
					spotAvailableForSide(
						snapshot, makerLeg.BaseAsset, makerSide,
					),
				), step)
			}
			if hedgeInstrument.ContractType == "spot" {
				snapshot, snapshotErr := e.bidSpotSnapshot(
					ctx, execution.ID, hedgeLeg, hedgeCredentials,
				)
				if snapshotErr != nil {
					return fmt.Errorf("%w: read spot balance: %v",
						ErrVenueUnavailable, snapshotErr)
				}
				qty = decimal.Min(
					qty,
					spotAvailableForSide(
						snapshot, hedgeLeg.BaseAsset, hedgeSide,
					),
				)
			}
			qty = floorToStep(qty, step)
		}
		if strings.EqualFold(execution.PositionEffect, "close") && execution.ReduceOnly &&
			carrySameSignAsMaker(parseDecimal(combination.CarryBaseQuantity), makerSide) {
			expected := expectedCarryAfterMaker(
				parseDecimal(combination.CarryBaseQuantity), qty, makerSide,
			)
			closeable := arbitrageCloseableBase(combination)
			closeable = decimal.Min(
				closeable,
				arbitrageReduceLegAvailable(combination, hedgeLegName, hedgeSide),
			)
			if expected.Abs().GreaterThan(closeable) {
				return ErrRiskLimit
			}
		}
		if !qty.IsPositive() {
			return ErrRiskLimit
		}
	}
	execution.TargetBaseQuantity = qty.String()
	var fillConfirmedAt time.Time
	applyMakerFill := func(order Order) error {
		makerFilled := parseDecimal(order.FilledQuantity)
		previous := parseDecimal(execution.LegAFilledQuantity)
		if combination.MakerLeg == "b" {
			previous = parseDecimal(execution.LegBFilledQuantity)
		}
		if makerFilled.LessThan(previous) {
			makerFilled = previous
		}
		if makerFilled.Equal(previous) {
			return nil
		}
		if combination.MakerLeg == "b" {
			execution.LegBFilledQuantity = makerFilled.String()
		} else {
			execution.LegAFilledQuantity = makerFilled.String()
		}
		if fillConfirmedAt.IsZero() && makerFilled.IsPositive() {
			fillConfirmedAt = time.Now()
		}
		if !terminalStatus(order.Status) {
			_, err := e.store.UpdateArbitrageExecution(context.Background(), *execution)
			return err
		}
		return nil
	}

	if stored.ID == "" {
		preparedQuantity, preparedPrice, prepareErr := prepareArbitrageOrder(
			makerInstrument, "limit", qty.String(), price, price, *execution,
		)
		if prepareErr != nil {
			return prepareErr
		}
		qty = parsePositiveDecimal(preparedQuantity)
		price = preparedPrice
		intent := e.orderIntent(
			combination, *execution, makerLeg, makerInstrument,
			makerSide, "limit", qty.String(), price, "maker", 0,
		)
		stored, created, err = e.orders.CreateIntent(ctx, intent)
	}
	if err != nil {
		return err
	}
	if !created {
		price = stored.Price
		qty = parsePositiveDecimal(stored.Quantity)
	}
	execution.TargetBaseQuantity = qty.String()
	execution.MakerOrderID = stored.ID
	if execution.HedgeOrderID == "" {
		execution.Status = "maker_open"
	} else {
		execution.Status = "reconciling"
	}
	if _, err := e.store.UpdateArbitrageExecution(ctx, *execution); err != nil {
		return err
	}
	combination.RuntimeState = execution.Status
	combination.ErrorMessage = ""
	combination, _ = e.store.UpdateArbitrageCombinationRuntime(ctx, combination)

	order := stored
	makerWatch := e.watchOrder(ctx, makerOrders, makerAdapter, stored)
	observation := orderObservation{subscription: makerOrders, watch: makerWatch}
	restReprice := false
	if created {
		lock := e.service.accountLock(makerCredentials.TradingAccountID)
		lock.Lock()
		order, err = e.service.submitWithOptions(
			ctx, makerAdapter, makerCredentials, makerInstrument, stored, "GTC", true,
		)
		lock.Unlock()
		if err != nil {
			if errors.Is(err, ErrVenueRejected) &&
				makerPostOnlyReprice(makerInstrument, order) {
				restReprice = true
				err = nil
			} else {
				if errors.Is(err, ErrVenueRejected) {
					execution.Status = "failed"
					_, _ = e.store.UpdateArbitrageExecution(
						context.Background(), *execution,
					)
				}
				return err
			}
		}
	} else if stored.Status == "pending" {
		order, err = e.service.recover(
			ctx, makerAdapter, makerCredentials, makerInstrument, stored,
		)
		if err != nil {
			if errors.Is(err, ErrVenueRejected) {
				execution.Status = "failed"
				_, _ = e.store.UpdateArbitrageExecution(
					context.Background(), *execution,
				)
			}
			return err
		}
	}
	restReprice = restReprice || makerPostOnlyReprice(makerInstrument, order)
	if !restReprice {
		_ = e.store.AppendArbitrageEvent(ctx, combination.ID, execution.ID, "maker_accepted", map[string]any{
			"leg": combination.MakerLeg, "side": makerSide, "price": price,
			"quantity": qty.String(),
		})
	}
	outcome := makerTerminal
	if restReprice {
		outcome = makerReprice
	} else {
		var observeErr error
		order, outcome, observeErr = e.observeMaker(
			ctx, combination, execution, makerLeg, makerInstrument, makerCredentials,
			makerAdapter, order, makerSide, price, execution.Direction,
			observation, applyMakerFill, permit,
		)
		if errors.Is(observeErr, ErrArbitrageReconciling) {
			return observeErr
		}
		if observeErr != nil && !errorsIsCanceled(observeErr) {
			return observeErr
		}
	}
	if makerPostOnlyReprice(makerInstrument, order) {
		outcome = makerReprice
	} else if order.Status == "rejected" {
		message := strings.TrimSpace(strings.Join(
			[]string{order.ErrorCode, order.ErrorMessage},
			" ",
		))
		if message == "" {
			message = "maker order rejected"
		}
		return fmt.Errorf("%w: %s", ErrVenueRejected, message)
	}
	if ctx.Err() != nil && !parseDecimal(order.FilledQuantity).IsPositive() &&
		execution.Status != "reconciling" && execution.Status != "maker_canceling" {
		execution.Status = "canceled"
		_, _ = e.store.UpdateArbitrageExecution(context.Background(), *execution)
		return nil
	}
	if err := applyMakerFill(order); err != nil {
		return err
	}
	appendMakerTerminal := func() {
		_ = e.store.AppendArbitrageEvent(
			context.Background(), combination.ID, execution.ID, "maker_terminal",
			map[string]any{
				"status": order.Status, "filledQuantity": order.FilledQuantity,
				"outcome": string(outcome),
			},
		)
	}
	switch outcome {
	case makerReprice:
		_ = e.store.AppendArbitrageEvent(
			context.Background(), combination.ID, execution.ID, "maker_requote",
			map[string]any{"filledQuantity": order.FilledQuantity},
		)
	case makerOpportunityGone:
		_ = e.store.AppendArbitrageEvent(
			context.Background(), combination.ID, execution.ID, "opportunity_gone",
			map[string]any{"filledQuantity": order.FilledQuantity},
		)
	}
	if !parseDecimal(order.FilledQuantity).IsPositive() {
		appendMakerTerminal()
		combination, err = e.refreshArbitragePositions(
			context.Background(), combination, execution,
		)
		if err != nil {
			return err
		}
		if outcome == makerReprice {
			combination.RuntimeState = "repricing"
		} else if outcome == makerOpportunityGone {
			combination.RuntimeState = "opportunity_gone"
		} else {
			combination.RuntimeState = "monitoring"
		}
		combination.ErrorMessage = ""
		_, _ = e.store.UpdateArbitrageCombinationRuntime(context.Background(), combination)
		return e.completeExecution(context.Background(), combination, execution, priceValue)
	}
	hedgeStartedAt := time.Now()
	timing := hedgeTiming{dispatchStartedAt: hedgeStartedAt}
	if e.hedgeEmergencyAfter > 0 {
		timing.emergencyAt = hedgeStartedAt.Add(e.hedgeEmergencyAfter)
	}
	if enteredDust && carrySameSignAsMaker(startCarry, makerSide) {
		_, err = e.hedgeCarryWithTiming(
			context.Background(), &combination, execution, hedgeLeg, hedgeInstrument,
			hedgeCredentials, hedgeAdapter, hedgeOrders, timing,
		)
	} else {
		_, err = e.hedgeMakerCarryWithDeadline(
			context.Background(), &combination, execution, hedgeLeg, hedgeInstrument,
			hedgeCredentials, hedgeAdapter, hedgeOrders, hedgeSide, timing,
		)
	}
	appendMakerTerminal()
	if err != nil {
		if errors.Is(err, errLastClipDustHandoff) {
			return err
		}
		if handoffErr := e.maybeHandoffLastClipHedgeMinNotional(
			context.Background(), combination, execution, err,
		); handoffErr != nil {
			return handoffErr
		}
		return err
	}
	return e.completeExecution(context.Background(), combination, execution, priceValue)
}

func (e *ArbitrageExecutor) executeSimultaneousMarket(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	bboA, bboB marketdata.BBO,
) error {
	aSide, bSide, ok := arbitrageLegSides(execution.Direction)
	if !ok {
		return fmt.Errorf("%w: unsupported arbitrage direction %q for simultaneous_market",
			ErrInvalidArgument, execution.Direction)
	}
	instrumentA, credentialsA, adapterA, err := e.executionDependencies(
		ctx, combination.OwnerUsername, combination.LegA,
	)
	if err != nil {
		return err
	}
	instrumentB, credentialsB, adapterB, err := e.executionDependencies(
		ctx, combination.OwnerUsername, combination.LegB,
	)
	if err != nil {
		return err
	}
	updatedCombination, refreshErr := e.refreshArbitragePositions(
		ctx, combination, execution,
	)
	if refreshErr != nil {
		return refreshErr
	}
	combination = updatedCombination
	if err := e.prefetchReduceSpotSnapshots(
		ctx, combination, *execution,
		arbitrageExecutionLeg{
			leg: combination.LegA, instrument: instrumentA, credentials: credentialsA,
		},
		arbitrageExecutionLeg{
			leg: combination.LegB, instrument: instrumentB, credentials: credentialsB,
		},
	); err != nil {
		return err
	}
	ordersA := e.subscribeOrders(ctx, credentialsA, instrumentA)
	if ordersA != nil {
		defer ordersA.Close()
	}
	ordersB := e.subscribeOrders(ctx, credentialsB, instrumentB)
	if ordersB != nil {
		defer ordersB.Close()
	}
	priceA := parsePositiveDecimal(bboA.AskPrice)
	if aSide == "sell" {
		priceA = parsePositiveDecimal(bboA.BidPrice)
	}
	priceB := parsePositiveDecimal(bboB.AskPrice)
	if bSide == "sell" {
		priceB = parsePositiveDecimal(bboB.BidPrice)
	}
	notional := executionNotional(combination, *execution)
	orderTypeA, orderTypeB := arbitrageOrderTypes(combination)
	stepA := instrumentOrderStep(instrumentA, orderTypeA)
	stepB := instrumentOrderStep(instrumentB, orderTypeB)
	qtyA := parsePositiveDecimal(execution.TargetBaseQuantity)
	qtyB := qtyA
	if !qtyA.IsPositive() {
		qtyA = baseQuantityForNotional(notional, priceA, stepA)
		qtyB = baseQuantityForNotional(notional, priceB, stepB)
	}
	if execution.ReduceOnly &&
		strings.EqualFold(strings.TrimSpace(execution.PositionEffect), "close") {
		candidate := decimal.Min(qtyA, qtyB)
		candidate = decimal.Min(candidate, arbitrageCloseableBase(combination))
		candidate = decimal.Min(
			candidate,
			arbitrageReduceLegAvailable(combination, "a", aSide),
		)
		candidate = decimal.Min(
			candidate,
			arbitrageReduceLegAvailable(combination, "b", bSide),
		)
		if instrumentA.ContractType == "spot" {
			snapshot, snapshotErr := e.bidSpotSnapshot(
				ctx, execution.ID, combination.LegA, credentialsA,
			)
			if snapshotErr != nil {
				return fmt.Errorf("%w: read spot balance: %v",
					ErrVenueUnavailable, snapshotErr)
			}
			candidate = decimal.Min(candidate, spotAvailableForSide(
				snapshot, combination.LegA.BaseAsset, aSide,
			))
		}
		if instrumentB.ContractType == "spot" {
			snapshot, snapshotErr := e.bidSpotSnapshot(
				ctx, execution.ID, combination.LegB, credentialsB,
			)
			if snapshotErr != nil {
				return fmt.Errorf("%w: read spot balance: %v",
					ErrVenueUnavailable, snapshotErr)
			}
			candidate = decimal.Min(candidate, spotAvailableForSide(
				snapshot, combination.LegB.BaseAsset, bSide,
			))
		}
		commonStep := commonQuantityStep(stepA, stepB)
		qtyA = floorToStep(candidate, commonStep)
		qtyB = qtyA
	}
	if !qtyA.IsPositive() || !qtyB.IsPositive() {
		return ErrRiskLimit
	}
	orderTypeA, orderPriceA := "market", ""
	if instrumentA.ContractType == "spot" {
		orderTypeA = "limit"
		orderPriceA = protectedIOCPrice(
			bboA, aSide, parsePositiveDecimal(instrumentA.PriceTick), e.iocBps,
		)
	}
	orderTypeB, orderPriceB := "market", ""
	if instrumentB.ContractType == "spot" {
		orderTypeB = "limit"
		orderPriceB = protectedIOCPrice(
			bboB, bSide, parsePositiveDecimal(instrumentB.PriceTick), e.iocBps,
		)
	}
	preparedQtyA, preparedPriceA, err := prepareArbitrageOrder(
		instrumentA, orderTypeA, qtyA.String(), orderPriceA, priceA.String(), *execution,
	)
	if err != nil {
		return err
	}
	preparedQtyB, preparedPriceB, err := prepareArbitrageOrder(
		instrumentB, orderTypeB, qtyB.String(), orderPriceB, priceB.String(), *execution,
	)
	if err != nil {
		return err
	}
	qtyA, qtyB = parsePositiveDecimal(preparedQtyA), parsePositiveDecimal(preparedQtyB)
	intents, created, err := e.orders.CreateArbitrageIntents(ctx, []Order{
		e.orderIntent(combination, *execution, combination.LegA, instrumentA, aSide, orderTypeA, qtyA.String(), preparedPriceA, "market", 0),
		e.orderIntent(combination, *execution, combination.LegB, instrumentB, bSide, orderTypeB, qtyB.String(), preparedPriceB, "market", 0),
	})
	if err != nil {
		return err
	}
	watchA := e.watchOrder(ctx, ordersA, adapterA, intents[0])
	watchB := e.watchOrder(ctx, ordersB, adapterB, intents[1])
	type result struct {
		leg   string
		order Order
		err   error
	}
	results := make(chan result, 2)
	submit := func(
		leg string,
		adapter exchange.Adapter,
		credentials Credentials,
		instrument Instrument,
		intent Order,
		created bool,
	) {
		if terminalStatus(intent.Status) || intent.Status == "open" || intent.Status == "partially_filled" {
			results <- result{leg: leg, order: intent}
			return
		}
		lock := e.service.accountLock(credentials.TradingAccountID)
		lock.Lock()
		order := intent
		var submitErr error
		if created {
			if intent.OrderType == "limit" {
				order, submitErr = e.service.submitWithOptions(
					ctx, adapter, credentials, instrument, intent, "IOC", false,
				)
			} else {
				order, submitErr = e.service.submit(ctx, adapter, credentials, instrument, intent)
			}
		} else {
			order, submitErr = e.service.recover(ctx, adapter, credentials, instrument, intent)
		}
		lock.Unlock()
		results <- result{leg: leg, order: order, err: submitErr}
	}
	go submit("a", adapterA, credentialsA, instrumentA, intents[0], created[0])
	go submit("b", adapterB, credentialsB, instrumentB, intents[1], created[1])
	var orderA, orderB Order
	var submitFailure error
	for range 2 {
		result := <-results
		if result.leg == "a" {
			orderA = result.order
		} else {
			orderB = result.order
		}
		if result.err != nil && submitFailure == nil {
			submitFailure = result.err
		}
	}
	var observeErrA, observeErrB error
	orderA, observeErrA = e.observeToTerminal(
		ctx, credentialsA, instrumentA, adapterA, orderA,
		orderObservation{subscription: ordersA, watch: watchA},
	)
	orderB, observeErrB = e.observeToTerminal(
		ctx, credentialsB, instrumentB, adapterB, orderB,
		orderObservation{subscription: ordersB, watch: watchB},
	)
	if observeErrA != nil || observeErrB != nil ||
		!terminalStatus(orderA.Status) || !terminalStatus(orderB.Status) {
		execution.Status = "reconciling"
		_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
		return ErrVenueUncertain
	}
	fillA := parseDecimal(orderA.FilledQuantity)
	fillB := parseDecimal(orderB.FilledQuantity)
	execution.LegAFilledQuantity = fillA.String()
	execution.LegBFilledQuantity = fillB.String()
	mark := priceA.Add(priceB).Div(decimal.NewFromInt(2))
	delta := deltaNotional(fillA, fillB, mark)
	execution.DeltaNotional = delta.String()
	if _, err := e.store.UpdateArbitrageExecution(ctx, *execution); err != nil {
		return err
	}
	updated, err := e.refreshArbitragePositions(ctx, combination, execution)
	if err != nil {
		return err
	}
	if carry := parseDecimal(updated.CarryBaseQuantity); !carry.IsZero() {
		var hedgeLeg ArbitrageLeg
		var hedgeInstrument Instrument
		var hedgeCredentials Credentials
		var hedgeAdapter exchange.Adapter
		var hedgeOrders *orderstream.Subscription
		requiredSide := "sell"
		if carry.IsNegative() {
			requiredSide = "buy"
		}
		if aSide == requiredSide {
			hedgeLeg, hedgeInstrument, hedgeCredentials, hedgeAdapter = combination.LegA, instrumentA, credentialsA, adapterA
			hedgeOrders = ordersA
		} else {
			hedgeLeg, hedgeInstrument, hedgeCredentials, hedgeAdapter = combination.LegB, instrumentB, credentialsB, adapterB
			hedgeOrders = ordersB
		}
		_, hedgeErr := e.hedgeCarry(
			context.Background(), &updated, execution, hedgeLeg, hedgeInstrument,
			hedgeCredentials, hedgeAdapter, hedgeOrders, time.Now(),
		)
		if hedgeErr != nil {
			return hedgeErr
		}
	}
	if err := e.completeExecution(context.Background(), updated, execution, mark); err != nil {
		return err
	}
	if submitFailure != nil {
		message := sanitizeError(submitFailure)
		_ = e.store.AppendArbitrageEvent(
			context.Background(), combination.ID, execution.ID,
			"submit_recovered",
			map[string]any{"message": message},
		)
	}
	return nil
}

func (e *ArbitrageExecutor) observeMaker(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	leg ArbitrageLeg,
	instrument Instrument,
	credentials Credentials,
	adapter exchange.Adapter,
	order Order,
	side, workingPrice, direction string,
	observation orderObservation,
	onFill func(Order) error,
	permit *executionWorkPermit,
) (Order, makerOutcome, error) {
	var updates <-chan orderstream.Update
	if observation.watch != nil {
		defer observation.watch.Close()
		updates = observation.watch.Updates()
	}
	interval := e.observerInterval
	timer := time.NewTimer(interval)
	defer timer.Stop()
	lastREST := time.Now()
	var queryFailureStarted time.Time
	queryFailures := 0
	var streamGeneration uint64
	applyFill := func(current Order) error {
		if onFill == nil || !parseDecimal(current.FilledQuantity).IsPositive() {
			return nil
		}
		return onFill(current)
	}
	if err := applyFill(order); err != nil {
		return order, makerTerminal, err
	}
	cancelObservedMaker := func() (Order, makerOutcome, error) {
		if err := e.acquireMakerWork(ctx, permit, true); err != nil {
			return order, makerTerminal, err
		}
		canceled, err := e.cancelMakerOnce(
			ctx, combination, execution, credentials, instrument, adapter, order,
			observation, applyFill,
		)
		if terminalStatus(canceled.Status) {
			return canceled, makerTerminal, err
		}
		if stayObservingMaker(ctx, err) {
			order = canceled
			return order, makerTerminal, err
		}
		return canceled, makerTerminal, err
	}
	for !terminalStatus(order.Status) {
		if shouldYieldMakerPermit(order) {
			permit.Yield()
		}
		select {
		case <-ctx.Done():
			return cancelObservedMaker()
		case update, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			if err := e.acquireMakerWork(ctx, permit, ctx.Err() != nil); err != nil {
				return order, makerTerminal, err
			}
			refreshed, err := e.applyOrderStreamUpdate(ctx, order, update, instrument)
			if errors.Is(err, errHyperliquidFillUnknown) {
				parked, parkErr := e.parkMakerReconciling(combination, execution, order)
				return parked, makerTerminal, parkErr
			}
			if err != nil {
				continue
			}
			order = refreshed
			if err := applyFill(order); err != nil {
				return order, makerTerminal, err
			}
			if terminalStatus(order.Status) {
				break
			}
			continue
		case <-timer.C:
			if ctx.Err() != nil {
				return cancelObservedMaker()
			}
			if err := e.acquireMakerWork(ctx, permit, false); err != nil {
				return cancelObservedMaker()
			}
		}
		healthy := observation.subscription != nil && observation.subscription.Healthy()
		generation := uint64(0)
		if healthy {
			generation = observation.subscription.Generation()
		}
		if !healthy || generation != streamGeneration || time.Since(lastREST) >= e.streamAudit {
			refreshed, err := e.queryInternal(ctx, credentials, instrument, adapter, order)
			lastREST = time.Now()
			if err == nil {
				order = refreshed
				interval = e.observerInterval
				streamGeneration = generation
				queryFailureStarted = time.Time{}
				queryFailures = 0
				if err := applyFill(order); err != nil {
					return order, makerTerminal, err
				}
			} else {
				queryFailures++
				if queryFailureStarted.IsZero() {
					queryFailureStarted = time.Now()
				}
				interval = min(2*time.Second, interval*2)
				failureWindow := e.timeout
				if failureWindow <= 0 || failureWindow > maxOrderReconcileUncertainAge {
					failureWindow = maxOrderReconcileUncertainAge
				}
				if queryFailures >= maxOrderReconcileFailures ||
					time.Since(queryFailureStarted) >= failureWindow {
					return order, makerTerminal, ErrVenueUncertain
				}
			}
		}
		timer.Reset(interval)
		if terminalStatus(order.Status) {
			break
		}
		latestA, latestAErr := e.latestForLeg(combination.LegA)
		latestB, latestBErr := e.latestForLeg(combination.LegB)
		if latestAErr != nil || latestBErr != nil ||
			!opportunityStillValid(combination, latestA, latestB, direction) {
			canceled, err := e.cancelMakerOnce(
				ctx, combination, execution, credentials, instrument, adapter, order,
				observation, applyFill,
			)
			if terminalStatus(canceled.Status) {
				return canceled, makerOpportunityGone, err
			}
			if stayObservingMaker(ctx, err) {
				order = canceled
				continue
			}
			return canceled, makerOpportunityGone, err
		}
		latest := latestA
		if leg.InstrumentID == combination.LegB.InstrumentID {
			latest = latestB
		}
		top := latest.BidPrice
		if side == "sell" {
			top = latest.AskPrice
		}
		distance := parsePositiveDecimal(workingPrice).Sub(parsePositiveDecimal(top)).Abs()
		tick := parsePositiveDecimal(instrument.PriceTick)
		if makerShouldReprice(distance, tick, e.repriceTicks) {
			e.logger.Info("arbitrage maker reprice",
				"combination_id", combination.ID, "order_id", order.ID,
				"working_price", workingPrice, "top_price", top,
				"distance_ticks", distance.Div(tick).String())
			canceled, err := e.cancelMakerOnce(
				ctx, combination, execution, credentials, instrument, adapter, order,
				observation, applyFill,
			)
			if terminalStatus(canceled.Status) {
				return canceled, makerReprice, err
			}
			if stayObservingMaker(ctx, err) {
				order = canceled
				continue
			}
			return canceled, makerReprice, err
		}
	}
	return order, makerTerminal, nil
}

func shouldYieldMakerPermit(order Order) bool {
	return !terminalStatus(order.Status) && !parseDecimal(order.FilledQuantity).IsPositive()
}

func (e *ArbitrageExecutor) acquireMakerWork(
	ctx context.Context,
	permit *executionWorkPermit,
	drain bool,
) error {
	if permit == nil || permit.Held() {
		return nil
	}
	acquireCtx := ctx
	cancel := func() {}
	if drain {
		acquireCtx, cancel = context.WithTimeout(context.Background(), e.cancelTimeout())
	}
	defer cancel()
	return permit.Acquire(acquireCtx, workPermitUrgent)
}

func (e *ArbitrageExecutor) cancelTimeout() time.Duration {
	if e.timeout > 0 {
		return e.timeout
	}
	return 12 * time.Second
}

const (
	arbitrageCancelWSSGrace  = time.Second
	arbitrageCancelRESTFloor = 2 * time.Second
)

type cancelGeneration struct {
	recorded bool
	value    uint64
}

type makerCancelConfirm struct {
	credentials Credentials
	adapter     exchange.Adapter
	generation  cancelGeneration
	deadline    time.Time
	restDone    *uint32
}

func snapshotCancelGeneration(observation orderObservation) cancelGeneration {
	if observation.subscription == nil || !observation.subscription.Healthy() {
		return cancelGeneration{}
	}
	return cancelGeneration{recorded: true, value: observation.subscription.Generation()}
}

func cancelGenerationChanged(snap cancelGeneration, observation orderObservation) bool {
	if !snap.recorded {
		return true
	}
	if observation.subscription == nil || !observation.subscription.Healthy() {
		return true
	}
	return observation.subscription.Generation() != snap.value
}

func (e *ArbitrageExecutor) persistMakerCanceling(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
) ArbitrageCombination {
	if execution != nil {
		execution.Status = "maker_canceling"
		execution.ErrorMessage = ""
		_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
	}
	combination.RuntimeState = "maker_canceling"
	combination.ErrorMessage = ""
	updated, err := e.store.UpdateArbitrageCombinationRuntime(ctx, combination)
	if err == nil {
		return updated
	}
	return combination
}

func (e *ArbitrageExecutor) restoreMakerOpen(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
) ArbitrageCombination {
	if execution != nil {
		execution.Status = "maker_open"
		execution.ErrorMessage = ""
		_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
	}
	combination.RuntimeState = "maker_open"
	combination.ErrorMessage = ""
	updated, err := e.store.UpdateArbitrageCombinationRuntime(ctx, combination)
	if err == nil {
		return updated
	}
	return combination
}

func (e *ArbitrageExecutor) cancelMakerOnce(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	credentials Credentials,
	instrument Instrument,
	adapter exchange.Adapter,
	order Order,
	observation orderObservation,
	applyFill func(Order) error,
) (Order, error) {
	e.persistMakerCanceling(ctx, combination, execution)
	generation := snapshotCancelGeneration(observation)
	confirmDeadline := time.Now().Add(e.cancelTimeout())
	cancelCtx, cancel := context.WithDeadline(context.Background(), confirmDeadline)
	order, err := e.cancelInternal(cancelCtx, credentials, instrument, adapter, order)
	cancel()
	if terminalStatus(order.Status) {
		if applyFill != nil {
			if fillErr := applyFill(order); err == nil && fillErr != nil {
				err = fillErr
			}
		}
		e.logCancelConfirm(instrument, order.Status, "cancel_response", "")
		return order, err
	}
	var restDone uint32
	return e.finishMakerCancel(
		ctx, combination, execution, instrument, order, err, observation, applyFill,
		makerCancelConfirm{
			credentials: credentials, adapter: adapter,
			generation: generation, deadline: confirmDeadline, restDone: &restDone,
		},
	)
}

func (e *ArbitrageExecutor) finishMakerCancel(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	instrument Instrument,
	order Order,
	cancelErr error,
	observation orderObservation,
	applyFill func(Order) error,
	confirm ...makerCancelConfirm,
) (Order, error) {
	var state makerCancelConfirm
	if len(confirm) > 0 {
		state = confirm[0]
	}
	if terminalStatus(order.Status) {
		return order, cancelErr
	}
	if makerCancelNotSent(cancelErr) && ctx.Err() == nil {
		e.restoreMakerOpen(ctx, combination, execution)
		return order, cancelErr
	}
	if cancelErr == nil ||
		errors.Is(cancelErr, exchange.ErrAmbiguousCancel) ||
		errors.Is(cancelErr, exchange.ErrUncertain) ||
		errors.Is(cancelErr, exchange.ErrOrderNotFound) ||
		errors.Is(cancelErr, context.DeadlineExceeded) {
		return e.awaitCancelTerminal(
			ctx, combination, execution, instrument, order, observation, applyFill, state, cancelErr,
		)
	}
	parked, parkErr := e.parkMakerReconciling(combination, execution, order)
	if parkErr != nil {
		return parked, parkErr
	}
	return parked, cancelErr
}

func makerCancelNotSent(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "invalid hyperliquid order id") {
		return true
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	return false
}

func stayObservingMaker(ctx context.Context, err error) bool {
	return makerCancelNotSent(err) && ctx.Err() == nil
}

func (e *ArbitrageExecutor) awaitCancelTerminal(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	instrument Instrument,
	order Order,
	observation orderObservation,
	applyFill func(Order) error,
	confirm makerCancelConfirm,
	cancelErr error,
) (Order, error) {
	if e.makerCancelNeedsImmediateREST(cancelErr, observation, confirm) {
		return e.confirmMakerCancelByRESTOnce(
			combination, execution, confirm, instrument, order, applyFill, immediateCancelRESTReason(cancelErr, observation, confirm),
		)
	}
	if !confirm.generation.recorded {
		confirm.generation = snapshotCancelGeneration(observation)
	}
	if observation.watch == nil || observation.subscription == nil {
		return e.confirmMakerCancelByRESTOnce(
			combination, execution, confirm, instrument, order, applyFill, "watcher_missing",
		)
	}
	updates := observation.watch.Updates()
	graceUntil := time.Now().Add(arbitrageCancelWSSGrace)
	if !confirm.deadline.IsZero() && confirm.deadline.Before(graceUntil) {
		graceUntil = confirm.deadline
	}
	timer := time.NewTimer(time.Until(graceUntil))
	defer timer.Stop()
	healthEvery := time.Second
	if e.observerInterval > 0 && e.observerInterval < healthEvery {
		healthEvery = e.observerInterval
	}
	health := time.NewTicker(healthEvery)
	defer health.Stop()
	for !terminalStatus(order.Status) {
		select {
		case <-ctx.Done():
			return e.parkMakerReconciling(combination, execution, order)
		case <-timer.C:
			return e.confirmMakerCancelByRESTOnce(
				combination, execution, confirm, instrument, order, applyFill, "wss_grace_elapsed",
			)
		case <-health.C:
			if observation.subscription == nil || !observation.subscription.Healthy() {
				return e.confirmMakerCancelByRESTOnce(
					combination, execution, confirm, instrument, order, applyFill, "wss_unhealthy",
				)
			}
			if confirm.generation.recorded && cancelGenerationChanged(confirm.generation, observation) {
				return e.confirmMakerCancelByRESTOnce(
					combination, execution, confirm, instrument, order, applyFill, "generation_changed",
				)
			}
		case update, ok := <-updates:
			if !ok {
				return e.confirmMakerCancelByRESTOnce(
					combination, execution, confirm, instrument, order, applyFill, "watcher_closed",
				)
			}
			persistCtx, persistCancel := context.WithTimeout(context.Background(), 2*time.Second)
			refreshed, err := e.applyOrderStreamUpdate(persistCtx, order, update, instrument)
			persistCancel()
			if err != nil {
				reason := "wss_persist_failed"
				if errors.Is(err, errHyperliquidFillUnknown) {
					reason = "wss_parse_failed"
				}
				return e.confirmMakerCancelByRESTOnce(
					combination, execution, confirm, instrument, order, applyFill, reason,
				)
			}
			order = refreshed
			e.logCancelConfirm(instrument, order.Status, "wss", "")
			if terminalStatus(order.Status) && applyFill != nil {
				if fillErr := applyFill(order); fillErr != nil {
					return order, fillErr
				}
			}
		}
	}
	return order, nil
}

func (e *ArbitrageExecutor) makerCancelNeedsImmediateREST(
	cancelErr error,
	observation orderObservation,
	confirm makerCancelConfirm,
) bool {
	if observation.watch == nil || observation.subscription == nil {
		return true
	}
	if observation.subscription != nil && !observation.subscription.Healthy() {
		return true
	}
	if confirm.generation.recorded && cancelGenerationChanged(confirm.generation, observation) {
		return true
	}
	if errors.Is(cancelErr, exchange.ErrAmbiguousCancel) ||
		errors.Is(cancelErr, exchange.ErrUncertain) ||
		errors.Is(cancelErr, exchange.ErrOrderNotFound) ||
		errors.Is(cancelErr, context.DeadlineExceeded) {
		return true
	}
	return false
}

func immediateCancelRESTReason(err error, observation orderObservation, confirm makerCancelConfirm) string {
	switch {
	case errors.Is(err, exchange.ErrAmbiguousCancel):
		return "ambiguous_cancel"
	case errors.Is(err, exchange.ErrUncertain):
		return "uncertain_cancel"
	case errors.Is(err, exchange.ErrOrderNotFound):
		return "order_not_found"
	case errors.Is(err, context.DeadlineExceeded):
		return "cancel_timeout"
	case observation.watch == nil || observation.subscription == nil:
		return "watcher_missing"
	case observation.subscription != nil && !observation.subscription.Healthy():
		return "wss_unhealthy"
	case cancelGenerationChanged(confirm.generation, observation):
		return "generation_changed"
	default:
		return "immediate"
	}
}

func (e *ArbitrageExecutor) confirmMakerCancelByRESTOnce(
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	confirm makerCancelConfirm,
	instrument Instrument,
	order Order,
	applyFill func(Order) error,
	reason string,
) (Order, error) {
	if confirm.restDone != nil && !atomic.CompareAndSwapUint32(confirm.restDone, 0, 1) {
		e.logCancelConfirm(instrument, "park", "rest", reason)
		return e.parkMakerReconciling(combination, execution, order)
	}
	if confirm.adapter == nil || e.service == nil {
		e.logCancelConfirm(instrument, "park", "rest", reason)
		return e.parkMakerReconciling(combination, execution, order)
	}
	timeout := time.Until(confirm.deadline)
	if timeout <= 0 {
		timeout = arbitrageCancelRESTFloor
	}
	restCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	refreshed, err := e.queryInternal(restCtx, confirm.credentials, instrument, confirm.adapter, order)
	if err != nil {
		e.logCancelConfirm(instrument, "failed", "rest", reason)
		return e.parkMakerReconciling(combination, execution, order)
	}
	if terminalStatus(refreshed.Status) {
		if applyFill != nil {
			if fillErr := applyFill(refreshed); fillErr != nil {
				return refreshed, fillErr
			}
		}
		e.logCancelConfirm(instrument, refreshed.Status, "rest", reason)
		return refreshed, nil
	}
	e.logCancelConfirm(instrument, refreshed.Status, "rest", reason)
	return e.parkMakerReconciling(combination, execution, refreshed)
}

func (e *ArbitrageExecutor) logCancelConfirm(instrument Instrument, outcome, source, reason string) {
	if e == nil || e.logger == nil {
		return
	}
	e.logger.Info("arbitrage cancel confirm",
		"venue", instrument.Exchange,
		"outcome", outcome,
		"source", source,
		"reason", reason,
	)
}

func (e *ArbitrageExecutor) parkMakerReconciling(
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	order Order,
) (Order, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if execution != nil {
		execution.Status = "reconciling"
		execution.ErrorMessage = ""
		_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
	}
	combination.RuntimeState = "reconciling"
	combination.ErrorMessage = ""
	_, _ = e.store.UpdateArbitrageCombinationRuntime(ctx, combination)
	if order.ID != "" {
		if deferrer, ok := e.orders.(interface {
			DeferReconcile(context.Context, string, time.Time) error
		}); ok {
			_ = deferrer.DeferReconcile(ctx, order.ID, time.Now().Add(5*time.Second))
		}
	}
	return order, ErrArbitrageReconciling
}

func (e *ArbitrageExecutor) refreshArbitragePositions(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
) (ArbitrageCombination, error) {
	updated, err := e.store.RecomputeArbitrageBasePositionsForExecution(ctx, execution.ID)
	if err != nil {
		return combination, err
	}
	if active, activeErr := e.store.GetActiveArbitrageExecution(
		ctx, combination.ID,
	); activeErr == nil && active.ID == execution.ID {
		*execution = active
	}
	return updated, nil
}

func (e *ArbitrageExecutor) prefetchReduceSpotSnapshots(
	ctx context.Context,
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	legs ...arbitrageExecutionLeg,
) error {
	if !execution.ReduceOnly ||
		!strings.EqualFold(strings.TrimSpace(execution.PositionEffect), "close") {
		return nil
	}
	for _, item := range legs {
		if item.instrument.ContractType != "spot" {
			continue
		}
		if _, err := e.bidSpotSnapshot(
			ctx, execution.ID, item.leg, item.credentials,
		); err != nil {
			return fmt.Errorf("%w: read %s spot balance: %v",
				ErrVenueUnavailable, item.leg.ExchangeSymbol, err)
		}
	}
	return nil
}

func (e *ArbitrageExecutor) bidSpotSnapshot(
	ctx context.Context,
	executionID string,
	leg ArbitrageLeg,
	credentials Credentials,
) (portfolio.Snapshot, error) {
	e.mu.Lock()
	if snapshots := e.spotSnapshots[executionID]; snapshots != nil {
		if snapshot, ok := snapshots[credentials.TradingAccountID]; ok {
			e.mu.Unlock()
			return snapshot, nil
		}
	}
	e.mu.Unlock()
	if e.service == nil || e.service.portfolios == nil {
		return portfolio.Snapshot{}, portfolio.ErrUnsupported
	}
	snapshot, err := e.service.portfolios.Snapshot(
		ctx, leg.Exchange,
		portfolio.Credentials{
			APIKey: credentials.APIKey, APISecret: credentials.APISecret,
			Passphrase:   credentials.Passphrase,
			AccountIndex: credentials.AccountIndex,
		},
	)
	if err != nil {
		return portfolio.Snapshot{}, err
	}
	e.mu.Lock()
	if e.spotSnapshots == nil {
		e.spotSnapshots = make(map[string]map[int64]portfolio.Snapshot)
	}
	if e.spotSnapshots[executionID] == nil {
		e.spotSnapshots[executionID] = make(map[int64]portfolio.Snapshot)
	}
	e.spotSnapshots[executionID][credentials.TradingAccountID] = snapshot
	e.mu.Unlock()
	return snapshot, nil
}

func spotAvailableForSide(
	snapshot portfolio.Snapshot,
	baseAsset, side string,
) decimal.Decimal {
	balance := decimal.Zero
	for asset, raw := range snapshot.SpotBalances {
		if !strings.EqualFold(asset, baseAsset) {
			continue
		}
		balance = parseDecimal(raw)
		break
	}
	if strings.EqualFold(side, "sell") && balance.IsPositive() {
		return balance
	}
	if strings.EqualFold(side, "buy") && balance.IsNegative() {
		return balance.Abs()
	}
	return decimal.Zero
}

func arbitrageLegName(
	combination ArbitrageCombination,
	leg ArbitrageLeg,
) string {
	if leg.TradingAccountID == combination.LegA.TradingAccountID &&
		leg.InstrumentID == combination.LegA.InstrumentID {
		return "a"
	}
	if leg.TradingAccountID == combination.LegB.TradingAccountID &&
		leg.InstrumentID == combination.LegB.InstrumentID {
		return "b"
	}
	return ""
}

const lastCloseCarryDeferredEvent = "last_close_carry_deferred_to_leg_flatten"

func lastCloseStandaloneCarryReady(
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	hedgeLeg ArbitrageLeg,
	instrument Instrument,
	bbo marketdata.BBO,
) (bool, map[string]any) {
	carry := parseDecimal(combination.CarryBaseQuantity)
	side := carryHedgeSide(carry)
	legName := arbitrageLegName(combination, hedgeLeg)
	available := arbitrageReduceLegAvailable(combination, legName, side)
	step := closeFlattenQuantityStep(instrument)
	payload := map[string]any{
		"carryBaseQuantity": carry.String(),
		"candidateLeg":      legName,
		"side":              side,
		"available":         available.String(),
		"step":              step.String(),
	}
	if !available.IsPositive() {
		payload["reason"] = "not_reducible"
		return false, payload
	}
	if !step.IsPositive() {
		payload["reason"] = "step_unavailable"
		return false, payload
	}
	if !floorToStep(carry.Abs(), step).IsPositive() {
		payload["reason"] = "below_quantity_step"
		return false, payload
	}
	price := parsePositiveDecimal(bbo.AskPrice)
	if side == "sell" {
		price = parsePositiveDecimal(bbo.BidPrice)
	}
	_, eligible, err := executableHedgeQuantity(
		decimal.Min(carry.Abs(), available),
		price,
		instrument,
		allowBelowMinNotionalCombo(combination, execution, instrument),
	)
	if err != nil || !eligible {
		payload["reason"] = "not_executable"
		if err != nil {
			payload["error"] = err.Error()
		}
		return false, payload
	}
	return true, payload
}

func arbitrageReduceLegAvailable(
	combination ArbitrageCombination,
	legName, side string,
) decimal.Decimal {
	baseline := decimal.Zero
	combo := decimal.Zero
	switch legName {
	case "a":
		if !combination.VenueBaselineCapturedAt.IsZero() &&
			!combination.VenueBaselineCapturedAt.Equal(time.Unix(0, 0).UTC()) {
			baseline = parseDecimal(combination.LegAVenueBaselineBasePosition)
		}
		combo = parseDecimal(combination.LegABasePosition)
	case "b":
		if !combination.VenueBaselineCapturedAt.IsZero() &&
			!combination.VenueBaselineCapturedAt.Equal(time.Unix(0, 0).UTC()) {
			baseline = parseDecimal(combination.LegBVenueBaselineBasePosition)
		}
		combo = parseDecimal(combination.LegBBasePosition)
	default:
		return decimal.Zero
	}
	position := baseline.Add(combo)
	if position.IsPositive() && strings.EqualFold(side, "sell") {
		return position
	}
	if position.IsNegative() && strings.EqualFold(side, "buy") {
		return position.Abs()
	}
	return decimal.Zero
}

func (e *ArbitrageExecutor) carryHedgeExecutableAtBBO(
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	instrument Instrument,
	bbo marketdata.BBO,
) (bool, error) {
	carry := parseDecimal(combination.CarryBaseQuantity)
	if carry.IsZero() {
		return false, nil
	}
	side := carryHedgeSide(carry)
	price := parsePositiveDecimal(bbo.AskPrice)
	if side == "sell" {
		price = parsePositiveDecimal(bbo.BidPrice)
	}
	_, eligible, err := executableHedgeQuantity(
		carry.Abs(), price, instrument,
		allowBelowMinNotionalCombo(combination, execution, instrument),
	)
	return eligible, err
}

const (
	dustReasonStepRoundsToZero = "step_rounds_to_zero"
	dustReasonBelowMinQuantity = "below_min_quantity"
	dustReasonBelowMinNotional = "below_min_notional"
)

func hedgeQuantityDustReason(
	target, price decimal.Decimal,
	instrument Instrument,
) string {
	quantity := floorToStep(target.Abs(), parsePositiveDecimal(instrument.QuantityStep))
	if !quantity.IsPositive() {
		return dustReasonStepRoundsToZero
	}
	switch instrument.MinQuantityStatus {
	case exchange.ConstraintKnown:
		minimum := parsePositiveDecimal(instrument.MinQuantity)
		if !minimum.IsPositive() {
			return ""
		}
		if quantity.LessThan(minimum) {
			return dustReasonBelowMinQuantity
		}
	case exchange.ConstraintNotApplicable:
	default:
		return ""
	}
	switch instrument.MinNotionalStatus {
	case exchange.ConstraintKnown:
		minimum := parsePositiveDecimal(instrument.MinNotional)
		if !minimum.IsPositive() || !price.IsPositive() {
			return ""
		}
		if quantity.Mul(price).LessThan(minimum) {
			return dustReasonBelowMinNotional
		}
	case exchange.ConstraintNotApplicable:
	default:
		return ""
	}
	return ""
}

func hedgeDeferredDustPayload(
	side string,
	residual, price decimal.Decimal,
	instrument Instrument,
	extra map[string]any,
) map[string]any {
	payload := map[string]any{
		"reason":           hedgeQuantityDustReason(residual, price, instrument),
		"residualQuantity": residual.Abs().String(),
		"residualNotional": residual.Abs().Mul(price).String(),
		"minNotional":      instrument.MinNotional,
		"minQuantity":      instrument.MinQuantity,
		"quantityStep":     instrument.QuantityStep,
		"hedgeSide":        side,
	}
	for key, value := range extra {
		payload[key] = value
	}
	return payload
}

func persistHedgeDeferredDust(
	e *ArbitrageExecutor,
	ctx context.Context,
	combination ArbitrageCombination,
) ArbitrageCombination {
	combination.RuntimeState = "hedge_deferred_dust"
	combination.ErrorMessage = ""
	updated, err := e.store.UpdateArbitrageCombinationRuntime(ctx, combination)
	if err != nil {
		return combination
	}
	return updated
}

func (e *ArbitrageExecutor) hedgeCarry(
	ctx context.Context,
	combination *ArbitrageCombination,
	execution *ArbitrageExecution,
	leg ArbitrageLeg,
	instrument Instrument,
	credentials Credentials,
	adapter exchange.Adapter,
	subscription *orderstream.Subscription,
	dispatchStart time.Time,
) (bool, error) {
	return e.hedgeCarryWithTiming(
		ctx, combination, execution, leg, instrument, credentials, adapter,
		subscription, hedgeTiming{dispatchStartedAt: dispatchStart},
	)
}

func (e *ArbitrageExecutor) hedgeMakerCarryWithDeadline(
	ctx context.Context,
	combination *ArbitrageCombination,
	execution *ArbitrageExecution,
	leg ArbitrageLeg,
	instrument Instrument,
	credentials Credentials,
	adapter exchange.Adapter,
	subscription *orderstream.Subscription,
	side string,
	timing hedgeTiming,
) (bool, error) {
	makerFilled, hedgeFilled, remaining := makerHedgeRemaining(*combination, *execution)
	if strings.TrimSpace(execution.MakerOrderID) == "" {
		_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
		return e.hedgeCarryWithTiming(
			ctx, combination, execution, leg, instrument, credentials, adapter,
			subscription, timing,
		)
	}
	if !remaining.IsPositive() {
		_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
		return false, nil
	}
	if strings.TrimSpace(execution.HedgeOrderID) != "" {
		_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
		return e.hedgeCarryWithTiming(
			ctx, combination, execution, leg, instrument, credentials, adapter,
			subscription, timing,
		)
	}
	latest, err := e.latestForLeg(leg)
	if err != nil {
		_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
		return false, ErrMarketDataStale
	}
	price := parsePositiveDecimal(latest.AskPrice)
	if side == "sell" {
		price = parsePositiveDecimal(latest.BidPrice)
	}
	skipMinNotional := allowBelowMinNotionalCombo(*combination, *execution, instrument)
	quantity, eligible, err := executableHedgeQuantity(remaining, price, instrument, skipMinNotional)
	if err != nil {
		_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
		return false, err
	}
	if eligible && execution.ReduceOnly &&
		strings.EqualFold(strings.TrimSpace(execution.PositionEffect), "close") &&
		instrument.ContractType == "spot" {
		snapshot, snapshotErr := e.bidSpotSnapshot(ctx, execution.ID, leg, credentials)
		if snapshotErr != nil {
			_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
			return false, fmt.Errorf("%w: read spot balance: %v", ErrVenueUnavailable, snapshotErr)
		}
		available := spotAvailableForSide(snapshot, leg.BaseAsset, side)
		quantity, eligible, err = executableHedgeQuantity(
			decimal.Min(remaining, available), price, instrument, skipMinNotional,
		)
		if err != nil {
			_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
			return false, err
		}
		if !eligible {
			_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
			return false, ErrArbitrageUnsafeHedge
		}
	}
	if !eligible {
		_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
		if skipMinNotional {
			return false, ErrArbitrageUnsafeHedge
		}
		*combination = persistHedgeDeferredDust(e, ctx, *combination)
		_ = e.store.AppendArbitrageEvent(
			ctx, combination.ID, execution.ID, "hedge_deferred_dust",
			hedgeDeferredDustPayload(side, remaining, price, instrument, map[string]any{
				"confirmedMakerQuantity": makerFilled.String(),
				"confirmedHedgeQuantity": hedgeFilled.String(),
				"quantity":               quantity.String(),
			}),
		)
		return false, nil
	}
	started := time.Now()
	if !timing.dispatchStartedAt.IsZero() {
		started = timing.dispatchStartedAt
	}
	filled, hedgeErr := e.hedgeIOCTimed(
		ctx, *combination, execution, leg, instrument, credentials, adapter,
		side, quantity, timing, hedgeIOCAdmission{
			fastPath: true, makerFilled: makerFilled.String(), hedgeFilled: hedgeFilled.String(),
		}, subscription,
	)
	e.lastHedgeLatency.Store(uint64(time.Since(started).Milliseconds()))
	if hedgeAdmissionConflict(hedgeErr) || errors.Is(hedgeErr, errLastClipDustHandoff) {
		_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
		if !terminalArbitrageExecutionStatus(execution.Status) && hedgeAdmissionConflict(hedgeErr) {
			_, _ = e.parkMakerReconciling(*combination, execution, Order{})
		}
		return false, hedgeErr
	}
	return e.finishHedgeCarry(
		ctx, combination, execution, leg, instrument, filled, hedgeErr, true,
	)
}

func makerHedgeRemaining(
	combination ArbitrageCombination, execution ArbitrageExecution,
) (makerFilled, hedgeFilled, remaining decimal.Decimal) {
	if strings.EqualFold(strings.TrimSpace(combination.MakerLeg), "b") {
		makerFilled = parseDecimal(execution.LegBFilledQuantity)
		hedgeFilled = parseDecimal(execution.LegAFilledQuantity)
	} else {
		makerFilled = parseDecimal(execution.LegAFilledQuantity)
		hedgeFilled = parseDecimal(execution.LegBFilledQuantity)
	}
	remaining = makerFilled.Sub(hedgeFilled)
	if remaining.IsNegative() {
		remaining = decimal.Zero
	}
	return makerFilled, hedgeFilled, remaining
}

func hedgeAdmissionConflict(err error) bool {
	return errors.Is(err, ErrArbitrageHedgeAdmissionConflict) ||
		errors.Is(err, ErrArbitrageExecutionTerminal) ||
		errors.Is(err, ErrArbitrageHedgeSequence) ||
		errors.Is(err, ErrIdempotencyConflict) ||
		errors.Is(err, ErrNotFound)
}

func (e *ArbitrageExecutor) hedgeCarryWithTiming(
	ctx context.Context,
	combination *ArbitrageCombination,
	execution *ArbitrageExecution,
	leg ArbitrageLeg,
	instrument Instrument,
	credentials Credentials,
	adapter exchange.Adapter,
	subscription *orderstream.Subscription,
	timing hedgeTiming,
) (bool, error) {
	if combination == nil {
		return false, ErrInvalidArgument
	}
	if execution.HedgeOrderID != "" {
		existing, getErr := e.orders.GetByOwner(
			ctx, combination.OwnerUsername, execution.HedgeOrderID,
		)
		if getErr != nil {
			return false, getErr
		}
		if !terminalStatus(existing.Status) {
			existing, getErr = e.observeToTerminal(
				ctx, credentials, instrument, adapter, existing,
				orderObservation{subscription: subscription},
			)
			if getErr != nil || !terminalStatus(existing.Status) {
				execution.Status = "reconciling"
				_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
				return false, ErrVenueUncertain
			}
		}
	}
	updated, err := e.refreshArbitragePositions(ctx, *combination, execution)
	if err != nil {
		return false, err
	}
	*combination = updated
	carry := parseDecimal(combination.CarryBaseQuantity)
	if carry.IsZero() {
		return false, nil
	}
	side := carryHedgeSide(carry)
	target := carry.Abs()
	latest, err := e.latestForLeg(leg)
	if err != nil {
		return false, ErrMarketDataStale
	}
	price := parsePositiveDecimal(latest.AskPrice)
	if side == "sell" {
		price = parsePositiveDecimal(latest.BidPrice)
	}
	skipMinNotional := allowBelowMinNotionalCombo(*combination, *execution, instrument)
	quantity, eligible, err := executableHedgeQuantity(
		target, price, instrument, skipMinNotional,
	)
	if err != nil {
		return false, err
	}
	if eligible && execution.ReduceOnly &&
		strings.EqualFold(strings.TrimSpace(execution.PositionEffect), "close") {
		legName := arbitrageLegName(*combination, leg)
		available := arbitrageReduceLegAvailable(*combination, legName, side)
		if instrument.ContractType == "spot" {
			snapshot, snapshotErr := e.bidSpotSnapshot(
				ctx, execution.ID, leg, credentials,
			)
			if snapshotErr != nil {
				return false, fmt.Errorf("%w: read spot balance: %v",
					ErrVenueUnavailable, snapshotErr)
			}
			available = decimal.Min(
				available,
				spotAvailableForSide(snapshot, leg.BaseAsset, side),
			)
		}
		target = decimal.Min(target, available)
		if !target.IsPositive() {
			return false, ErrArbitrageUnsafeHedge
		}
		quantity, eligible, err = executableHedgeQuantity(
			target, price, instrument, skipMinNotional,
		)
		if err != nil {
			return false, err
		}
		if !eligible {
			return false, ErrArbitrageUnsafeHedge
		}
	}
	if !eligible {
		if skipMinNotional {
			return false, ErrArbitrageUnsafeHedge
		}
		*combination = persistHedgeDeferredDust(e, ctx, *combination)
		_ = e.store.AppendArbitrageEvent(
			ctx, combination.ID, execution.ID, "hedge_deferred_dust",
			hedgeDeferredDustPayload(side, carry, price, instrument, map[string]any{
				"carryBaseQuantity": carry.String(),
				"quantity":          quantity.String(),
			}),
		)
		return false, nil
	}

	if timing.dispatchStartedAt.IsZero() {
		timing.dispatchStartedAt = time.Now()
	}
	started := time.Now()
	filled, hedgeErr := e.hedgeIOCTimed(
		ctx, *combination, execution, leg, instrument, credentials, adapter,
		side, quantity, timing, hedgeIOCAdmission{
			aggregateCarry: true,
			expectedCarry:  combination.CarryBaseQuantity,
		}, subscription,
	)
	e.lastHedgeLatency.Store(uint64(time.Since(started).Milliseconds()))
	if errors.Is(hedgeErr, errLastClipDustHandoff) {
		return false, hedgeErr
	}
	return e.finishHedgeCarry(
		ctx, combination, execution, leg, instrument, filled, hedgeErr, false,
	)
}

func (e *ArbitrageExecutor) finishHedgeCarry(
	ctx context.Context,
	combination *ArbitrageCombination,
	execution *ArbitrageExecution,
	leg ArbitrageLeg,
	instrument Instrument,
	filled decimal.Decimal,
	hedgeErr error,
	useExecutionResidual bool,
) (bool, error) {
	if combination == nil {
		return false, ErrInvalidArgument
	}
	if leg.InstrumentID == combination.LegA.InstrumentID {
		execution.LegAFilledQuantity = parseDecimal(
			execution.LegAFilledQuantity,
		).Add(filled).String()
	} else {
		execution.LegBFilledQuantity = parseDecimal(
			execution.LegBFilledQuantity,
		).Add(filled).String()
	}
	_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
	updated, refreshErr := e.refreshArbitragePositions(ctx, *combination, execution)
	if refreshErr != nil {
		return true, refreshErr
	}
	*combination = updated
	if hedgeErr != nil {
		return true, hedgeErr
	}
	residual := parseDecimal(updated.CarryBaseQuantity)
	if useExecutionResidual {
		_, _, remaining := makerHedgeRemaining(updated, *execution)
		residual = remaining
	}
	skipMinNotional := allowBelowMinNotionalCombo(updated, *execution, instrument)
	if !residual.IsZero() {
		latest, err := e.latestForLeg(leg)
		if err != nil {
			return true, ErrMarketDataStale
		}
		price := parsePositiveDecimal(latest.AskPrice)
		if residual.IsPositive() {
			price = parsePositiveDecimal(latest.BidPrice)
		}
		if useExecutionResidual && !hedgeHedgeIsSell(updated, *execution, leg) {
			price = parsePositiveDecimal(latest.AskPrice)
		}
		target := residual.Abs()
		_, stillExecutable, eligibilityErr := executableHedgeQuantity(
			target, price, instrument, skipMinNotional,
		)
		if eligibilityErr != nil {
			return true, eligibilityErr
		}
		if stillExecutable {
			execution.Status = "reconciling"
			_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
			return true, ErrRiskLimit
		}
		if skipMinNotional {
			return true, ErrArbitrageUnsafeHedge
		}
		_ = e.store.AppendArbitrageEvent(
			ctx, combination.ID, execution.ID, "hedge_completed",
			map[string]any{
				"filledBaseQuantity": filled.String(),
				"carryBaseQuantity":  updated.CarryBaseQuantity,
			},
		)
		*combination = persistHedgeDeferredDust(e, ctx, updated)
		_ = e.store.AppendArbitrageEvent(
			ctx, combination.ID, execution.ID, "hedge_deferred_dust",
			hedgeDeferredDustPayload(carryHedgeSide(residual), residual, price, instrument, map[string]any{
				"carryBaseQuantity": updated.CarryBaseQuantity,
			}),
		)
		return true, nil
	}
	_ = e.store.AppendArbitrageEvent(
		ctx, combination.ID, execution.ID, "hedge_completed",
		map[string]any{
			"filledBaseQuantity": filled.String(),
			"carryBaseQuantity":  updated.CarryBaseQuantity,
		},
	)
	updated.RuntimeState = "monitoring"
	updated.ErrorMessage = ""
	stored, _ := e.store.UpdateArbitrageCombinationRuntime(ctx, updated)
	*combination = stored
	return true, nil
}

func hedgeHedgeIsSell(
	combination ArbitrageCombination, execution ArbitrageExecution, leg ArbitrageLeg,
) bool {
	aSide, bSide, ok := arbitrageLegSides(execution.Direction)
	if !ok {
		return true
	}
	if leg.InstrumentID == combination.LegA.InstrumentID {
		return aSide == "sell"
	}
	return bSide == "sell"
}

func executableHedgeQuantity(
	target, price decimal.Decimal,
	instrument Instrument,
	skipMinNotional bool,
) (decimal.Decimal, bool, error) {
	quantity := floorToStep(target, parsePositiveDecimal(instrument.QuantityStep))
	if !quantity.IsPositive() {
		return decimal.Zero, false, nil
	}
	switch instrument.MinQuantityStatus {
	case exchange.ConstraintKnown:
		minimum := parsePositiveDecimal(instrument.MinQuantity)
		if !minimum.IsPositive() {
			return quantity, false, ErrInstrumentUnavailable
		}
		if quantity.LessThan(minimum) {
			return quantity, false, nil
		}
	case exchange.ConstraintNotApplicable:
	case exchange.ConstraintUnknown, "":
		return quantity, false, ErrInstrumentUnavailable
	default:
		return quantity, false, ErrInstrumentUnavailable
	}
	if skipMinNotional {
		return quantity, true, nil
	}
	switch instrument.MinNotionalStatus {
	case exchange.ConstraintKnown:
		minimum := parsePositiveDecimal(instrument.MinNotional)
		if !minimum.IsPositive() || !price.IsPositive() {
			return quantity, false, ErrInstrumentUnavailable
		}
		if quantity.Mul(price).LessThan(minimum) {
			return quantity, false, nil
		}
	case exchange.ConstraintNotApplicable:
	case exchange.ConstraintUnknown, "":
		return quantity, false, ErrInstrumentUnavailable
	default:
		return quantity, false, ErrInstrumentUnavailable
	}
	return quantity, true, nil
}

func (e *ArbitrageExecutor) hedgeIOC(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	leg ArbitrageLeg,
	instrument Instrument,
	credentials Credentials,
	adapter exchange.Adapter,
	side string,
	target decimal.Decimal,
	dispatchStart time.Time,
	subscriptions ...*orderstream.Subscription,
) (decimal.Decimal, error) {
	return e.hedgeIOCTimed(
		ctx, combination, execution, leg, instrument, credentials, adapter,
		side, target, hedgeTiming{dispatchStartedAt: dispatchStart}, hedgeIOCAdmission{},
		subscriptions...,
	)
}

func (e *ArbitrageExecutor) hedgeIOCTimed(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	leg ArbitrageLeg,
	instrument Instrument,
	credentials Credentials,
	adapter exchange.Adapter,
	side string,
	target decimal.Decimal,
	timing hedgeTiming,
	admission hedgeIOCAdmission,
	subscriptions ...*orderstream.Subscription,
) (decimal.Decimal, error) {
	var subscription *orderstream.Subscription
	if len(subscriptions) > 0 {
		subscription = subscriptions[0]
	}
	filled := decimal.Zero
	if handoffErr := e.maybeHandoffLastClipHedgeMinNotional(
		ctx, combination, execution, errors.New(execution.ErrorMessage),
	); handoffErr != nil {
		return filled, handoffErr
	}
	hedgeStarted := timing.dispatchStartedAt
	if hedgeStarted.IsZero() {
		hedgeStarted = time.Now()
	}
	expectedSequence := execution.HedgeSequence
	if reusedSeq, _, ok := e.reusableHedgeAttempt(ctx, combination, *execution); ok {
		expectedSequence = reusedSeq
	}
	lastOrder := Order{}
	lastTerminal := false
	for retry := 0; retry < e.iocRetries && filled.LessThan(target); retry++ {
		if retry > 0 && !timing.emergencyAt.IsZero() && !time.Now().Before(timing.emergencyAt) &&
			lastTerminal {
			break
		}
		latest, err := e.latestForLeg(leg)
		if err != nil {
			return filled, ErrMarketDataStale
		}
		remaining := floorToStep(target.Sub(filled), parsePositiveDecimal(instrument.QuantityStep))
		if !remaining.IsPositive() {
			break
		}
		price := protectedIOCPrice(latest, side, parsePositiveDecimal(instrument.PriceTick), e.iocBps)
		price, err = normalizeHedgeIOCPrice(instrument, side, price)
		if err != nil {
			return filled, err
		}
		preparedQuantity, preparedPrice, prepareErr := prepareArbitrageOrder(
			instrument, "limit", remaining.String(), price, price, *execution,
		)
		if prepareErr != nil {
			return filled, prepareErr
		}
		remaining = parsePositiveDecimal(preparedQuantity)
		price = preparedPrice
		attempt := int(expectedSequence)
		role := "hedge"
		if admission.aggregateCarry {
			role = "residual"
		}
		intent := e.orderIntent(
			combination, *execution, leg, instrument, side, "limit",
			remaining.String(), price, role, attempt,
		)
		prepareIn := PrepareArbitrageHedgeIntentInput{
			Combination:      combination,
			Execution:        *execution,
			Order:            intent,
			ExpectedSequence: expectedSequence,
			HedgeLeg:         leg,
			HedgeSide:        side,
			TargetQuantity:   remaining.String(),
			CarryQuantity:    combination.CarryBaseQuantity,
		}
		if admission.fastPath && retry == 0 {
			prepareIn.FastPathAdmission = true
			prepareIn.ConfirmedMakerFilled = admission.makerFilled
			prepareIn.ConfirmedHedgeFilled = admission.hedgeFilled
		}
		if admission.aggregateCarry && retry == 0 {
			prepareIn.AggregateCarry = true
			prepareIn.ExpectedCarryQuantity = admission.expectedCarry
		}
		intentStarted := time.Now()
		prepared, err := e.store.PrepareArbitrageHedgeIntent(ctx, prepareIn)
		if e.logger != nil {
			e.logger.Info("arbitrage hedge intent",
				"combination_id", combination.ID,
				"execution_id", execution.ID,
				"hedge_intent_transaction_ms", time.Since(intentStarted).Milliseconds(),
			)
		}
		if err != nil {
			return filled, err
		}
		combination = prepared.Combination
		*execution = prepared.Execution
		stored := prepared.Order
		created := prepared.Created
		order := stored
		watch := e.watchOrder(ctx, subscription, adapter, stored)
		if created {
			if e.logger != nil {
				e.logger.Info("arbitrage hedge dispatch",
					"combination_id", combination.ID,
					"execution_id", execution.ID,
					"hedge_order_id", stored.ID,
					"hedge_sequence", expectedSequence,
					"hedge_dispatch_ms", time.Since(hedgeStarted).Milliseconds(),
					"maker_fill_to_hedge_submit_ms", time.Since(hedgeStarted).Milliseconds(),
				)
			}
			if e.service == nil {
				return filled, ErrInvalidArgument
			}
			lock := e.service.accountLock(credentials.TradingAccountID)
			waitStart := time.Now()
			lock.Lock()
			if e.logger != nil {
				e.logger.Info("arbitrage hedge account lock",
					"combination_id", combination.ID,
					"execution_id", execution.ID,
					"hedge_account_lock_wait_ms", time.Since(waitStart).Milliseconds(),
				)
			}
			order, err = e.service.submitPrepared(
				ctx, adapter, credentials, instrument, stored, "IOC", false,
			)
			lock.Unlock()
			if err != nil {
				if errors.Is(err, ErrVenueUncertain) {
					execution.Status = "reconciling"
					_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
					return filled, err
				}
				if handoffErr := e.maybeHandoffLastClipHedgeMinNotional(
					ctx, combination, execution, err,
				); handoffErr != nil {
					return filled, handoffErr
				}
				return filled, err
			}
		} else if stored.Status == "pending" {
			if e.service == nil {
				return filled, ErrInvalidArgument
			}
			order, err = e.service.recover(
				ctx, adapter, credentials, instrument, stored,
			)
			if err != nil {
				if handoffErr := e.maybeHandoffLastClipHedgeMinNotional(
					ctx, combination, execution, err,
				); handoffErr != nil {
					return filled, handoffErr
				}
				return filled, err
			}
		}
		order, observeErr := e.observeToTerminal(
			ctx, credentials, instrument, adapter, order,
			orderObservation{subscription: subscription, watch: watch},
		)
		if observeErr != nil || !terminalStatus(order.Status) {
			execution.Status = "reconciling"
			_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
			return filled, ErrVenueUncertain
		}
		lastOrder = order
		lastTerminal = true
		if strings.EqualFold(order.Status, "rejected") {
			message := strings.TrimSpace(strings.Join(
				[]string{order.ErrorCode, order.ErrorMessage},
				" ",
			))
			rejectErr := fmt.Errorf("%w: %s", ErrVenueRejected, message)
			if handoffErr := e.maybeHandoffLastClipHedgeMinNotional(
				ctx, combination, execution, rejectErr,
			); handoffErr != nil {
				return filled, handoffErr
			}
		}
		filled = filled.Add(parseDecimal(order.FilledQuantity))
		_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
		_ = e.store.AppendArbitrageEvent(ctx, combination.ID, execution.ID, "hedge_attempt", map[string]any{
			"leg": intent.ArbitrageLeg, "attempt": attempt, "quantity": remaining.String(),
			"filledQuantity": order.FilledQuantity, "status": order.Status,
		})
		if terminalStatus(order.Status) && filled.LessThan(target) {
			expectedSequence = execution.HedgeSequence
		}
		if filled.LessThan(target) && !timing.emergencyAt.IsZero() &&
			!time.Now().Before(timing.emergencyAt) {
			break
		}
	}
	residual := target.Sub(filled)
	if residual.IsPositive() && !timing.emergencyAt.IsZero() && lastTerminal {
		trigger := "retries_exhausted"
		if !time.Now().Before(timing.emergencyAt) {
			trigger = "deadline"
		}
		emergencyFilled, emergencyErr := e.submitEmergencyHedge(
			ctx, combination, execution, leg, instrument, credentials, adapter,
			subscription, side, residual, lastOrder, hedgeStarted, trigger,
		)
		filled = filled.Add(emergencyFilled)
		if emergencyErr != nil {
			residual = target.Sub(filled).Abs()
			e.logger.Info("arbitrage hedge complete",
				"combination_id", combination.ID, "execution_id", execution.ID,
				"target_base_quantity", target.String(), "filled_base_quantity", filled.String(),
				"residual_base_quantity", residual.String(),
				"hedge_latency_ms", time.Since(hedgeStarted).Milliseconds())
			return filled, emergencyErr
		}
	}
	residual = target.Sub(filled).Abs()
	e.logger.Info("arbitrage hedge complete",
		"combination_id", combination.ID, "execution_id", execution.ID,
		"target_base_quantity", target.String(), "filled_base_quantity", filled.String(),
		"residual_base_quantity", residual.String(),
		"hedge_latency_ms", time.Since(hedgeStarted).Milliseconds())
	return filled, nil
}

func (e *ArbitrageExecutor) submitEmergencyHedge(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	leg ArbitrageLeg,
	instrument Instrument,
	credentials Credentials,
	adapter exchange.Adapter,
	subscription *orderstream.Subscription,
	side string,
	remaining decimal.Decimal,
	previous Order,
	hedgeStarted time.Time,
	trigger string,
) (decimal.Decimal, error) {
	latest, err := e.latestForLeg(leg)
	if err != nil {
		return decimal.Zero, ErrMarketDataStale
	}
	price := parsePositiveDecimal(latest.AskPrice)
	if side == "sell" {
		price = parsePositiveDecimal(latest.BidPrice)
	}
	skipMinNotional := allowBelowMinNotionalCombo(combination, *execution, instrument)
	quantity, eligible, err := executableHedgeQuantity(remaining, price, instrument, skipMinNotional)
	if err != nil {
		return decimal.Zero, err
	}
	if !eligible {
		if skipMinNotional {
			return decimal.Zero, ErrArbitrageUnsafeHedge
		}
		return decimal.Zero, nil
	}
	reference := price.String()
	preparedQuantity, _, prepareErr := prepareArbitrageOrder(
		instrument, "market", quantity.String(), "", reference, *execution,
	)
	if prepareErr != nil {
		return decimal.Zero, prepareErr
	}
	quantity = parsePositiveDecimal(preparedQuantity)
	if quantity.GreaterThan(remaining) {
		quantity = remaining
	}
	expectedSequence := execution.HedgeSequence
	intent := e.orderIntent(
		combination, *execution, leg, instrument, side, "market",
		quantity.String(), "", "hedge", int(expectedSequence),
	)
	makerFilled, hedgeFilled, _ := makerHedgeRemaining(combination, *execution)
	_ = e.store.AppendArbitrageEvent(ctx, combination.ID, execution.ID, "hedge_emergency_triggered", map[string]any{
		"combination_id":           combination.ID,
		"execution_id":             execution.ID,
		"venue":                    instrument.Exchange,
		"side":                     side,
		"confirmed_maker_quantity": makerFilled.String(),
		"confirmed_hedge_quantity": hedgeFilled.String(),
		"residual_quantity":        remaining.String(),
		"elapsed_ms":               time.Since(hedgeStarted).Milliseconds(),
		"deadline_ms":              e.hedgeEmergencyAfter.Milliseconds(),
		"trigger_reason":           trigger,
		"previous_order_status":    previous.Status,
	})
	prepared, err := e.store.PrepareArbitrageHedgeIntent(ctx, PrepareArbitrageHedgeIntentInput{
		Combination:      combination,
		Execution:        *execution,
		Order:            intent,
		ExpectedSequence: expectedSequence,
		HedgeLeg:         leg,
		HedgeSide:        side,
		TargetQuantity:   quantity.String(),
		CarryQuantity:    combination.CarryBaseQuantity,
	})
	if err != nil {
		_ = e.store.AppendArbitrageEvent(ctx, combination.ID, execution.ID, "hedge_emergency_failed", map[string]any{
			"error": err.Error(), "trigger_reason": trigger,
		})
		return decimal.Zero, err
	}
	combination = prepared.Combination
	*execution = prepared.Execution
	stored := prepared.Order
	order := stored
	watch := e.watchOrder(ctx, subscription, adapter, stored)
	if prepared.Created {
		if e.service == nil {
			return decimal.Zero, ErrInvalidArgument
		}
		lock := e.service.accountLock(credentials.TradingAccountID)
		lock.Lock()
		order, err = e.service.submitPrepared(
			ctx, adapter, credentials, instrument, stored, "", false,
		)
		lock.Unlock()
		if err != nil {
			if errors.Is(err, ErrVenueUncertain) {
				execution.Status = "reconciling"
				_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
				_ = e.store.AppendArbitrageEvent(
					ctx, combination.ID, execution.ID, "hedge_emergency_skipped_uncertain",
					map[string]any{"emergency_order_id": stored.ID, "trigger_reason": trigger},
				)
			} else {
				_ = e.store.AppendArbitrageEvent(
					ctx, combination.ID, execution.ID, "hedge_emergency_failed",
					map[string]any{"error": err.Error(), "trigger_reason": trigger},
				)
			}
			return parseDecimal(order.FilledQuantity), err
		}
	} else if stored.Status == "pending" {
		if e.service == nil {
			return decimal.Zero, ErrInvalidArgument
		}
		order, err = e.service.recover(ctx, adapter, credentials, instrument, stored)
		if err != nil {
			return parseDecimal(order.FilledQuantity), err
		}
	}
	order, observeErr := e.observeToTerminal(
		ctx, credentials, instrument, adapter, order,
		orderObservation{subscription: subscription, watch: watch},
	)
	if observeErr != nil || !terminalStatus(order.Status) {
		execution.Status = "reconciling"
		_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
		_ = e.store.AppendArbitrageEvent(
			ctx, combination.ID, execution.ID, "hedge_emergency_skipped_uncertain",
			map[string]any{
				"emergency_order_id":    stored.ID,
				"previous_order_status": order.Status,
				"trigger_reason":        trigger,
			},
		)
		return parseDecimal(order.FilledQuantity), ErrVenueUncertain
	}
	_ = e.store.AppendArbitrageEvent(ctx, combination.ID, execution.ID, "hedge_emergency_completed", map[string]any{
		"emergency_order_id": stored.ID,
		"filledQuantity":     order.FilledQuantity,
		"status":             order.Status,
		"trigger_reason":     trigger,
	})
	if order.Status == "rejected" {
		return parseDecimal(order.FilledQuantity), fmt.Errorf(
			"%w: emergency hedge rejected", ErrVenueRejected,
		)
	}
	return parseDecimal(order.FilledQuantity), nil
}

func (e *ArbitrageExecutor) completeExecution(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	mark decimal.Decimal,
) error {
	if err := e.requireExecutionOrdersTerminal(ctx, combination, execution); err != nil {
		return err
	}
	refreshed, err := e.refreshArbitragePositions(ctx, combination, execution)
	if err != nil {
		return err
	}
	combination = refreshed
	fillA := parseDecimal(execution.LegAFilledQuantity)
	fillB := parseDecimal(execution.LegBFilledQuantity)
	if lastCloseClipMustMatchFills(combination, *execution) && !fillA.Equal(fillB) {
		remaining := fillA.Sub(fillB).Abs()
		result, failErr := e.store.FailLastCloseClipUnbalanced(
			ctx, combination, *execution, fillA.String(), fillB.String(), remaining.String(),
		)
		if failErr != nil {
			return failErr
		}
		*execution = result.Execution
		return nil
	}
	execution.DeltaNotional = deltaNotional(fillA, fillB, mark).String()
	execution.Status = "completed"
	execution.ErrorMessage = ""
	if _, err := e.store.UpdateArbitrageExecution(ctx, *execution); err != nil {
		return err
	}
	delta := signedPositionDelta(execution.Direction, fillA, fillB, mark)
	updated, err := e.store.UpdateArbitragePositionFromBase(
		ctx, combination.ID, mark.String(), delta.Abs().String(),
	)
	if err == nil {
		_ = e.store.AppendArbitrageEvent(ctx, combination.ID, execution.ID, "execution_completed", map[string]any{
			"positionDelta":              delta.String(),
			"positionNotional":           updated.PositionNotional,
			"cumulativeTurnoverNotional": updated.CumulativeTurnoverNotional,
			"deltaNotional":              execution.DeltaNotional,
		})
	}
	return err
}

func (e *ArbitrageExecutor) requireExecutionOrdersTerminal(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
) error {
	orders, err := e.store.ListArbitrageOrders(
		ctx, combination.OwnerUsername, combination.ID,
	)
	if err != nil {
		return err
	}
	for _, order := range orders {
		if order.ArbitrageExecutionID != execution.ID {
			continue
		}
		if !terminalStatus(order.Status) {
			_, parkErr := e.parkMakerReconciling(combination, execution, order)
			if parkErr != nil {
				return parkErr
			}
			return ErrArbitrageReconciling
		}
	}
	return nil
}

func (e *ArbitrageExecutor) executionDependencies(
	ctx context.Context,
	owner string,
	leg ArbitrageLeg,
) (Instrument, Credentials, exchange.Adapter, error) {
	instrument, err := e.catalog.Get(ctx, leg.InstrumentID)
	if err != nil {
		return Instrument{}, Credentials{}, nil, err
	}
	credentials, err := e.credentials.GetInternal(ctx, e.token, owner, leg.TradingAccountID)
	if err != nil {
		return Instrument{}, Credentials{}, nil, err
	}
	adapter, ok := e.venues.Adapter(leg.Exchange)
	if !ok {
		return Instrument{}, Credentials{}, nil, ErrUnsupportedExchange
	}
	return instrument, credentials, adapter, nil
}

func warmArbitrageOrderTransport(
	ctx context.Context,
	adapter exchange.Adapter,
	credentials Credentials,
	instrument Instrument,
) error {
	warmer, ok := adapter.(exchange.OrderTransportWarmer)
	if !ok {
		return nil
	}
	return warmer.WarmOrderTransport(ctx, toVenueCredentials(credentials), toVenueInstrument(instrument))
}

func (e *ArbitrageExecutor) orderIntent(
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	leg ArbitrageLeg,
	instrument Instrument,
	side, orderType, quantity, price, role string,
	attempt int,
) Order {
	legName := arbitrageLegName(combination, leg)
	if legName == "" {
		legName = "a"
	}
	idempotency := fmt.Sprintf("arb:%s:%s:%s:%d", execution.ID, legName, role, attempt)
	reduceOnly := execution.ReduceOnly && instrument.ContractType == "perpetual"
	fingerprintExtras := []string(nil)
	if reduceOnly {
		fingerprintExtras = []string{"reduce_only"}
	}
	return Order{
		IdempotencyKey: idempotency, OwnerUsername: combination.OwnerUsername,
		TradingAccountID: leg.TradingAccountID, ProductName: leg.ProductName,
		Exchange: leg.Exchange, InstrumentID: instrument.ID,
		ContractType: instrument.ContractType, ExchangeSymbol: instrument.ExchangeSymbol,
		BaseAsset: instrument.BaseAsset, QuoteAsset: instrument.QuoteAsset,
		Side: side, OrderType: orderType, Quantity: quantity, Price: price,
		RequestFingerprint: requestFingerprint(
			leg.TradingAccountID, instrument.ID, side, orderType, quantity, price, fingerprintExtras...,
		),
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: legName, ArbitrageRole: role,
		ReduceOnly: reduceOnly,
	}
}

func (e *ArbitrageExecutor) reusableHedgeAttempt(
	ctx context.Context,
	combination ArbitrageCombination,
	execution ArbitrageExecution,
) (int64, string, bool) {
	if execution.HedgeOrderID != "" || e.store == nil {
		return 0, "", false
	}
	orders, err := e.store.ListArbitrageOrders(ctx, combination.OwnerUsername, combination.ID)
	if err != nil {
		return 0, "", false
	}
	for _, order := range orders {
		if order.ArbitrageExecutionID != execution.ID || order.ArbitrageRole != "hedge" {
			continue
		}
		if terminalStatus(order.Status) {
			continue
		}
		seq, ok := hedgeSequenceFromIdempotency(order.IdempotencyKey)
		if ok {
			return seq, order.IdempotencyKey, true
		}
	}
	return 0, "", false
}

func hedgeSequenceFromIdempotency(key string) (int64, bool) {
	parts := strings.Split(key, ":")
	if len(parts) != 5 || parts[0] != "arb" || parts[3] != "hedge" {
		return 0, false
	}
	seq, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil {
		return 0, false
	}
	return seq, true
}

func (e *ArbitrageExecutor) queryInternal(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	adapter exchange.Adapter,
	order Order,
) (Order, error) {
	if e.orderStreams != nil {
		e.restFallbacks.Add(1)
	}
	resolution, err := resolveVenueOrder(ctx, adapter, toVenueCredentials(credentials), exchange.QueryRequest{
		Instrument: toVenueInstrument(instrument), ClientOrderID: order.ClientOrderID,
		VenueOrderID: order.VenueOrderID, CreatedAt: order.CreatedAt,
	})
	if err != nil {
		return order, err
	}
	if resolution.ConfirmedAbsent {
		return order, exchange.ErrOrderNotFound
	}
	result := resolution.Result
	return e.service.persistResult(ctx, order.ID, "arbitrage_observe", normalizeVenueResult(order, result))
}

func (e *ArbitrageExecutor) cancelInternal(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	adapter exchange.Adapter,
	order Order,
) (Order, error) {
	if terminalStatus(order.Status) {
		return order, nil
	}
	result, err := adapter.CancelOrder(ctx, toVenueCredentials(credentials), exchange.CancelRequest{
		Instrument: toVenueInstrument(instrument), ClientOrderID: order.ClientOrderID,
		VenueOrderID: order.VenueOrderID,
	})
	shouldPersist := err == nil ||
		errors.Is(err, exchange.ErrAmbiguousCancel) ||
		errors.Is(err, exchange.ErrUncertain) ||
		hasVenueResult(result)
	if shouldPersist && e.service != nil {
		persisted, persistErr := e.service.persistResult(
			ctx, order.ID, "arbitrage_cancel", normalizeVenueResult(order, result),
		)
		if persistErr == nil {
			order = persisted
		} else if err == nil {
			err = persistErr
		}
		return order, err
	}
	if result.Status != "" {
		order.Status = result.Status
		if result.VenueOrderID != "" {
			order.VenueOrderID = result.VenueOrderID
		}
		if result.FilledQuantity != "" {
			order.FilledQuantity = result.FilledQuantity
		}
	}
	return order, err
}

func (e *ArbitrageExecutor) settleCanceledOrder(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	adapter exchange.Adapter,
	order Order,
	observation orderObservation,
) (Order, error) {
	if confirmedAbsentReliableZeroFill(order) {
		return order, nil
	}
	if !terminalStatus(order.Status) {
		return e.observeToTerminal(
			ctx, credentials, instrument, adapter, order, observation,
		)
	}
	if observation.watch != nil {
		timer := time.NewTimer(e.observerInterval)
		defer timer.Stop()
		select {
		case update, ok := <-observation.watch.Updates():
			if ok {
				if refreshed, err := e.applyOrderStreamUpdate(
					ctx, order, update, instrument,
				); err == nil {
					order = refreshed
				}
			}
		case <-timer.C:
		case <-ctx.Done():
			return order, ctx.Err()
		}
	}
	refreshed, err := e.queryInternal(ctx, credentials, instrument, adapter, order)
	if err != nil {
		return order, err
	}
	return refreshed, nil
}

func (e *ArbitrageExecutor) observeToTerminal(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	adapter exchange.Adapter,
	order Order,
	observation ...orderObservation,
) (Order, error) {
	if terminalStatus(order.Status) {
		return order, nil
	}
	var subscription *orderstream.Subscription
	var watch *orderstream.OrderWatch
	if len(observation) > 0 {
		subscription = observation[0].subscription
		watch = observation[0].watch
	}
	var updates <-chan orderstream.Update
	if watch != nil {
		defer watch.Close()
		updates = watch.Updates()
	}
	timer := time.NewTimer(e.timeout)
	defer timer.Stop()
	interval := e.observerInterval
	observer := time.NewTimer(interval)
	defer observer.Stop()
	lastREST := time.Now()
	var streamGeneration uint64
	for {
		select {
		case <-ctx.Done():
			return order, ctx.Err()
		case <-timer.C:
			refreshed, err := e.queryInternal(ctx, credentials, instrument, adapter, order)
			if err == nil {
				order = refreshed
				if terminalStatus(order.Status) {
					return order, nil
				}
			}
			return order, ErrVenueUncertain
		case update, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			refreshed, err := e.applyOrderStreamUpdate(ctx, order, update, instrument)
			if errors.Is(err, errHyperliquidFillUnknown) {
				return order, ErrVenueUncertain
			}
			if err != nil {
				continue
			}
			order = refreshed
			if terminalStatus(order.Status) {
				return order, nil
			}
			continue
		case <-observer.C:
		}
		healthy := subscription != nil && subscription.Healthy()
		generation := uint64(0)
		if healthy {
			generation = subscription.Generation()
		}
		if healthy && generation == streamGeneration && time.Since(lastREST) < e.streamAudit {
			observer.Reset(min(e.streamAudit-time.Since(lastREST), time.Second))
			continue
		}
		refreshed, err := e.queryInternal(ctx, credentials, instrument, adapter, order)
		lastREST = time.Now()
		if err != nil {
			interval = min(2*time.Second, interval*2)
			observer.Reset(interval)
			continue
		}
		streamGeneration = generation
		interval = e.observerInterval
		observer.Reset(interval)
		order = refreshed
		if terminalStatus(order.Status) {
			return order, nil
		}
	}
}

type orderObservation struct {
	subscription *orderstream.Subscription
	watch        *orderstream.OrderWatch
}

func (e *ArbitrageExecutor) latestForLeg(leg ArbitrageLeg) (marketdata.BBO, error) {
	key, err := marketdata.NewKey(leg.Exchange, leg.ContractType, leg.ExchangeSymbol)
	if err != nil {
		return marketdata.BBO{}, err
	}
	return e.market.Latest(key)
}

func (e *ArbitrageExecutor) subscribeOrders(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
) *orderstream.Subscription {
	if e.orderStreams == nil {
		return nil
	}
	subscription, err := e.orderStreams.Subscribe(ctx, orderstream.Key{
		Account: strconv.FormatInt(credentials.TradingAccountID, 10),
		Venue:   credentials.Exchange, Product: instrument.ContractType,
	}, toOrderStreamCredentials(credentials))
	if err != nil {
		e.logger.Warn("arbitrage private order stream unavailable",
			"account_id", credentials.TradingAccountID,
			"exchange", credentials.Exchange,
			"product", instrument.ContractType,
			"error", sanitizeError(err))
		return nil
	}
	return subscription
}

func (e *ArbitrageExecutor) watchOrder(
	ctx context.Context,
	subscription *orderstream.Subscription,
	adapter exchange.Adapter,
	order Order,
) *orderstream.OrderWatch {
	if subscription == nil {
		return nil
	}
	clientOrderID := order.ClientOrderID
	if encoder, ok := adapter.(exchange.ClientOrderIDEncoder); ok {
		clientOrderID = encoder.VenueClientOrderID(clientOrderID)
	}
	watch, err := subscription.WatchOrder(ctx, clientOrderID)
	if err != nil {
		e.logger.Warn("arbitrage order stream watch unavailable",
			"order_id", order.ID, "error", sanitizeError(err))
		return nil
	}
	return watch
}

func hyperliquidTerminalFillUnknown(exchangeName string, update orderstream.Update) bool {
	if !strings.EqualFold(exchangeName, "hyperliquid") ||
		update.Type != orderstream.UpdateOrder {
		return false
	}
	status := string(update.Status)
	if status == string(orderstream.StatusNew) {
		status = "open"
	}
	return terminalStatus(status) && strings.TrimSpace(update.CumulativeFilled) == ""
}

func streamFillSkipsVenueWatermark(
	exchangeName string,
	updateType orderstream.UpdateType,
	cumulativeFilled string,
) bool {
	if updateType != orderstream.UpdateTrade {
		return false
	}
	if strings.EqualFold(exchangeName, "hyperliquid") {
		return true
	}
	return strings.EqualFold(exchangeName, "bitget") &&
		strings.TrimSpace(cumulativeFilled) == ""
}

func (e *ArbitrageExecutor) applyOrderStreamUpdate(
	ctx context.Context,
	order Order,
	update orderstream.Update,
	instruments ...Instrument,
) (Order, error) {
	if update.Type == orderstream.UpdateConnection {
		return order, nil
	}
	e.streamEvents.Add(1)
	if lag := time.Since(update.EventTime); !update.EventTime.IsZero() && lag > 0 {
		e.lastStreamLagMS.Store(uint64(lag.Milliseconds()))
	}
	status := string(update.Status)
	if status == string(orderstream.StatusNew) {
		status = "open"
	}
	cumulativeFilled := update.CumulativeFilled
	lastFilled := update.LastFilled
	if len(instruments) > 0 {
		venueInstrument := toVenueInstrument(instruments[0])
		var err error
		if cumulativeFilled != "" {
			cumulativeFilled, err = exchange.FromVenueQuantity(venueInstrument, cumulativeFilled)
			if err != nil {
				return order, err
			}
		}
		if lastFilled != "" {
			lastFilled, err = exchange.FromVenueQuantity(venueInstrument, lastFilled)
			if err != nil {
				return order, err
			}
		}
	}
	if hyperliquidTerminalFillUnknown(order.Exchange, orderstream.Update{
		Type: update.Type, Status: update.Status, CumulativeFilled: cumulativeFilled,
	}) {
		return order, errHyperliquidFillUnknown
	}
	streamUpdate := StreamUpdate{
		Result: VenueResult{
			VenueOrderID: update.VenueOrderID, Status: status,
			FilledQuantity: cumulativeFilled, AveragePrice: update.AveragePrice,
			ErrorCode: update.ErrorCode, ErrorMessage: update.ErrorMessage,
		},
		EventAt: update.EventTime, ReceivedAt: time.Now(),
		SkipVenueWatermark: streamFillSkipsVenueWatermark(
			order.Exchange, update.Type, cumulativeFilled,
		),
	}
	currentFilled := parseDecimal(order.FilledQuantity)
	incomingFilled := parseDecimal(cumulativeFilled)
	fillQuantity := decimal.Zero
	if incomingFilled.GreaterThan(currentFilled) {
		fillQuantity = incomingFilled.Sub(currentFilled)
	} else if cumulativeFilled == "" {
		fillQuantity = parseDecimal(lastFilled).Abs()
	}
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
	updated, err := e.orders.ApplyStreamUpdate(ctx, order.ID, streamUpdate)
	if err != nil {
		return order, err
	}
	_ = e.orders.AppendEvent(ctx, order.ID, "arbitrage_stream", map[string]any{
		"status": updated.Status, "filledQuantity": updated.FilledQuantity,
		"venueOrderId": updated.VenueOrderID, "tradeId": update.TradeID,
	})
	return updated, nil
}

func protectedIOCPrice(
	bbo marketdata.BBO,
	side string,
	tick decimal.Decimal,
	protectionBps int,
) string {
	offset := decimal.NewFromInt(int64(protectionBps)).Div(decimal.NewFromInt(10000))
	if side == "buy" {
		price := parsePositiveDecimal(bbo.AskPrice).Mul(decimal.NewFromInt(1).Add(offset))
		return ceilToStep(price, tick).String()
	}
	price := parsePositiveDecimal(bbo.BidPrice).Mul(decimal.NewFromInt(1).Sub(offset))
	return floorToStep(price, tick).String()
}

func normalizeHedgeIOCPrice(instrument Instrument, side, price string) (string, error) {
	if !strings.EqualFold(strings.TrimSpace(instrument.Exchange), "hyperliquid") {
		return price, nil
	}
	value, err := decimal.NewFromString(strings.TrimSpace(price))
	if err != nil || !value.IsPositive() {
		return "", fmt.Errorf("%w: invalid Hyperliquid hedge price %q", ErrInvalidArgument, price)
	}
	step, err := decimal.NewFromString(strings.TrimSpace(instrument.QuantityStep))
	if err != nil || !step.IsPositive() {
		return "", ErrInstrumentUnavailable
	}
	szDecimals := -1
	for candidate := 0; candidate <= 6; candidate++ {
		if step.Equal(decimal.New(1, -int32(candidate))) {
			szDecimals = candidate
			break
		}
	}
	if szDecimals < 0 {
		return "", ErrInstrumentUnavailable
	}

	magnitude := int32(value.Abs().NumDigits()) + value.Exponent() - 1
	decimals := int32(4) - magnitude
	if decimals < 0 {
		decimals = 0
	}
	maxDecimals := int32(6 - szDecimals)
	if decimals > maxDecimals {
		decimals = maxDecimals
	}
	increment := decimal.New(1, -decimals)
	switch strings.ToLower(strings.TrimSpace(side)) {
	case "buy":
		value = ceilToStep(value, increment)
	case "sell":
		value = floorToStep(value, increment)
	default:
		return "", fmt.Errorf("%w: unsupported hedge side %q", ErrInvalidArgument, side)
	}
	if !value.IsPositive() {
		return "", fmt.Errorf("%w: Hyperliquid hedge price rounds to zero", ErrInvalidArgument)
	}
	return value.String(), nil
}

func passiveMakerPrice(price, side string, tick decimal.Decimal) string {
	value := parsePositiveDecimal(price)
	if side == "sell" {
		return ceilToStep(value, tick).String()
	}
	return floorToStep(value, tick).String()
}

func makerShouldReprice(distance, tick decimal.Decimal, ticks int) bool {
	return tick.IsPositive() && ticks > 0 &&
		distance.GreaterThanOrEqual(tick.Mul(decimal.NewFromInt(int64(ticks))))
}

func errorsIsCanceled(err error) bool {
	return err != nil && errors.Is(err, context.Canceled)
}
