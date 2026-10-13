package trader

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/marketdata"
)

var lastCloseResidualFlattenNamespace = uuid.MustParse("8c1e5b2a-6d44-4f0a-9b31-2d4e6f8091ab")

func lastCloseResidualFlattenExecutionID(combinationID string) string {
	return uuid.NewSHA1(
		lastCloseResidualFlattenNamespace, []byte(strings.TrimSpace(combinationID)),
	).String()
}

func lastCloseResidualFlattenIdempotencyKey(executionID, legName string) string {
	return "arb:" + strings.TrimSpace(executionID) + ":" +
		strings.ToLower(strings.TrimSpace(legName)) + ":" +
		lastCloseResidualFlattenIdempotencyRole + ":0"
}

func lastCloseResidualFlattenExecution(combinationID string, execution ArbitrageExecution) bool {
	if strings.TrimSpace(execution.ID) == "" ||
		execution.ID != lastCloseResidualFlattenExecutionID(combinationID) {
		return false
	}
	return execution.ReduceOnly &&
		!execution.LastCloseClip &&
		strings.EqualFold(strings.TrimSpace(execution.PositionEffect), "close")
}

type LastCloseResidualFlattenClaim struct {
	ExpectedVersion int64
	Combination     ArbitrageCombination
	Orders          []Order
}

type LastCloseResidualDustClose struct {
	CombinationID      string
	ExecutionID        string
	ExpectedVersion    int64
	LastOrderUpdatedAt time.Time
	LegANotional       decimal.Decimal
	LegBNotional       decimal.Decimal
	LegAReducible      bool
	LegBReducible      bool
	Payload            map[string]any
}

type lastCloseResidualFlattenStore interface {
	GetLastCloseResidualFlattenExecution(context.Context, string) (ArbitrageExecution, bool, error)
	ClaimLastCloseResidualFlattenExecution(
		context.Context, LastCloseResidualFlattenClaim,
	) (ArbitrageExecution, bool, error)
	CloseLastCloseResidualWithDust(
		context.Context, LastCloseResidualDustClose,
	) (ArbitrageCombination, bool, error)
}

func (e *ArbitrageExecutor) RunLastCloseResidualFlatten(
	ctx context.Context,
	combination ArbitrageCombination,
	bboA, bboB marketdata.BBO,
) error {
	if latest, err := e.store.GetArbitrageCombinationByOwner(
		ctx, combination.OwnerUsername, combination.ID,
	); err == nil {
		combination = latest
	}
	return e.runLastCloseResidualFlatten(ctx, combination, bboA, bboB)
}

func (e *ArbitrageExecutor) runLastCloseResidualFlatten(
	ctx context.Context,
	combination ArbitrageCombination,
	bboA, bboB marketdata.BBO,
) error {
	if !strings.EqualFold(strings.TrimSpace(combination.Status), "closing") ||
		combination.PositionUncertain || combination.CircuitOpen ||
		combination.RuntimeState == "manual_intervention" ||
		combinationLooksFlat(combination) {
		return nil
	}
	store, ok := e.store.(lastCloseResidualFlattenStore)
	if !ok || e.catalog == nil {
		return nil
	}
	instrumentA, err := e.catalog.Get(ctx, combination.LegA.InstrumentID)
	if err != nil {
		return err
	}
	instrumentB, err := e.catalog.Get(ctx, combination.LegB.InstrumentID)
	if err != nil {
		return err
	}
	execution, found, err := store.GetLastCloseResidualFlattenExecution(ctx, combination.ID)
	if err != nil {
		return err
	}
	orders, err := e.store.ListArbitrageOrders(ctx, combination.OwnerUsername, combination.ID)
	if err != nil {
		return err
	}
	if !found {
		if !lastCloseResidualTinyEligible(combination, instrumentA, instrumentB, bboA, bboB) {
			return nil
		}
		if !residualFlattenDirectionAgrees(combination) {
			return nil
		}
		executions, listErr := e.store.ListArbitrageExecutions(ctx, combination.ID, 100)
		if listErr != nil {
			return listErr
		}
		if residualFlattenBlockedByOtherWork(combination.ID, orders, executions) {
			return nil
		}
		planned := lastCloseResidualFlattenOrders(combination, instrumentA, instrumentB)
		if len(planned) == 0 {
			return e.maybeCloseLastCloseResidualDust(
				ctx, store, combination, ArbitrageExecution{}, orders, instrumentA, instrumentB, bboA, bboB,
			)
		}
		claimed, won, claimErr := store.ClaimLastCloseResidualFlattenExecution(ctx, LastCloseResidualFlattenClaim{
			ExpectedVersion: combination.Version,
			Combination:     combination,
			Orders:          planned,
		})
		if claimErr != nil {
			return claimErr
		}
		if !won && claimed.ID == "" {
			return nil
		}
		execution = claimed
		found = claimed.ID != ""
		if latest, reloadErr := e.store.GetArbitrageCombinationByOwner(
			ctx, combination.OwnerUsername, combination.ID,
		); reloadErr == nil {
			combination = latest
		}
		orders, err = e.store.ListArbitrageOrders(ctx, combination.OwnerUsername, combination.ID)
		if err != nil {
			return err
		}
	}
	if !found || !lastCloseResidualFlattenExecution(combination.ID, execution) {
		return nil
	}
	related := lastCloseResidualFlattenOrdersFor(orders, execution.ID)
	if err := e.progressLastCloseResidualFlattenOrders(
		ctx, combination, &execution, related, instrumentA, instrumentB,
	); err != nil {
		return err
	}
	orders, err = e.store.ListArbitrageOrders(ctx, combination.OwnerUsername, combination.ID)
	if err != nil {
		return err
	}
	related = lastCloseResidualFlattenOrdersFor(orders, execution.ID)
	if len(related) > 0 && !lastCloseClipOrdersAllTerminal(related) {
		return nil
	}
	if !terminalArbitrageExecutionStatus(execution.Status) {
		execution.Status = "completed"
		execution.ErrorMessage = ""
		if updated, updateErr := e.store.UpdateArbitrageExecution(ctx, execution); updateErr == nil {
			execution = updated
		}
	}
	if latest, reloadErr := e.store.GetArbitrageCombinationByOwner(
		ctx, combination.OwnerUsername, combination.ID,
	); reloadErr == nil {
		combination = latest
	}
	return e.maybeCloseLastCloseResidualDust(
		ctx, store, combination, execution, orders, instrumentA, instrumentB, bboA, bboB,
	)
}

func (e *ArbitrageExecutor) progressLastCloseResidualFlattenOrders(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	orders []Order,
	instrumentA, instrumentB Instrument,
) error {
	if e.service == nil || e.credentials == nil || e.venues == nil {
		return nil
	}
	for index := range orders {
		order := orders[index]
		if terminalStatus(order.Status) {
			continue
		}
		leg := combination.LegA
		instrument := instrumentA
		if strings.EqualFold(order.ArbitrageLeg, "b") {
			leg = combination.LegB
			instrument = instrumentB
		}
		_, credentials, adapter, err := e.executionDependencies(
			ctx, combination.OwnerUsername, leg,
		)
		if err != nil {
			return err
		}
		lock := e.service.accountLock(credentials.TradingAccountID)
		lock.Lock()
		if order.Status == "pending" && strings.TrimSpace(order.VenueOrderID) == "" {
			order, err = e.service.submit(ctx, adapter, credentials, instrument, order)
		} else {
			order, err = e.service.recover(ctx, adapter, credentials, instrument, order)
		}
		lock.Unlock()
		if err != nil && !terminalStatus(order.Status) {
			return nil
		}
		observed, observeErr := e.observeToTerminal(
			ctx, credentials, instrument, adapter, order,
		)
		orders[index] = observed
		if observeErr != nil || !terminalStatus(observed.Status) {
			return nil
		}
	}
	if execution.Status == "pending" {
		execution.Status = "hedging"
		_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
	}
	return nil
}

func (e *ArbitrageExecutor) maybeCloseLastCloseResidualDust(
	ctx context.Context,
	store lastCloseResidualFlattenStore,
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	orders []Order,
	instrumentA, instrumentB Instrument,
	bboA, bboB marketdata.BBO,
) error {
	decision, ok := lastCloseResidualDustDecision(
		combination, orders, execution.ID, instrumentA, instrumentB, bboA, bboB, time.Now().UTC(),
	)
	if !ok {
		return nil
	}
	_, _, err := store.CloseLastCloseResidualWithDust(ctx, LastCloseResidualDustClose{
		CombinationID:      combination.ID,
		ExecutionID:        execution.ID,
		ExpectedVersion:    combination.Version,
		LastOrderUpdatedAt: decision.LastOrderUpdatedAt,
		LegANotional:       decision.LegANotional,
		LegBNotional:       decision.LegBNotional,
		LegAReducible:      decision.LegAReducible,
		LegBReducible:      decision.LegBReducible,
		Payload:            decision.Payload,
	})
	return err
}

type lastCloseResidualDustDecisionResult struct {
	LastOrderUpdatedAt time.Time
	LegANotional       decimal.Decimal
	LegBNotional       decimal.Decimal
	LegAReducible      bool
	LegBReducible      bool
	Payload            map[string]any
}

func residualFlattenDirectionAgrees(combination ArbitrageCombination) bool {
	owned, ownedOK := arbitrageOwnedCloseDirection(combination)
	venue, venueOK := arbitrageVenueReduceDirection(combination)
	if ownedOK && !venueOK {
		return false
	}
	if ownedOK && venueOK && owned != venue {
		return false
	}
	return true
}

func residualFlattenBlockedByOtherWork(
	combinationID string,
	orders []Order,
	executions []ArbitrageExecution,
) bool {
	residualID := lastCloseResidualFlattenExecutionID(combinationID)
	for _, execution := range executions {
		if execution.ID == residualID || terminalArbitrageExecutionStatus(execution.Status) {
			continue
		}
		return true
	}
	for _, order := range orders {
		if order.ArbitrageExecutionID == residualID || terminalStatus(order.Status) {
			continue
		}
		return true
	}
	return false
}

func lastCloseResidualTinyEligible(
	combination ArbitrageCombination,
	instrumentA, instrumentB Instrument,
	bboA, bboB marketdata.BBO,
) bool {
	now := time.Now().UTC()
	return closeFlattenTinyOwnedPositions(closeFlattenTinyOwnedInput{
		Combination:    combination,
		InstrumentA:    instrumentA,
		InstrumentB:    instrumentB,
		BBOA:           bboA,
		BBOB:           bboB,
		Now:            now,
		RequireOrders:  false,
		SignalBBOStale: 2 * time.Minute,
	})
}

func lastCloseResidualFlattenOrders(
	combination ArbitrageCombination,
	instrumentA, instrumentB Instrument,
) []Order {
	execution := ArbitrageExecution{
		ID:             lastCloseResidualFlattenExecutionID(combination.ID),
		CombinationID:  combination.ID,
		ReduceOnly:     true,
		LastCloseClip:  false,
		PositionEffect: "close",
		Status:         "hedging",
	}
	planned := planOwnedFlattenLegs(combination, instrumentA, instrumentB)
	orders := make([]Order, 0, len(planned))
	for _, leg := range planned {
		instrument := instrumentA
		if leg.LegName == "b" {
			instrument = instrumentB
		}
		if _, ok := closeFlattenPlaceRequest(instrument, signedFlattenBase(leg)); !ok {
			continue
		}
		orders = append(orders, lastCloseResidualFlattenOrderIntent(
			combination, execution, leg, instrument,
		))
	}
	return orders
}

func lastCloseResidualFlattenOrderIntent(
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	leg OwnedFlattenLeg,
	instrument Instrument,
) Order {
	return Order{
		IdempotencyKey:   lastCloseResidualFlattenIdempotencyKey(execution.ID, leg.LegName),
		OwnerUsername:    combination.OwnerUsername,
		TradingAccountID: leg.Leg.TradingAccountID,
		ProductName:      leg.Leg.ProductName,
		Exchange:         leg.Leg.Exchange,
		InstrumentID:     instrument.ID,
		ContractType:     instrument.ContractType,
		ExchangeSymbol:   instrument.ExchangeSymbol,
		BaseAsset:        instrument.BaseAsset,
		QuoteAsset:       instrument.QuoteAsset,
		Side:             leg.Side,
		OrderType:        "market",
		Quantity:         leg.BaseQuantity.String(),
		RequestFingerprint: requestFingerprint(
			leg.Leg.TradingAccountID, instrument.ID, leg.Side, "market",
			leg.BaseQuantity.String(), "", "reduce_only",
		),
		ArbitrageExecutionID: execution.ID,
		ArbitrageLeg:         leg.LegName,
		ArbitrageRole:        "market",
		ReduceOnly:           true,
		Status:               "pending",
	}
}

func lastCloseResidualFlattenOrdersFor(orders []Order, executionID string) []Order {
	needle := ":" + lastCloseResidualFlattenIdempotencyRole + ":"
	matched := make([]Order, 0, 2)
	for _, order := range orders {
		if strings.TrimSpace(order.ArbitrageExecutionID) != strings.TrimSpace(executionID) {
			continue
		}
		if !strings.Contains(order.IdempotencyKey, needle) {
			continue
		}
		matched = append(matched, order)
	}
	return matched
}

func lastCloseClipOrdersAllTerminal(orders []Order) bool {
	if len(orders) == 0 {
		return false
	}
	for _, order := range orders {
		if !terminalStatus(order.Status) {
			return false
		}
	}
	return true
}

func lastCloseResidualDustDecision(
	combination ArbitrageCombination,
	orders []Order,
	executionID string,
	instrumentA, instrumentB Instrument,
	bboA, bboB marketdata.BBO,
	now time.Time,
) (lastCloseResidualDustDecisionResult, bool) {
	var decision lastCloseResidualDustDecisionResult
	if !strings.EqualFold(strings.TrimSpace(combination.Status), "closing") ||
		combination.PositionUncertain || combination.CircuitOpen {
		return decision, false
	}
	for _, order := range orders {
		if !terminalStatus(order.Status) {
			return decision, false
		}
	}
	related := lastCloseResidualFlattenOrdersFor(orders, executionID)
	latest := latestOrderUpdatedAt(related)
	if combination.LastPositionReconciledAt.IsZero() ||
		combination.LastPositionReconciledAt.Equal(time.Unix(0, 0).UTC()) {
		return decision, false
	}
	if !latest.IsZero() && combination.LastPositionReconciledAt.Before(latest) {
		return decision, false
	}
	threshold := decimal.NewFromInt(arbitrageMinOpenNotional)
	legA := parseDecimal(combination.LegABasePosition)
	legB := parseDecimal(combination.LegBBasePosition)
	notionalA := decimal.Zero
	notionalB := decimal.Zero
	if !legA.IsZero() {
		priceA := closeFlattenOwnedExecutablePrice(legA, bboA)
		if !priceA.IsPositive() {
			return decision, false
		}
		notionalA = legA.Abs().Mul(priceA)
		if !notionalA.LessThan(threshold) {
			return decision, false
		}
	}
	if !legB.IsZero() {
		priceB := closeFlattenOwnedExecutablePrice(legB, bboB)
		if !priceB.IsPositive() {
			return decision, false
		}
		notionalB = legB.Abs().Mul(priceB)
		if !notionalB.LessThan(threshold) {
			return decision, false
		}
	}
	reducibleA := closeFlattenOwnedPlaceBase(combination, instrumentA, "a").Abs().IsPositive()
	reducibleB := closeFlattenOwnedPlaceBase(combination, instrumentB, "b").Abs().IsPositive()
	if reducibleA || reducibleB {
		return decision, false
	}
	if legA.IsZero() && legB.IsZero() {
		return decision, false
	}
	decision.LastOrderUpdatedAt = latest
	decision.LegANotional = notionalA
	decision.LegBNotional = notionalB
	decision.LegAReducible = reducibleA
	decision.LegBReducible = reducibleB
	decision.Payload = map[string]any{
		"combinationId": combination.ID,
		"executionId":   executionID,
		"legs": []map[string]any{
			lastCloseResidualDustLegPayload("a", combination.LegA, legA, notionalA, instrumentA, combination.LastPositionReconciledAt),
			lastCloseResidualDustLegPayload("b", combination.LegB, legB, notionalB, instrumentB, combination.LastPositionReconciledAt),
		},
		"reason":     "residual below step and minimum notional",
		"reviewTime": combination.LastPositionReconciledAt.UTC().Format(time.RFC3339Nano),
	}
	return decision, true
}

func lastCloseResidualDustLegPayload(
	name string,
	leg ArbitrageLeg,
	quantity, notional decimal.Decimal,
	instrument Instrument,
	reviewed time.Time,
) map[string]any {
	return map[string]any{
		"leg":           name,
		"exchange":      leg.Exchange,
		"accountId":     leg.TradingAccountID,
		"symbol":        instrument.ExchangeSymbol,
		"remainingQty":  quantity.String(),
		"notional":      notional.String(),
		"quantityStep":  closeFlattenQuantityStep(instrument).String(),
		"valuationTime": reviewed.UTC().Format(time.RFC3339Nano),
	}
}
