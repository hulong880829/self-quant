package trader

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/account/portfolio"
	"selfquant/backend/internal/trader/exchange"
)

const arbitragePositionAuditErrorPrefix = "account position differs from combination ledger:"

type arbitragePositionAuditStore interface {
	ListArbitrageCombinationsForPositionAudit(
		context.Context, int,
	) ([]ArbitrageCombination, error)
	UpdateArbitragePositionAudit(
		context.Context,
		string, int64, string, string, string, string, bool, string,
	) (ArbitrageCombination, error)
	AppendArbitrageEvent(
		context.Context, string, string, string, map[string]any,
	) error
	ListArbitrageOrders(context.Context, string, string) ([]Order, error)
	UpdateResultWithFillDelta(context.Context, string, VenueResult) (Order, bool, error)
	RecomputeArbitrageBasePositionsForExecution(
		context.Context, string,
	) (ArbitrageCombination, error)
	ApplyClosingExternalFlatReconcile(
		context.Context, closingExternalFlatReconcileRequest,
	) (ArbitrageCombination, bool, error)
}

type portfolioSnapshotProvider interface {
	Snapshot(
		context.Context, string, portfolio.Credentials,
	) (portfolio.Snapshot, error)
}

type portfolioSnapshotReader interface {
	portfolioSnapshotProvider
	TradeFills(
		context.Context, string, portfolio.Credentials, portfolio.TradeQuery,
	) ([]portfolio.TradeFill, error)
}

type ArbitragePositionAuditor struct {
	store                   arbitragePositionAuditStore
	catalog                 instrumentCatalog
	credentials             internalCredentialProvider
	portfolios              portfolioSnapshotReader
	venues                  *exchange.Registry
	valuations              arbitrageValuationProvider
	token                   string
	interval                time.Duration
	timeout                 time.Duration
	batchSize               int
	logger                  *slog.Logger
	deferredVersionConflict atomic.Uint64
	deferredActiveWork      atomic.Uint64
}

type ArbitragePositionAuditStats struct {
	DeferredVersionConflict uint64
	DeferredActiveWork      uint64
}

type auditedAccount struct {
	credentials Credentials
	snapshot    portfolio.Snapshot
}

func NewArbitragePositionAuditor(
	store arbitragePositionAuditStore,
	catalog instrumentCatalog,
	credentials internalCredentialProvider,
	portfolios portfolioSnapshotReader,
	token string,
	interval, timeout time.Duration,
	batchSize int,
	logger *slog.Logger,
) *ArbitragePositionAuditor {
	if interval <= 0 {
		interval = time.Minute
	}
	if timeout <= 0 {
		timeout = 12 * time.Second
	}
	if batchSize <= 0 {
		batchSize = 20
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ArbitragePositionAuditor{
		store: store, catalog: catalog, credentials: credentials,
		portfolios: portfolios, token: token, interval: interval,
		timeout: timeout, batchSize: batchSize, logger: logger,
	}
}

func (a *ArbitragePositionAuditor) ConfigureArbitrageValuations(
	valuations arbitrageValuationProvider,
) {
	a.valuations = valuations
}

func (a *ArbitragePositionAuditor) ConfigureDEXFillReaders(
	venues *exchange.Registry,
) {
	a.venues = venues
}

func (a *ArbitragePositionAuditor) Run(ctx context.Context) {
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		a.runOnce(ctx)
		stats := a.PositionAuditStats()
		a.logger.Info(
			"arbitrage position audit stats",
			"deferred_version_conflict", stats.DeferredVersionConflict,
			"deferred_active_work", stats.DeferredActiveWork,
		)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *ArbitragePositionAuditor) PositionAuditStats() ArbitragePositionAuditStats {
	return ArbitragePositionAuditStats{
		DeferredVersionConflict: a.deferredVersionConflict.Load(),
		DeferredActiveWork:      a.deferredActiveWork.Load(),
	}
}

func (a *ArbitragePositionAuditor) runOnce(ctx context.Context) {
	items, err := a.store.ListArbitrageCombinationsForPositionAudit(
		ctx, a.batchSize,
	)
	if err != nil {
		if ctx.Err() == nil {
			a.logger.Error("list arbitrage position audits failed", "error", err)
		}
		return
	}
	cache := make(map[auditAccountKey]auditedAccountResult)
	for _, item := range items {
		if ctx.Err() != nil {
			return
		}
		if err := a.audit(ctx, item, cache); err != nil {
			var deferred *ArbitragePositionAuditDeferredError
			if errors.As(err, &deferred) {
				if deferred.Reason == ArbitragePositionAuditDeferredVersionConflict {
					a.deferredVersionConflict.Add(1)
				} else {
					a.deferredActiveWork.Add(1)
				}
				a.logger.Info(
					"arbitrage position audit deferred",
					"combination_id", item.ID,
					"reason", deferred.Reason,
					"execution_id", deferred.ExecutionID,
				)
				continue
			}
			a.logger.Warn(
				"arbitrage account position audit failed",
				"combination_id", item.ID, "error", sanitizeError(err),
			)
		}
	}
}

type auditAccountKey struct {
	owner            string
	tradingAccountID int64
	exchange         string
}

type auditedAccountResult struct {
	account auditedAccount
	err     error
}

func (a *ArbitragePositionAuditor) audit(
	ctx context.Context,
	item ArbitrageCombination,
	cache map[auditAccountKey]auditedAccountResult,
) error {
	if cache == nil {
		cache = make(map[auditAccountKey]auditedAccountResult)
	}
	instrumentA, err := a.catalog.Get(ctx, item.LegA.InstrumentID)
	if err != nil {
		return err
	}
	instrumentB, err := a.catalog.Get(ctx, item.LegB.InstrumentID)
	if err != nil {
		return err
	}
	load := func(leg ArbitrageLeg) (auditedAccount, error) {
		key := auditAccountKey{
			owner:            item.OwnerUsername,
			tradingAccountID: leg.TradingAccountID,
			exchange:         strings.ToLower(strings.TrimSpace(leg.Exchange)),
		}
		if cached, ok := cache[key]; ok {
			return cached.account, cached.err
		}
		credentials, loadErr := a.credentials.GetInternal(
			ctx, a.token, item.OwnerUsername, leg.TradingAccountID,
		)
		if loadErr != nil {
			cache[key] = auditedAccountResult{err: loadErr}
			return auditedAccount{}, loadErr
		}
		queryCtx, cancel := context.WithTimeout(ctx, a.timeout)
		defer cancel()
		snapshot, loadErr := a.portfolios.Snapshot(
			queryCtx, credentials.Exchange, portfolio.Credentials{
				APIKey: credentials.APIKey, APISecret: credentials.APISecret,
				Passphrase:   credentials.Passphrase,
				AccountIndex: credentials.AccountIndex,
			},
		)
		if loadErr != nil {
			cache[key] = auditedAccountResult{err: loadErr}
			return auditedAccount{}, loadErr
		}
		loaded := auditedAccount{credentials: credentials, snapshot: snapshot}
		cache[key] = auditedAccountResult{account: loaded}
		return loaded, nil
	}
	accountA, err := load(item.LegA)
	if err != nil {
		return err
	}
	accountB, err := load(item.LegB)
	if err != nil {
		return err
	}
	venueA, err := venueBasePosition(accountA.snapshot, item.LegA, instrumentA)
	if err != nil {
		return err
	}
	venueB, err := venueBasePosition(accountB.snapshot, item.LegB, instrumentB)
	if err != nil {
		return err
	}
	if closingExternalFlatReady(item, venueA, venueB) {
		_, _, applyErr := a.store.ApplyClosingExternalFlatReconcile(
			ctx, closingExternalFlatReconcileRequest{
				CombinationID:   item.ID,
				ExpectedVersion: item.Version,
				VenueA:          venueA,
				VenueB:          venueB,
			},
		)
		return applyErr
	}
	localA := parseDecimal(item.LegABasePosition)
	localB := parseDecimal(item.LegBBasePosition)
	expectedA, expectedB := arbitrageExpectedVenuePositions(item, localA, localB)
	diffA := venueA.Sub(expectedA)
	diffB := venueB.Sub(expectedB)
	markA, markB := a.arbitragePositionAuditMarks(item)
	if !markA.IsPositive() {
		markA = portfolioAuditMark(accountA.snapshot, item.LegA)
	}
	if !markB.IsPositive() {
		markB = portfolioAuditMark(accountB.snapshot, item.LegB)
	}
	if !markA.IsPositive() {
		markA = markB
	}
	if !markB.IsPositive() {
		markB = markA
	}
	uncertain := positionDifferenceMaterial(diffA, markA, instrumentA) ||
		positionDifferenceMaterial(diffB, markB, instrumentB)
	if uncertain {
		accounts := map[int64]auditedAccount{
			item.LegA.TradingAccountID: accountA,
			item.LegB.TradingAccountID: accountB,
		}
		if reconciled, changed, reconcileErr := a.reconcileTradeFills(
			ctx, item, accounts, instrumentA, instrumentB,
		); reconcileErr == nil && changed {
			item = reconciled
			localA = parseDecimal(item.LegABasePosition)
			localB = parseDecimal(item.LegBBasePosition)
			expectedA, expectedB = arbitrageExpectedVenuePositions(item, localA, localB)
			diffA = venueA.Sub(expectedA)
			diffB = venueB.Sub(expectedB)
			markA, markB = a.arbitragePositionAuditMarks(item)
			if !markA.IsPositive() {
				markA = portfolioAuditMark(accountA.snapshot, item.LegA)
			}
			if !markB.IsPositive() {
				markB = portfolioAuditMark(accountB.snapshot, item.LegB)
			}
			if !markA.IsPositive() {
				markA = markB
			}
			if !markB.IsPositive() {
				markB = markA
			}
			uncertain = positionDifferenceMaterial(diffA, markA, instrumentA) ||
				positionDifferenceMaterial(diffB, markB, instrumentB)
		}
	}
	message := ""
	if uncertain {
		message = fmt.Sprintf(
			"%s leg_a_expected=%s leg_a_observed=%s leg_a_difference=%s "+
				"leg_b_expected=%s leg_b_observed=%s leg_b_difference=%s",
			arbitragePositionAuditErrorPrefix,
			expectedA.String(), venueA.String(), diffA.String(),
			expectedB.String(), venueB.String(), diffB.String(),
		)
	}
	updated, err := a.store.UpdateArbitragePositionAudit(
		ctx, item.ID, item.Version, venueA.String(), venueB.String(),
		diffA.String(), diffB.String(), uncertain, message,
	)
	if err != nil {
		return err
	}
	if item.PositionUncertain && !updated.PositionUncertain {
		_ = a.store.AppendArbitrageEvent(
			ctx, item.ID, "", "position_uncertain_recovered",
			map[string]any{
				"previousError":          item.ErrorMessage,
				"legAPositionDifference": updated.LegAPositionDifference,
				"legBPositionDifference": updated.LegBPositionDifference,
			},
		)
	}
	if item.RuntimeState == "manual_intervention" &&
		updated.RuntimeState == "monitoring" {
		_ = a.store.AppendArbitrageEvent(
			ctx, item.ID, "", "manual_intervention_recovered",
			map[string]any{
				"reason":                 "execution_completed_and_positions_matched",
				"carryBaseQuantity":      updated.CarryBaseQuantity,
				"legAPositionDifference": updated.LegAPositionDifference,
				"legBPositionDifference": updated.LegBPositionDifference,
			},
		)
	}
	if !item.PositionUncertain && updated.PositionUncertain &&
		strings.HasPrefix(updated.ErrorMessage, arbitragePositionAuditErrorPrefix) {
		_ = a.store.AppendArbitrageEvent(
			ctx, item.ID, "", "position_difference",
			map[string]any{
				"legALocalBasePosition":     item.LegABasePosition,
				"legAVenueBaselinePosition": item.LegAVenueBaselineBasePosition,
				"legAExpectedBasePosition":  expectedA.String(),
				"legAVenueBasePosition":     updated.LegAVenueBasePosition,
				"legAPositionDifference":    updated.LegAPositionDifference,
				"legBLocalBasePosition":     item.LegBBasePosition,
				"legBVenueBaselinePosition": item.LegBVenueBaselineBasePosition,
				"legBExpectedBasePosition":  expectedB.String(),
				"legBVenueBasePosition":     updated.LegBVenueBasePosition,
				"legBPositionDifference":    updated.LegBPositionDifference,
			},
		)
	}
	return nil
}

func venueMatchesCreationBaseline(venue, baseline decimal.Decimal) bool {
	return venue.Sub(baseline).IsZero()
}

func closingExternalFlatReady(
	item ArbitrageCombination,
	venueA, venueB decimal.Decimal,
) bool {
	if !strings.EqualFold(strings.TrimSpace(item.Status), "closing") ||
		!item.PositionUncertain {
		return false
	}
	if !strings.HasPrefix(item.ErrorMessage, arbitragePositionAuditErrorPrefix) {
		return false
	}
	if !strings.EqualFold(item.LegA.ContractType, "perpetual") ||
		!strings.EqualFold(item.LegB.ContractType, "perpetual") {
		return false
	}
	if !circuitOpenBaselineComplete(item) {
		return false
	}
	baselineA := parseDecimal(item.LegAVenueBaselineBasePosition)
	baselineB := parseDecimal(item.LegBVenueBaselineBasePosition)
	if !venueMatchesCreationBaseline(venueA, baselineA) ||
		!venueMatchesCreationBaseline(venueB, baselineB) {
		return false
	}
	return venueMatchesCreationBaseline(
		parseDecimal(item.LegAVenueBasePosition), baselineA,
	) && venueMatchesCreationBaseline(
		parseDecimal(item.LegBVenueBasePosition), baselineB,
	)
}

func arbitrageExpectedVenuePositions(
	item ArbitrageCombination,
	localA, localB decimal.Decimal,
) (decimal.Decimal, decimal.Decimal) {
	if item.VenueBaselineCapturedAt.IsZero() ||
		item.VenueBaselineCapturedAt.Equal(time.Unix(0, 0).UTC()) {
		return localA, localB
	}
	return parseDecimal(item.LegAVenueBaselineBasePosition).Add(localA),
		parseDecimal(item.LegBVenueBaselineBasePosition).Add(localB)
}

func (a *ArbitragePositionAuditor) reconcileTradeFills(
	ctx context.Context,
	item ArbitrageCombination,
	accounts map[int64]auditedAccount,
	instrumentA, instrumentB Instrument,
) (ArbitrageCombination, bool, error) {
	orders, err := a.store.ListArbitrageOrders(
		ctx, item.OwnerUsername, item.ID,
	)
	if err != nil {
		return item, false, err
	}
	ordersByVenueID := make(map[string]Order, len(orders))
	for _, order := range orders {
		if order.VenueOrderID != "" {
			ordersByVenueID[order.VenueOrderID] = order
		}
	}
	if len(ordersByVenueID) == 0 {
		return item, false, nil
	}
	since := item.CreatedAt.Add(-time.Minute)
	if item.CreatedAt.IsZero() {
		since = time.Now().UTC().Add(-7 * 24 * time.Hour)
	}
	type fillTotal struct {
		quantity decimal.Decimal
		notional decimal.Decimal
	}
	totals := make(map[string]fillTotal)
	for accountID, account := range accounts {
		type auditFill struct {
			orderID, quantity, price string
		}
		var fills []auditFill
		if dexArbitrageVenue(account.credentials.Exchange) {
			if a.venues == nil {
				return item, false, fmt.Errorf(
					"DEX fill readers are not configured for %s",
					account.credentials.Exchange,
				)
			}
			adapter, ok := a.venues.Adapter(account.credentials.Exchange)
			if !ok {
				return item, false, fmt.Errorf(
					"DEX exchange adapter is unavailable for %s",
					account.credentials.Exchange,
				)
			}
			reader, ok := adapter.(exchange.FillReader)
			if !ok {
				return item, false, fmt.Errorf(
					"DEX fill reader is unavailable for %s",
					account.credentials.Exchange,
				)
			}
			targets := []Instrument{}
			if item.LegA.TradingAccountID == accountID {
				targets = append(targets, instrumentA)
			}
			if item.LegB.TradingAccountID == accountID &&
				(item.LegA.TradingAccountID != accountID || instrumentB.ID != instrumentA.ID) {
				targets = append(targets, instrumentB)
			}
			for _, instrument := range targets {
				queryCtx, cancel := context.WithTimeout(ctx, a.timeout)
				venueFills, fillErr := reader.ListFills(
					queryCtx, toVenueCredentials(account.credentials),
					toVenueInstrument(instrument), since,
				)
				cancel()
				if fillErr != nil {
					return item, false, fillErr
				}
				for _, fill := range venueFills {
					fills = append(fills, auditFill{
						orderID: fill.VenueOrderID, quantity: fill.Quantity, price: fill.Price,
					})
				}
			}
		} else {
			queryCtx, cancel := context.WithTimeout(ctx, a.timeout)
			portfolioFills, fillErr := a.portfolios.TradeFills(
				queryCtx, account.credentials.Exchange,
				portfolio.Credentials{
					APIKey:     account.credentials.APIKey,
					APISecret:  account.credentials.APISecret,
					Passphrase: account.credentials.Passphrase,
				},
				portfolio.TradeQuery{Since: since, Until: time.Now().UTC()},
			)
			cancel()
			if fillErr != nil {
				return item, false, fillErr
			}
			for _, fill := range portfolioFills {
				fills = append(fills, auditFill{
					orderID: fill.OrderID, quantity: fill.Quantity, price: fill.Price,
				})
			}
		}
		for _, fill := range fills {
			order, ok := ordersByVenueID[fill.orderID]
			if !ok || order.TradingAccountID != accountID {
				continue
			}
			instrument := instrumentA
			if order.ArbitrageLeg == "b" {
				instrument = instrumentB
			}
			quantity, conversionErr := exchange.FromVenueQuantity(
				toVenueInstrument(instrument), fill.quantity,
			)
			if conversionErr != nil {
				return item, false, conversionErr
			}
			baseQuantity, quantityErr := decimal.NewFromString(quantity)
			price, priceErr := decimal.NewFromString(fill.price)
			if quantityErr != nil || priceErr != nil || !baseQuantity.IsPositive() ||
				price.IsNegative() {
				continue
			}
			total := totals[order.ID]
			total.quantity = total.quantity.Add(baseQuantity)
			total.notional = total.notional.Add(baseQuantity.Mul(price))
			totals[order.ID] = total
		}
	}
	changed := false
	seen := make(map[string]struct{})
	var executionIDs []string
	for _, order := range orders {
		total, ok := totals[order.ID]
		if !ok || !total.quantity.GreaterThan(parseDecimal(order.FilledQuantity)) {
			continue
		}
		average := decimal.Zero
		if total.quantity.IsPositive() {
			average = total.notional.Div(total.quantity)
		}
		updated, filledChanged, err := a.store.UpdateResultWithFillDelta(ctx, order.ID, VenueResult{
			VenueOrderID: order.VenueOrderID,
			Status:       order.Status, FilledQuantity: total.quantity.String(),
			AveragePrice: average.String(),
		})
		if err != nil {
			return item, changed, err
		}
		changed = true
		if !filledChanged || updated.ArbitrageExecutionID == "" {
			continue
		}
		if _, exists := seen[updated.ArbitrageExecutionID]; exists {
			continue
		}
		seen[updated.ArbitrageExecutionID] = struct{}{}
		executionIDs = append(executionIDs, updated.ArbitrageExecutionID)
	}
	if !changed {
		return item, false, nil
	}
	refreshed := item
	for _, executionID := range executionIDs {
		updated, err := a.store.RecomputeArbitrageBasePositionsForExecution(ctx, executionID)
		if err != nil {
			return refreshed, true, err
		}
		refreshed = updated
	}
	return refreshed, true, nil
}

func dexArbitrageVenue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "hyperliquid", "aster", "lighter":
		return true
	default:
		return false
	}
}

func portfolioAuditMark(
	snapshot portfolio.Snapshot,
	leg ArbitrageLeg,
) decimal.Decimal {
	target := normalizedAuditSymbol(leg.ExchangeSymbol)
	for _, position := range snapshot.Positions {
		if position.Kind != "" && position.Kind != "cex" {
			continue
		}
		if !strings.EqualFold(position.Exchange, leg.Exchange) {
			continue
		}
		symbolMatches := normalizedAuditSymbol(position.WireSymbol) == target ||
			normalizedAuditSymbol(position.Symbol) == target
		if !symbolMatches && position.BaseAsset != "" {
			symbolMatches = strings.EqualFold(position.BaseAsset, leg.BaseAsset)
		}
		if symbolMatches {
			if mark := parsePositiveDecimal(position.MarkPrice); mark.IsPositive() {
				return mark
			}
		}
	}
	return decimal.Zero
}

func (a *ArbitragePositionAuditor) arbitragePositionAuditMarks(
	item ArbitrageCombination,
) (decimal.Decimal, decimal.Decimal) {
	mark := arbitragePositionAuditMark(item)
	if mark.IsPositive() || a.valuations == nil {
		return mark, mark
	}
	valuation, ok := a.valuations.ArbitrageValuation(item.ID)
	if !ok {
		return decimal.Zero, decimal.Zero
	}
	markA := parsePositiveDecimal(valuation.LegAMid)
	markB := parsePositiveDecimal(valuation.LegBMid)
	if !markA.IsPositive() {
		markA = markB
	}
	if !markB.IsPositive() {
		markB = markA
	}
	return markA, markB
}

func venueBasePosition(
	snapshot portfolio.Snapshot,
	leg ArbitrageLeg,
	instrument Instrument,
) (decimal.Decimal, error) {
	if leg.ContractType == "spot" {
		for asset, value := range snapshot.SpotBalances {
			if strings.EqualFold(asset, leg.BaseAsset) {
				position, err := decimal.NewFromString(value)
				if err != nil {
					return decimal.Zero, fmt.Errorf("parse spot account position: %w", err)
				}
				return position, nil
			}
		}
		return decimal.Zero, nil
	}
	total := decimal.Zero
	target := normalizedAuditSymbol(leg.ExchangeSymbol)
	for _, position := range snapshot.Positions {
		if position.Kind != "" && position.Kind != "cex" {
			continue
		}
		if !strings.EqualFold(position.Exchange, leg.Exchange) {
			continue
		}
		symbolMatches := normalizedAuditSymbol(position.WireSymbol) == target ||
			normalizedAuditSymbol(position.Symbol) == target
		if !symbolMatches && position.BaseAsset != "" {
			symbolMatches = strings.EqualFold(position.BaseAsset, leg.BaseAsset)
		}
		if !symbolMatches {
			continue
		}
		signed, err := decimal.NewFromString(position.SignedContractSize)
		if err != nil {
			return decimal.Zero, fmt.Errorf("parse account contract position: %w", err)
		}
		converted, err := exchange.FromVenueQuantity(
			toVenueInstrument(instrument), signed.Abs().String(),
		)
		if err != nil {
			return decimal.Zero, err
		}
		base, _ := decimal.NewFromString(converted)
		if signed.IsNegative() {
			base = base.Neg()
		}
		total = total.Add(base)
	}
	return total, nil
}

func positionDifferenceMaterial(
	difference decimal.Decimal,
	price decimal.Decimal,
	instrument Instrument,
) bool {
	quantity, executable, err := executableHedgeQuantity(
		difference.Abs(), price, instrument, false,
	)
	if err != nil {
		// Missing or unknown constraints must not release an uncertain
		// position. The quantity has already passed the step threshold.
		return quantity.IsPositive()
	}
	return executable
}

func arbitragePositionAuditMark(item ArbitrageCombination) decimal.Decimal {
	positionNotional := parseDecimal(item.PositionNotional).Abs()
	pairedBase := decimal.Min(
		parseDecimal(item.LegABasePosition).Abs(),
		parseDecimal(item.LegBBasePosition).Abs(),
	)
	if !positionNotional.IsPositive() || !pairedBase.IsPositive() {
		return decimal.Zero
	}
	return positionNotional.Div(pairedBase)
}

func normalizedAuditSymbol(value string) string {
	replacer := strings.NewReplacer("-", "", "_", "", "/", "", ":", "")
	return strings.ToUpper(replacer.Replace(strings.TrimSpace(value)))
}
