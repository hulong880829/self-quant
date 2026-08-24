package trader

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
	"selfquant/backend/internal/trader/orderstream"
)

type ArbitrageExecutor struct {
	store            arbitrageStore
	orders           arbitrageOrderStore
	service          *Service
	catalog          instrumentCatalog
	credentials      internalCredentialProvider
	venues           *exchange.Registry
	market           *marketdata.Manager
	orderStreams     *orderstream.Manager
	token            string
	observerInterval time.Duration
	repriceTicks     int
	iocTicks         int
	iocRetries       int
	timeout          time.Duration
	streamAudit      time.Duration
	logger           *slog.Logger
	mu               sync.Mutex
	active           map[string]*arbitrageRun
	streamEvents     atomic.Uint64
	restFallbacks    atomic.Uint64
	lastStreamLagMS  atomic.Uint64
	lastHedgeLatency atomic.Uint64
}

type ArbitrageExecutorStats struct {
	StreamEvents       uint64
	RESTFallbacks      uint64
	LastStreamLagMS    uint64
	LastHedgeLatencyMS uint64
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

type arbitrageRun struct {
	combinationID string
	cancel        context.CancelFunc
	done          chan struct{}
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
	repriceTicks, iocTicks, iocRetries int,
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
		iocTicks: iocTicks, iocRetries: iocRetries, timeout: timeout,
		logger: logger, active: make(map[string]*arbitrageRun),
	}
}

func (e *ArbitrageExecutor) Execute(
	parent context.Context,
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	bboA, bboB marketdata.BBO,
) {
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
		e.mu.Unlock()
	}()

	started := time.Now()
	var err error
	switch combination.ExecutionMode {
	case "maker_then_hedge":
		err = e.executeMakerThenHedge(ctx, combination, &execution, bboA, bboB)
	case "simultaneous_market":
		err = e.executeSimultaneousMarket(ctx, combination, &execution, bboA, bboB)
	default:
		err = ErrInvalidArgument
	}
	if err != nil {
		execution.ErrorMessage = sanitizeError(err)
		if errorsIsCanceled(err) && !executionHasExposure(execution) {
			execution.Status = "canceled"
			_, _ = e.store.UpdateArbitrageExecution(context.Background(), execution)
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
			_, _ = e.store.RecordArbitrageFailure(
				context.Background(), combination.ID, execution.ErrorMessage,
			)
			e.logger.Error("arbitrage execution needs recovery",
				"combination_id", combination.ID, "execution_id", execution.ID,
				"direction", execution.Direction, "error", sanitizeError(err))
			return
		}
		execution.Status = "failed"
		_, _ = e.store.UpdateArbitrageExecution(context.Background(), execution)
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
	if leftover, leftoverErr := e.store.GetActiveArbitrageExecution(ctx, combination.ID); leftoverErr == nil {
		leftover.Status = "canceled"
		_, _ = e.store.UpdateArbitrageExecution(ctx, leftover)
	}
	combination.Status = "closed"
	combination.MarketDataStale = true
	_, err := e.store.UpdateArbitrageCombinationRuntime(ctx, combination)
	if err == nil {
		_ = e.store.AppendArbitrageEvent(ctx, combination.ID, "", "closed", map[string]any{
			"activeExecutions": len(runs),
		})
	}
	return err
}

func (e *ArbitrageExecutor) executeMakerThenHedge(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	bboA, bboB marketdata.BBO,
) error {
	aSide, bSide, ok := arbitrageLegSides(execution.Direction)
	if !ok {
		return ErrInvalidArgument
	}
	makerLeg, hedgeLeg := combination.LegA, combination.LegB
	makerSide, hedgeSide := aSide, bSide
	makerBBO := bboA
	if combination.MakerLeg == "b" {
		makerLeg, hedgeLeg = combination.LegB, combination.LegA
		makerSide, hedgeSide = bSide, aSide
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
	makerOrders := e.subscribeOrders(ctx, makerCredentials, makerInstrument)
	if makerOrders != nil {
		defer makerOrders.Close()
	}
	hedgeOrders := e.subscribeOrders(ctx, hedgeCredentials, hedgeInstrument)
	if hedgeOrders != nil {
		defer hedgeOrders.Close()
	}
	if execution.Status == "hedging" && execution.HedgeOrderID != "" {
		recovering, getErr := e.orders.GetByOwner(
			ctx, combination.OwnerUsername, execution.HedgeOrderID,
		)
		if getErr != nil {
			return getErr
		}
		watch := e.watchOrder(ctx, hedgeOrders, recovering)
		recovering, _ = e.observeToTerminal(
			ctx, hedgeCredentials, hedgeInstrument, hedgeAdapter, recovering,
			orderObservation{subscription: hedgeOrders, watch: watch},
		)
		recoveredFill := parseDecimal(recovering.FilledQuantity)
		if combination.MakerLeg == "b" {
			execution.LegAFilledQuantity = parseDecimal(
				execution.LegAFilledQuantity,
			).Add(recoveredFill).String()
		} else {
			execution.LegBFilledQuantity = parseDecimal(
				execution.LegBFilledQuantity,
			).Add(recoveredFill).String()
		}
		execution.Status = "maker_open"
		if updated, updateErr := e.store.UpdateArbitrageExecution(ctx, *execution); updateErr == nil {
			*execution = updated
		} else {
			return updateErr
		}
	}
	price := makerBBO.BidPrice
	if makerSide == "sell" {
		price = makerBBO.AskPrice
	}
	price = passiveMakerPrice(price, makerSide, parsePositiveDecimal(makerInstrument.PriceTick))
	priceValue := parsePositiveDecimal(price)
	step := parsePositiveDecimal(makerInstrument.QuantityStep)
	qty := baseQuantityForNotional(
		executionNotional(combination, *execution), priceValue, step,
	)
	if !qty.IsPositive() {
		return ErrRiskLimit
	}
	execution.TargetBaseQuantity = qty.String()
	applyMakerFill := func(order Order) error {
		makerFilled := parseDecimal(order.FilledQuantity)
		var hedged decimal.Decimal
		if combination.MakerLeg == "b" {
			if makerFilled.LessThan(parseDecimal(execution.LegBFilledQuantity)) {
				makerFilled = parseDecimal(execution.LegBFilledQuantity)
			}
			execution.LegBFilledQuantity = makerFilled.String()
			hedged = parseDecimal(execution.LegAFilledQuantity)
		} else {
			if makerFilled.LessThan(parseDecimal(execution.LegAFilledQuantity)) {
				makerFilled = parseDecimal(execution.LegAFilledQuantity)
			}
			execution.LegAFilledQuantity = makerFilled.String()
			hedged = parseDecimal(execution.LegBFilledQuantity)
		}
		unhedged := floorToStep(
			makerFilled.Sub(hedged), parsePositiveDecimal(hedgeInstrument.QuantityStep),
		)
		if !unhedged.IsPositive() {
			_, _ = e.store.UpdateArbitrageExecution(context.Background(), *execution)
			return nil
		}
		execution.Status = "hedging"
		if updated, updateErr := e.store.UpdateArbitrageExecution(context.Background(), *execution); updateErr == nil {
			*execution = updated
		}
		hedgeStarted := time.Now()
		filled, hedgeErr := e.hedgeIOC(
			context.Background(), combination, execution, hedgeLeg, hedgeInstrument,
			hedgeCredentials, hedgeAdapter, hedgeSide, unhedged, hedgeOrders,
		)
		e.lastHedgeLatency.Store(uint64(time.Since(hedgeStarted).Milliseconds()))
		if combination.MakerLeg == "b" {
			execution.LegAFilledQuantity = hedged.Add(filled).String()
		} else {
			execution.LegBFilledQuantity = hedged.Add(filled).String()
		}
		execution.Status = "maker_open"
		_, _ = e.store.UpdateArbitrageExecution(context.Background(), *execution)
		return hedgeErr
	}
	attempt := 0
	for {
		var stored Order
		created := false
		if execution.MakerOrderID != "" {
			stored, err = e.orders.GetByOwner(ctx, combination.OwnerUsername, execution.MakerOrderID)
			execution.MakerOrderID = ""
		} else {
			intent := e.orderIntent(
				combination, *execution, makerLeg, makerInstrument,
				makerSide, "limit", qty.String(), price, "maker", attempt,
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
		execution.MakerOrderID = stored.ID
		execution.Attempt = attempt
		execution.Status = "maker_open"
		_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
		_ = e.store.AppendArbitrageEvent(ctx, combination.ID, execution.ID, "maker_submitted", map[string]any{
			"leg": combination.MakerLeg, "side": makerSide, "price": price,
			"quantity": qty.String(), "attempt": attempt,
		})
		e.logger.Info("arbitrage maker submit",
			"combination_id", combination.ID, "execution_id", execution.ID,
			"attempt", attempt, "leg", combination.MakerLeg,
			"trigger_to_submit_ms", time.Since(execution.CreatedAt).Milliseconds())
		order := stored
		makerWatch := e.watchOrder(ctx, makerOrders, stored)
		observation := orderObservation{subscription: makerOrders, watch: makerWatch}
		var submitErr error
		if created {
			lock := e.service.accountLock(makerCredentials.TradingAccountID)
			lock.Lock()
			order, submitErr = e.service.submitWithOptions(
				ctx, makerAdapter, makerCredentials, makerInstrument, stored, "GTC", true,
			)
			lock.Unlock()
		}
		if submitErr != nil && order.ID == "" {
			return submitErr
		}
		order, reprice, observeErr := e.observeMaker(
			ctx, combination, makerLeg, makerInstrument, makerCredentials,
			makerAdapter, order, makerSide, price, observation, applyMakerFill,
		)
		if observeErr != nil && !errorsIsCanceled(observeErr) {
			return observeErr
		}
		filled := parseDecimal(order.FilledQuantity)
		if filled.IsPositive() {
			return e.completeExecution(context.Background(), combination, execution, priceValue)
		}
		if ctx.Err() != nil {
			execution.Status = "canceled"
			_, _ = e.store.UpdateArbitrageExecution(context.Background(), *execution)
			return nil
		}
		if !reprice {
			return fmt.Errorf("%w: maker ended without fill", ErrVenueRejected)
		}
		attempt++
		latest, latestErr := e.latestForLeg(makerLeg)
		if latestErr != nil {
			return ErrMarketDataStale
		}
		price = latest.BidPrice
		if makerSide == "sell" {
			price = latest.AskPrice
		}
		price = passiveMakerPrice(price, makerSide, parsePositiveDecimal(makerInstrument.PriceTick))
	}
}

func (e *ArbitrageExecutor) executeSimultaneousMarket(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	bboA, bboB marketdata.BBO,
) error {
	aSide, bSide, ok := arbitrageLegSides(execution.Direction)
	if !ok {
		return ErrInvalidArgument
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
	qtyA := baseQuantityForNotional(notional, priceA, parsePositiveDecimal(instrumentA.QuantityStep))
	qtyB := baseQuantityForNotional(notional, priceB, parsePositiveDecimal(instrumentB.QuantityStep))
	if !qtyA.IsPositive() || !qtyB.IsPositive() {
		return ErrRiskLimit
	}
	intents, created, err := e.orders.CreateArbitrageIntents(ctx, []Order{
		e.orderIntent(combination, *execution, combination.LegA, instrumentA, aSide, "market", qtyA.String(), "", "market", 0),
		e.orderIntent(combination, *execution, combination.LegB, instrumentB, bSide, "market", qtyB.String(), "", "market", 0),
	})
	if err != nil {
		return err
	}
	watchA := e.watchOrder(ctx, ordersA, intents[0])
	watchB := e.watchOrder(ctx, ordersB, intents[1])
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
			order, submitErr = e.service.submit(ctx, adapter, credentials, instrument, intent)
		} else {
			order, submitErr = e.service.recover(ctx, adapter, credentials, instrument, intent)
			if submitErr != nil {
				submitErr = ErrVenueUncertain
			}
		}
		lock.Unlock()
		results <- result{leg: leg, order: order, err: submitErr}
	}
	go submit("a", adapterA, credentialsA, instrumentA, intents[0], created[0])
	go submit("b", adapterB, credentialsB, instrumentB, intents[1], created[1])
	var orderA, orderB Order
	for range 2 {
		result := <-results
		if result.leg == "a" {
			orderA = result.order
		} else {
			orderB = result.order
		}
		if result.err != nil && result.order.ID == "" {
			return result.err
		}
	}
	orderA, _ = e.observeToTerminal(
		ctx, credentialsA, instrumentA, adapterA, orderA,
		orderObservation{subscription: ordersA, watch: watchA},
	)
	orderB, _ = e.observeToTerminal(
		ctx, credentialsB, instrumentB, adapterB, orderB,
		orderObservation{subscription: ordersB, watch: watchB},
	)
	fillA := parseDecimal(orderA.FilledQuantity)
	fillB := parseDecimal(orderB.FilledQuantity)
	execution.LegAFilledQuantity = fillA.String()
	execution.LegBFilledQuantity = fillB.String()
	mark := priceA.Add(priceB).Div(decimal.NewFromInt(2))
	delta := deltaNotional(fillA, fillB, mark)
	execution.DeltaNotional = delta.String()
	maxDelta := parsePositiveDecimal(combination.MaxDeltaNotional)
	if delta.GreaterThan(maxDelta) {
		var hedgeLeg ArbitrageLeg
		var hedgeInstrument Instrument
		var hedgeCredentials Credentials
		var hedgeAdapter exchange.Adapter
		var hedgeSide string
		var residual decimal.Decimal
		var hedgeOrders *orderstream.Subscription
		if fillA.GreaterThan(fillB) {
			hedgeLeg, hedgeInstrument, hedgeCredentials, hedgeAdapter = combination.LegB, instrumentB, credentialsB, adapterB
			hedgeSide, residual, hedgeOrders = bSide, fillA.Sub(fillB), ordersB
		} else {
			hedgeLeg, hedgeInstrument, hedgeCredentials, hedgeAdapter = combination.LegA, instrumentA, credentialsA, adapterA
			hedgeSide, residual, hedgeOrders = aSide, fillB.Sub(fillA), ordersA
		}
		hedged, hedgeErr := e.hedgeIOC(
			context.Background(), combination, execution, hedgeLeg, hedgeInstrument,
			hedgeCredentials, hedgeAdapter, hedgeSide, residual, hedgeOrders,
		)
		if hedgeErr != nil {
			return hedgeErr
		}
		if hedgeLeg.TradingAccountID == combination.LegA.TradingAccountID {
			fillA = fillA.Add(hedged)
			execution.LegAFilledQuantity = fillA.String()
		} else {
			fillB = fillB.Add(hedged)
			execution.LegBFilledQuantity = fillB.String()
		}
	}
	return e.completeExecution(context.Background(), combination, execution, mark)
}

func (e *ArbitrageExecutor) observeMaker(
	ctx context.Context,
	combination ArbitrageCombination,
	leg ArbitrageLeg,
	instrument Instrument,
	credentials Credentials,
	adapter exchange.Adapter,
	order Order,
	side, workingPrice string,
	observation orderObservation,
	onFill func(Order) error,
) (Order, bool, error) {
	var updates <-chan orderstream.Update
	if observation.watch != nil {
		defer observation.watch.Close()
		updates = observation.watch.Updates()
	}
	interval := e.observerInterval
	timer := time.NewTimer(interval)
	defer timer.Stop()
	lastREST := time.Now()
	var streamGeneration uint64
	applyFill := func(current Order) error {
		if onFill == nil || !parseDecimal(current.FilledQuantity).IsPositive() {
			return nil
		}
		return onFill(current)
	}
	if err := applyFill(order); err != nil {
		return order, false, err
	}
	for !terminalStatus(order.Status) {
		select {
		case <-ctx.Done():
			cancelCtx, cancel := context.WithTimeout(context.Background(), e.timeout)
			defer cancel()
			canceled, err := e.cancelInternal(cancelCtx, credentials, instrument, adapter, order)
			if err == nil {
				canceled, err = e.settleCanceledOrder(
					cancelCtx, credentials, instrument, adapter, canceled, observation,
				)
			}
			if fillErr := applyFill(canceled); err == nil && fillErr != nil {
				err = fillErr
			}
			return canceled, false, err
		case update, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			refreshed, err := e.applyOrderStreamUpdate(ctx, order, update, instrument)
			if err != nil {
				continue
			}
			order = refreshed
			if err := applyFill(order); err != nil {
				return order, false, err
			}
			if terminalStatus(order.Status) {
				break
			}
			continue
		case <-timer.C:
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
				if err := applyFill(order); err != nil {
					return order, false, err
				}
			} else {
				interval = min(2*time.Second, interval*2)
			}
		}
		timer.Reset(interval)
		if terminalStatus(order.Status) {
			break
		}
		latest, latestErr := e.latestForLeg(leg)
		if latestErr != nil {
			continue
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
			canceled, err := e.cancelInternal(ctx, credentials, instrument, adapter, order)
			if err == nil {
				canceled, err = e.settleCanceledOrder(
					ctx, credentials, instrument, adapter, canceled, observation,
				)
			}
			if fillErr := applyFill(canceled); err == nil && fillErr != nil {
				err = fillErr
			}
			return canceled, true, err
		}
	}
	return order, false, nil
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
	subscriptions ...*orderstream.Subscription,
) (decimal.Decimal, error) {
	var subscription *orderstream.Subscription
	if len(subscriptions) > 0 {
		subscription = subscriptions[0]
	}
	filled := decimal.Zero
	hedgeStarted := time.Now()
	for retry := 0; retry < e.iocRetries && filled.LessThan(target); retry++ {
		latest, err := e.latestForLeg(leg)
		if err != nil {
			return filled, ErrMarketDataStale
		}
		remaining := floorToStep(target.Sub(filled), parsePositiveDecimal(instrument.QuantityStep))
		if !remaining.IsPositive() {
			break
		}
		price := protectedIOCPrice(latest, side, parsePositiveDecimal(instrument.PriceTick), e.iocTicks)
		attempt := int(execution.HedgeSequence)
		execution.HedgeSequence++
		if updated, updateErr := e.store.UpdateArbitrageExecution(ctx, *execution); updateErr == nil {
			*execution = updated
		} else if updateErr != nil {
			return filled, updateErr
		}
		intent := e.orderIntent(
			combination, *execution, leg, instrument, side, "limit",
			remaining.String(), price, "hedge", attempt,
		)
		stored, created, err := e.orders.CreateIntent(ctx, intent)
		if err != nil {
			return filled, err
		}
		order := stored
		watch := e.watchOrder(ctx, subscription, stored)
		execution.HedgeOrderID = stored.ID
		if updated, updateErr := e.store.UpdateArbitrageExecution(ctx, *execution); updateErr == nil {
			*execution = updated
		} else {
			return filled, updateErr
		}
		var submitErr error
		if created {
			lock := e.service.accountLock(credentials.TradingAccountID)
			lock.Lock()
			order, submitErr = e.service.submitWithOptions(
				ctx, adapter, credentials, instrument, stored, "IOC", false,
			)
			lock.Unlock()
		}
		if submitErr != nil && order.ID == "" {
			return filled, submitErr
		}
		order, _ = e.observeToTerminal(
			ctx, credentials, instrument, adapter, order,
			orderObservation{subscription: subscription, watch: watch},
		)
		filled = filled.Add(parseDecimal(order.FilledQuantity))
		_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
		_ = e.store.AppendArbitrageEvent(ctx, combination.ID, execution.ID, "hedge_attempt", map[string]any{
			"leg": intent.ArbitrageLeg, "attempt": attempt, "quantity": remaining.String(),
			"filledQuantity": order.FilledQuantity, "status": order.Status,
		})
	}
	residual := target.Sub(filled).Abs()
	latest, _ := e.latestForLeg(leg)
	mark := parsePositiveDecimal(latest.AskPrice)
	if side == "sell" {
		mark = parsePositiveDecimal(latest.BidPrice)
	}
	if residual.Mul(mark).GreaterThan(parsePositiveDecimal(combination.MaxDeltaNotional)) {
		return filled, ErrRiskLimit
	}
	e.logger.Info("arbitrage hedge complete",
		"combination_id", combination.ID, "execution_id", execution.ID,
		"target_base_quantity", target.String(), "filled_base_quantity", filled.String(),
		"residual_base_quantity", residual.String(),
		"hedge_latency_ms", time.Since(hedgeStarted).Milliseconds())
	return filled, nil
}

func (e *ArbitrageExecutor) completeExecution(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	mark decimal.Decimal,
) error {
	fillA := parseDecimal(execution.LegAFilledQuantity)
	fillB := parseDecimal(execution.LegBFilledQuantity)
	execution.DeltaNotional = deltaNotional(fillA, fillB, mark).String()
	if parsePositiveDecimal(execution.DeltaNotional).GreaterThan(
		parsePositiveDecimal(combination.MaxDeltaNotional),
	) {
		return ErrRiskLimit
	}
	execution.Status = "completed"
	execution.ErrorMessage = ""
	if _, err := e.store.UpdateArbitrageExecution(ctx, *execution); err != nil {
		return err
	}
	delta := signedPositionDelta(execution.Direction, fillA, fillB, mark)
	updated, err := e.store.AddArbitragePositionDelta(ctx, combination.ID, delta.String())
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

func (e *ArbitrageExecutor) orderIntent(
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	leg ArbitrageLeg,
	instrument Instrument,
	side, orderType, quantity, price, role string,
	attempt int,
) Order {
	legName := "a"
	if leg.TradingAccountID == combination.LegB.TradingAccountID {
		legName = "b"
	}
	idempotency := fmt.Sprintf("arb:%s:%s:%s:%d", execution.ID, legName, role, attempt)
	fingerprintExtras := []string(nil)
	if execution.ReduceOnly {
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
		ReduceOnly: execution.ReduceOnly,
	}
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
	result, err := adapter.GetOrder(ctx, exchange.Credentials{
		APIKey: credentials.APIKey, APISecret: credentials.APISecret, Passphrase: credentials.Passphrase,
	}, exchange.QueryRequest{
		Instrument: toVenueInstrument(instrument), ClientOrderID: order.ClientOrderID,
		VenueOrderID: order.VenueOrderID,
	})
	if err != nil {
		return order, err
	}
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
	result, err := adapter.CancelOrder(ctx, exchange.Credentials{
		APIKey: credentials.APIKey, APISecret: credentials.APISecret, Passphrase: credentials.Passphrase,
	}, exchange.CancelRequest{
		Instrument: toVenueInstrument(instrument), ClientOrderID: order.ClientOrderID,
		VenueOrderID: order.VenueOrderID,
	})
	if err != nil {
		return order, err
	}
	return e.service.persistResult(ctx, order.ID, "arbitrage_cancel", normalizeVenueResult(order, result))
}

func (e *ArbitrageExecutor) settleCanceledOrder(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	adapter exchange.Adapter,
	order Order,
	observation orderObservation,
) (Order, error) {
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
		return order, nil
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
	}, orderstream.Credentials{
		APIKey: credentials.APIKey, Secret: credentials.APISecret,
		Passphrase: credentials.Passphrase,
	})
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
	order Order,
) *orderstream.OrderWatch {
	if subscription == nil {
		return nil
	}
	watch, err := subscription.WatchOrder(ctx, order.ClientOrderID)
	if err != nil {
		e.logger.Warn("arbitrage order stream watch unavailable",
			"order_id", order.ID, "error", sanitizeError(err))
		return nil
	}
	return watch
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
	streamUpdate := StreamUpdate{
		Result: VenueResult{
			VenueOrderID: update.VenueOrderID, Status: status,
			FilledQuantity: cumulativeFilled, AveragePrice: update.AveragePrice,
		},
		EventAt: update.EventTime, ReceivedAt: time.Now(),
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
	protectionTicks int,
) string {
	if side == "buy" {
		price := parsePositiveDecimal(bbo.AskPrice).Add(tick.Mul(decimal.NewFromInt(int64(protectionTicks))))
		return ceilToStep(price, tick).String()
	}
	price := parsePositiveDecimal(bbo.BidPrice).Sub(tick.Mul(decimal.NewFromInt(int64(protectionTicks))))
	return floorToStep(price, tick).String()
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
	return err == context.Canceled || strings.Contains(strings.ToLower(err.Error()), "canceled")
}
