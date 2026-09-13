package trader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"golang.org/x/sync/errgroup"
	"selfquant/backend/internal/account/portfolio"
	"selfquant/backend/internal/trader/exchange"
)

func (s *Service) CreateArbitrageCombination(
	ctx context.Context,
	input CreateArbitrageInput,
) (ArbitrageCombination, error) {
	if s.arbitrage == nil {
		return ArbitrageCombination{}, ErrNotFound
	}
	normalized, err := normalizeArbitrageInput(input)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	owner, err := s.credentials.Owner(ctx, normalized.Token)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	accountA, err := s.credentials.Get(ctx, normalized.Token, normalized.LegATradingAccountID)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	accountB, err := s.credentials.Get(ctx, normalized.Token, normalized.LegBTradingAccountID)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	if !exchangeEnabled(s.arbitrageExchanges, accountA.Exchange) ||
		!exchangeEnabled(s.arbitrageExchanges, accountB.Exchange) {
		return ArbitrageCombination{}, ErrUnsupportedExchange
	}
	accountA, err = s.ensureArbitrageAccountReady(ctx, normalized.Token, accountA)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	if accountB.TradingAccountID == accountA.TradingAccountID {
		accountB = accountA
	} else {
		accountB, err = s.ensureArbitrageAccountReady(ctx, normalized.Token, accountB)
		if err != nil {
			return ArbitrageCombination{}, err
		}
	}
	if accountA.ProductName != accountB.ProductName {
		return ArbitrageCombination{}, ErrInvalidArgument
	}
	instrumentA, err := s.catalog.Get(ctx, normalized.LegAInstrumentID)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	instrumentB, err := s.catalog.Get(ctx, normalized.LegBInstrumentID)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	if !validArbitrageInstrumentPair(accountA, instrumentA, accountB, instrumentB) {
		return ArbitrageCombination{}, ErrInvalidArgument
	}
	orderTypeForHedge := func(instrument Instrument) string {
		if instrument.ContractType == "spot" {
			return "limit"
		}
		return "market"
	}
	requiredA, requiredB := orderTypeForHedge(instrumentA), orderTypeForHedge(instrumentB)
	if normalized.ExecutionMode == "maker_then_hedge" {
		if normalized.MakerLeg == "a" {
			requiredA = "limit"
		} else {
			requiredB = "limit"
		}
	}
	if !instrumentRulesReady(instrumentA, requiredA) ||
		!instrumentRulesReady(instrumentB, requiredB) {
		return ArbitrageCombination{}, ErrInstrumentUnavailable
	}
	adapterA, ok := s.venues.Adapter(accountA.Exchange)
	if !ok {
		return ArbitrageCombination{}, ErrUnsupportedExchange
	}
	adapterB, ok := s.venues.Adapter(accountB.Exchange)
	if !ok {
		return ArbitrageCombination{}, ErrUnsupportedExchange
	}
	for _, item := range []struct {
		adapter exchange.Adapter
		account Credentials
	}{
		{adapterA, accountA},
		{adapterB, accountB},
	} {
		capabilities, capabilityErr := venueCapabilities(ctx, item.adapter, item.account)
		if capabilityErr != nil {
			return ArbitrageCombination{}, capabilityErr
		}
		if !capabilities.Arbitrage {
			return ArbitrageCombination{}, ErrUnsupportedExchange
		}
	}
	checkedAccounts := make(map[int64]bool, 2)
	for _, leg := range []struct {
		account    Credentials
		instrument Instrument
		adapter    exchange.Adapter
	}{
		{accountA, instrumentA, adapterA},
		{accountB, instrumentB, adapterB},
	} {
		if leg.instrument.ContractType == "spot" ||
			checkedAccounts[leg.account.TradingAccountID] {
			continue
		}
		reader, supportsModeCheck := leg.adapter.(exchange.PositionModeReader)
		if !supportsModeCheck {
			return ArbitrageCombination{}, fmt.Errorf(
				"%w: %s account mode cannot be verified",
				ErrPositionMode, leg.account.Exchange,
			)
		}
		modeCtx, cancel := context.WithTimeout(ctx, s.timeout)
		mode, modeErr := reader.GetPositionMode(
			modeCtx,
			toVenueCredentials(leg.account),
			toVenueInstrument(leg.instrument),
		)
		cancel()
		if modeErr != nil {
			return ArbitrageCombination{}, fmt.Errorf(
				"%w: verify %s account mode: %v",
				ErrPositionMode, leg.account.Exchange, modeErr,
			)
		}
		if mode != exchange.PositionModeOneWay {
			return ArbitrageCombination{}, fmt.Errorf(
				"%w: %s account is in hedge mode",
				ErrPositionMode, leg.account.Exchange,
			)
		}
		checkedAccounts[leg.account.TradingAccountID] = true
	}
	levA, err := defaultLegLeverage(instrumentA.ContractType, normalized.LegALeverage)
	if err != nil {
		return ArbitrageCombination{}, tagCreateErrorLeg(err, "a")
	}
	levB, err := defaultLegLeverage(instrumentB.ContractType, normalized.LegBLeverage)
	if err != nil {
		return ArbitrageCombination{}, tagCreateErrorLeg(err, "b")
	}
	normalized.LegALeverage = levA.String()
	normalized.LegBLeverage = levB.String()
	legA := arbitrageLeg(accountA, instrumentA)
	legB := arbitrageLeg(accountB, instrumentB)
	probe := ArbitrageCombination{
		OwnerUsername: owner, AskThresholdBps: normalized.AskThresholdBps,
		BidThresholdBps: normalized.BidThresholdBps,
		TargetNotional:  normalized.TargetNotional,
		ExecutionMode:   normalized.ExecutionMode, MakerLeg: normalized.MakerLeg,
		RunMode: normalized.RunMode, EntryDirection: normalized.EntryDirection,
		LegALeverage: normalized.LegALeverage, LegBLeverage: normalized.LegBLeverage,
		ExitPolicy: normalized.ExitPolicy, ExitAnnualizedRate: normalized.ExitAnnualizedRate,
		ExitAfterSeconds:                  normalized.ExitAfterSeconds,
		EarlyExitFunding8hAnnualizedFloor: normalized.EarlyExitFunding8hAnnualizedFloor,
		LegA:                              legA, LegB: legB,
	}
	probe.RequestFingerprint = arbitrageFingerprint(probe)
	if lookup, ok := s.arbitrage.(arbitrageIdempotencyStore); ok {
		existing, lookupErr := lookup.GetArbitrageCombinationByIdempotencyKey(
			ctx, owner, normalized.IdempotencyKey,
		)
		if lookupErr == nil {
			if existing.RequestFingerprint == probe.RequestFingerprint ||
				sameArbitrageRequest(existing, probe) {
				return existing, nil
			}
			return ArbitrageCombination{}, ErrIdempotencyConflict
		}
		if !errors.Is(lookupErr, ErrNotFound) {
			return ArbitrageCombination{}, lookupErr
		}
	}
	unlockAccounts := s.lockArbitrageAccounts(
		accountA.TradingAccountID,
		accountB.TradingAccountID,
	)
	defer unlockAccounts()
	if checker, ok := s.arbitrage.(arbitrageConflictStore); ok {
		if conflictErr := checker.FindActiveArbitrageInstrumentConflict(
			ctx, owner, normalized.IdempotencyKey,
			accountA.TradingAccountID, instrumentA.ID,
			accountB.TradingAccountID, instrumentB.ID,
		); conflictErr != nil {
			return ArbitrageCombination{}, conflictErr
		}
	}
	if s.portfolios == nil {
		return ArbitrageCombination{}, fmt.Errorf(
			"%w: account position snapshots are unavailable",
			ErrVenueUnavailable,
		)
	}
	loadSnapshot := func(loadCtx context.Context, account Credentials) (portfolio.Snapshot, error) {
		queryCtx, cancel := context.WithTimeout(loadCtx, s.timeout)
		defer cancel()
		snapshot, snapshotErr := s.portfolios.Snapshot(
			queryCtx,
			account.Exchange,
			portfolio.Credentials{
				APIKey: account.APIKey, APISecret: account.APISecret,
				Passphrase:   account.Passphrase,
				AccountIndex: account.AccountIndex,
			},
		)
		if snapshotErr != nil {
			return portfolio.Snapshot{}, fmt.Errorf(
				"%w: capture %s account baseline: %v",
				ErrVenueUnavailable, account.Exchange, snapshotErr,
			)
		}
		return snapshot, nil
	}
	var snapshotA, snapshotB portfolio.Snapshot
	if accountA.TradingAccountID == accountB.TradingAccountID {
		snapshotA, err = loadSnapshot(ctx, accountA)
		snapshotB = snapshotA
	} else {
		group, groupCtx := errgroup.WithContext(ctx)
		group.Go(func() error {
			var loadErr error
			snapshotA, loadErr = loadSnapshot(groupCtx, accountA)
			return loadErr
		})
		group.Go(func() error {
			var loadErr error
			snapshotB, loadErr = loadSnapshot(groupCtx, accountB)
			return loadErr
		})
		err = group.Wait()
	}
	if err != nil {
		return ArbitrageCombination{}, err
	}
	target := parsePositiveDecimal(normalized.TargetNotional)
	checkCtx, cancelChecks := context.WithTimeout(ctx, s.timeout)
	defer cancelChecks()
	if err := readOnlyCreateLegCheck(
		accountA, instrumentA, "a", levA, target, snapshotA,
	); err != nil {
		return ArbitrageCombination{}, err
	}
	if err := readOnlyCreateLegCheck(
		accountB, instrumentB, "b", levB, target, snapshotB,
	); err != nil {
		return ArbitrageCombination{}, err
	}
	applied := []string{}
	applied, err = applyCreateLeg(
		checkCtx, adapterA, accountA, instrumentA, "a", levA, target, snapshotA, applied,
	)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	applied, err = applyCreateLeg(
		checkCtx, adapterB, accountB, instrumentB, "b", levB, target, snapshotB, applied,
	)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	baselineA, err := venueBasePosition(snapshotA, legA, instrumentA)
	if err != nil {
		return ArbitrageCombination{}, fmt.Errorf(
			"%w: capture %s baseline: %v",
			ErrVenueUnavailable, legA.ExchangeSymbol, err,
		)
	}
	baselineB, err := venueBasePosition(snapshotB, legB, instrumentB)
	if err != nil {
		return ArbitrageCombination{}, fmt.Errorf(
			"%w: capture %s baseline: %v",
			ErrVenueUnavailable, legB.ExchangeSymbol, err,
		)
	}
	item := ArbitrageCombination{
		ID: uuid.NewString(), IdempotencyKey: normalized.IdempotencyKey,
		OwnerUsername: owner, AskThresholdBps: normalized.AskThresholdBps,
		BidThresholdBps: normalized.BidThresholdBps,
		TargetNotional:  normalized.TargetNotional,
		ExecutionMode:   normalized.ExecutionMode,
		MakerLeg:        normalized.MakerLeg, Status: "running",
		RunMode:                           normalized.RunMode,
		EntryDirection:                    normalized.EntryDirection,
		LegALeverage:                      normalized.LegALeverage,
		LegBLeverage:                      normalized.LegBLeverage,
		ExitPolicy:                        normalized.ExitPolicy,
		ExitAnnualizedRate:                normalized.ExitAnnualizedRate,
		ExitAfterSeconds:                  normalized.ExitAfterSeconds,
		EarlyExitFunding8hAnnualizedFloor: normalized.EarlyExitFunding8hAnnualizedFloor,
		OneShotPhase:                      "",
		PositionNotional:                  "0", CumulativeTurnoverNotional: "0",
		MarketDataStale:               true,
		LegA:                          legA,
		LegB:                          legB,
		LegAVenueBaselineBasePosition: baselineA.String(),
		LegBVenueBaselineBasePosition: baselineB.String(),
		VenueBaselineCapturedAt:       time.Now().UTC(),
	}
	if item.RunMode == "one_shot" {
		item.OneShotPhase = "building_target"
	}
	item.RequestFingerprint = arbitrageFingerprint(item)
	created, inserted, err := s.arbitrage.CreateArbitrageCombination(ctx, item)
	if err != nil {
		if errors.Is(err, ErrActiveArbitrageInstrumentConflict) {
			return ArbitrageCombination{}, err
		}
		s.logger.Error(
			"arbitrage create insert failed after leverage applied",
			"appliedLegs", appliedLegsJSON(applied),
			"error", err,
		)
		return ArbitrageCombination{}, fmt.Errorf(
			"%w: create arbitrage combination: appliedLegs=%s: %v",
			ErrPersistence, appliedLegsJSON(applied), err,
		)
	}
	if !inserted && created.RequestFingerprint != item.RequestFingerprint &&
		!sameArbitrageRequest(created, item) {
		return ArbitrageCombination{}, ErrIdempotencyConflict
	}
	return created, nil
}

func (s *Service) ensureArbitrageAccountReady(
	ctx context.Context,
	token string,
	account Credentials,
) (Credentials, error) {
	switch strings.ToLower(strings.TrimSpace(account.Exchange)) {
	case "hyperliquid", "aster", "lighter":
	default:
		return account, nil
	}
	provider, ok := s.credentials.(tradingReadinessProvider)
	if !ok {
		return Credentials{}, fmt.Errorf(
			"%w: %s trading readiness is unavailable",
			ErrVenueUnavailable, account.Exchange,
		)
	}
	readinessCtx, cancel := context.WithTimeout(ctx, s.timeout)
	readiness, err := provider.InspectTradingReadiness(
		readinessCtx, token, account.TradingAccountID,
	)
	cancel()
	if err != nil {
		return Credentials{}, fmt.Errorf(
			"%w: inspect %s trading readiness: %v",
			ErrVenueUnavailable, account.Exchange, err,
		)
	}
	if !readiness.Ready {
		detail := strings.TrimSpace(readiness.UnavailableReason)
		if detail == "" {
			detail = strings.TrimSpace(readiness.UnavailableCode)
		}
		if detail == "" {
			detail = strings.TrimSpace(readiness.Status)
		}
		return Credentials{}, fmt.Errorf(
			"%w: %s account is not trading-ready: %s",
			ErrVenueUnavailable, account.Exchange, detail,
		)
	}
	// Lighter readiness may discover and persist account/API-key indexes.
	// Re-fetch credentials so execution never carries the stale preflight copy.
	if strings.EqualFold(account.Exchange, "lighter") {
		refreshed, refreshErr := s.credentials.Get(ctx, token, account.TradingAccountID)
		if refreshErr != nil {
			return Credentials{}, fmt.Errorf(
				"%w: refresh lighter credentials after readiness: %v",
				ErrVenueUnavailable, refreshErr,
			)
		}
		account = refreshed
	}
	return account, nil
}

func (s *Service) lockArbitrageAccounts(accountA, accountB int64) func() {
	if accountA == accountB {
		lock := s.accountLock(accountA)
		lock.Lock()
		return lock.Unlock
	}
	first, second := accountA, accountB
	if first > second {
		first, second = second, first
	}
	firstLock := s.accountLock(first)
	secondLock := s.accountLock(second)
	firstLock.Lock()
	secondLock.Lock()
	return func() {
		secondLock.Unlock()
		firstLock.Unlock()
	}
}

func (s *Service) GetArbitrageCombination(
	ctx context.Context,
	token, id string,
) (ArbitrageCombination, []Order, []ArbitrageExecution, []ArbitrageEvent, error) {
	if s.arbitrage == nil || strings.TrimSpace(token) == "" || !validUUID(id) {
		return ArbitrageCombination{}, nil, nil, nil, ErrInvalidArgument
	}
	owner, err := s.credentials.Owner(ctx, token)
	if err != nil {
		return ArbitrageCombination{}, nil, nil, nil, err
	}
	item, err := s.arbitrage.GetArbitrageCombinationByOwner(ctx, owner, id)
	if err != nil {
		return ArbitrageCombination{}, nil, nil, nil, err
	}
	item = s.withArbitrageValuation(item)
	orders, err := s.arbitrage.ListRecentSubmittedArbitrageOrders(ctx, owner, id, 10)
	if err != nil {
		return ArbitrageCombination{}, nil, nil, nil, err
	}
	executions, err := s.arbitrage.ListArbitrageExecutions(ctx, id, 20)
	if err != nil {
		return ArbitrageCombination{}, nil, nil, nil, err
	}
	events, err := s.arbitrage.ListArbitrageEvents(ctx, id, 50)
	if err != nil {
		return ArbitrageCombination{}, nil, nil, nil, err
	}
	return item, orders, executions, events, nil
}

func (s *Service) ListArbitrageCombinations(
	ctx context.Context,
	token, view string,
	limit int,
	cursor string,
) ([]ArbitrageCombination, string, int64, error) {
	if s.arbitrage == nil || strings.TrimSpace(token) == "" {
		return nil, "", 0, ErrInvalidArgument
	}
	view = strings.ToLower(strings.TrimSpace(view))
	if view == "" {
		view = "running"
	}
	if view != "running" && view != "closed" {
		return nil, "", 0, ErrInvalidArgument
	}
	if cursor != "" && !validUUID(cursor) {
		return nil, "", 0, ErrInvalidArgument
	}
	owner, err := s.credentials.Owner(ctx, token)
	if err != nil {
		return nil, "", 0, err
	}
	items, next, err := s.arbitrage.ListArbitrageCombinations(ctx, owner, view, limit, cursor)
	if err != nil {
		return nil, "", 0, err
	}
	for index := range items {
		items[index] = s.withArbitrageValuation(items[index])
	}
	total, err := s.arbitrage.CountArbitrageCombinations(ctx, owner, view)
	if err != nil {
		return nil, "", 0, err
	}
	return items, next, total, nil
}

func (s *Service) withArbitrageValuation(
	item ArbitrageCombination,
) ArbitrageCombination {
	if s.arbitrageValuations == nil {
		return item
	}
	valuation, ok := s.arbitrageValuations.ArbitrageValuation(item.ID)
	if !ok {
		return item
	}
	legAPrice := parsePositiveDecimal(valuation.LegAMid)
	legBPrice := parsePositiveDecimal(valuation.LegBMid)
	if legAPrice.IsPositive() && !valuation.LegAUpdatedAt.IsZero() {
		item.LegAVenueValuationPrice = legAPrice.String()
		item.LegAVenueNotional = parseDecimal(
			item.LegAVenueBasePosition,
		).Mul(legAPrice).String()
		item.LegAVenueValuationAt = valuation.LegAUpdatedAt
	}
	if legBPrice.IsPositive() && !valuation.LegBUpdatedAt.IsZero() {
		item.LegBVenueValuationPrice = legBPrice.String()
		item.LegBVenueNotional = parseDecimal(
			item.LegBVenueBasePosition,
		).Mul(legBPrice).String()
		item.LegBVenueValuationAt = valuation.LegBUpdatedAt
	}
	return item
}

func (s *Service) UpdateArbitrageCombination(
	ctx context.Context,
	input UpdateArbitrageInput,
) (ArbitrageCombination, error) {
	if s.arbitrage == nil {
		return ArbitrageCombination{}, ErrNotFound
	}
	normalized, err := normalizeUpdateArbitrageInput(input)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	owner, err := s.credentials.Owner(ctx, normalized.Token)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	store, ok := s.arbitrage.(arbitrageConfigStore)
	if !ok {
		return ArbitrageCombination{}, ErrNotFound
	}
	return store.UpdateArbitrageCombinationConfig(
		ctx, owner, normalized.CombinationID, normalized,
	)
}

func (s *Service) CloseArbitrageCombination(
	ctx context.Context,
	token, id string,
) (ArbitrageCombination, error) {
	if s.arbitrage == nil || strings.TrimSpace(token) == "" || !validUUID(id) {
		return ArbitrageCombination{}, ErrInvalidArgument
	}
	owner, err := s.credentials.Owner(ctx, token)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	item, err := s.arbitrage.GetArbitrageCombinationByOwner(ctx, owner, id)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	if item.Status == "closed" {
		return item, nil
	}
	if item.Status != "running" && item.Status != "failed" && item.Status != "closing" {
		return ArbitrageCombination{}, ErrArbitrageNotClosable
	}
	if item.Status == "closing" {
		return item, nil
	}
	return s.arbitrage.MarkArbitrageClosing(ctx, owner, id)
}

func normalizeUpdateArbitrageInput(
	input UpdateArbitrageInput,
) (UpdateArbitrageInput, error) {
	input.Token = strings.TrimSpace(input.Token)
	input.CombinationID = strings.TrimSpace(input.CombinationID)
	if input.Token == "" || !validUUID(input.CombinationID) {
		return UpdateArbitrageInput{}, ErrInvalidArgument
	}
	if input.AskThresholdBps == nil && input.BidThresholdBps == nil &&
		input.TargetNotional == nil {
		return UpdateArbitrageInput{}, fmt.Errorf(
			"%w: at least one arbitrage parameter is required",
			ErrInvalidArgument,
		)
	}
	normalize := func(value **string, positive bool) error {
		if *value == nil {
			return nil
		}
		raw := strings.TrimSpace(**value)
		parsed, err := decimal.NewFromString(raw)
		if err != nil {
			return fmt.Errorf("%w: invalid decimal value %q", ErrInvalidArgument, raw)
		}
		if positive && !parsed.IsPositive() {
			return fmt.Errorf(
				"%w: notional value must be positive",
				ErrArbitrageConfigInvalid,
			)
		}
		canonical := parsed.String()
		*value = &canonical
		return nil
	}
	for _, item := range []struct {
		value    **string
		positive bool
	}{
		{&input.AskThresholdBps, false},
		{&input.BidThresholdBps, false},
		{&input.TargetNotional, true},
	} {
		if err := normalize(item.value, item.positive); err != nil {
			return UpdateArbitrageInput{}, err
		}
	}
	return input, nil
}

func normalizeArbitrageInput(input CreateArbitrageInput) (CreateArbitrageInput, error) {
	input.Token = strings.TrimSpace(input.Token)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	input.ExecutionMode = strings.ToLower(strings.TrimSpace(input.ExecutionMode))
	input.MakerLeg = strings.ToLower(strings.TrimSpace(input.MakerLeg))
	if input.Token == "" || len(input.IdempotencyKey) < 8 ||
		input.LegATradingAccountID <= 0 || input.LegBTradingAccountID <= 0 ||
		input.LegAInstrumentID <= 0 || input.LegBInstrumentID <= 0 {
		return CreateArbitrageInput{}, fmt.Errorf(
			"%w: token, idempotency key, accounts and instruments are required",
			ErrInvalidArgument,
		)
	}
	if input.ExecutionMode != "maker_then_hedge" && input.ExecutionMode != "simultaneous_market" {
		return CreateArbitrageInput{}, fmt.Errorf(
			"%w: unsupported arbitrage execution mode %q",
			ErrInvalidArgument, input.ExecutionMode,
		)
	}
	if input.MakerLeg == "" {
		input.MakerLeg = "a"
	}
	if input.MakerLeg != "a" && input.MakerLeg != "b" {
		return CreateArbitrageInput{}, fmt.Errorf(
			"%w: preferred leg must be a or b, got %q",
			ErrInvalidArgument, input.MakerLeg,
		)
	}
	target, targetErr := decimal.NewFromString(strings.TrimSpace(input.TargetNotional))
	if targetErr != nil || !target.IsPositive() {
		return CreateArbitrageInput{}, fmt.Errorf(
			"%w: invalid notional budget %q",
			ErrInvalidArgument, input.TargetNotional,
		)
	}
	input.TargetNotional = target.String()
	input.RunMode = strings.ToLower(strings.TrimSpace(input.RunMode))
	if input.RunMode == "" {
		input.RunMode = "spread"
	}
	if input.RunMode != "spread" && input.RunMode != "one_shot" {
		return CreateArbitrageInput{}, fmt.Errorf(
			"%w: unsupported run mode %q", ErrInvalidArgument, input.RunMode,
		)
	}
	if input.RunMode == "one_shot" {
		input.AskThresholdBps = "0"
		input.BidThresholdBps = "0"
	} else {
		ask, askErr := decimal.NewFromString(strings.TrimSpace(input.AskThresholdBps))
		bid, bidErr := decimal.NewFromString(strings.TrimSpace(input.BidThresholdBps))
		if askErr != nil || bidErr != nil {
			return CreateArbitrageInput{}, fmt.Errorf(
				"%w: invalid thresholds (ask=%q bid=%q)",
				ErrInvalidArgument, input.AskThresholdBps, input.BidThresholdBps,
			)
		}
		input.AskThresholdBps = ask.String()
		input.BidThresholdBps = bid.String()
	}
	input.EntryDirection = strings.ToLower(strings.TrimSpace(input.EntryDirection))
	input.ExitPolicy = strings.ToLower(strings.TrimSpace(input.ExitPolicy))
	if input.RunMode == "one_shot" {
		if input.EntryDirection == "" {
			input.EntryDirection = "ask"
		}
		if input.EntryDirection != "ask" {
			return CreateArbitrageInput{}, fmt.Errorf(
				"%w: one_shot entry direction must be ask",
				ErrInvalidArgument,
			)
		}
		switch input.ExitPolicy {
		case "annualized":
			rate, err := decimal.NewFromString(strings.TrimSpace(input.ExitAnnualizedRate))
			if err != nil || !rate.IsPositive() {
				return CreateArbitrageInput{}, fmt.Errorf(
					"%w: one_shot annualized exit rate is required",
					ErrInvalidArgument,
				)
			}
			input.ExitAnnualizedRate = rate.String()
			input.ExitAfterSeconds = 0
		case "time":
			switch input.ExitAfterSeconds {
			case 3600, 14400, 28800, 86400, 604800:
			default:
				return CreateArbitrageInput{}, fmt.Errorf(
					"%w: one_shot hold time must be 3600, 14400, 28800, 86400 or 604800 seconds",
					ErrInvalidArgument,
				)
			}
			input.ExitAnnualizedRate = ""
		default:
			return CreateArbitrageInput{}, fmt.Errorf(
				"%w: one_shot exit policy must be annualized or time",
				ErrInvalidArgument,
			)
		}
		floor, err := normalizeEarlyExitFunding8hFloor(input.EarlyExitFunding8hAnnualizedFloor)
		if err != nil {
			return CreateArbitrageInput{}, err
		}
		input.EarlyExitFunding8hAnnualizedFloor = floor
	} else {
		input.EntryDirection = ""
		input.ExitPolicy = ""
		input.ExitAnnualizedRate = ""
		input.ExitAfterSeconds = 0
		input.EarlyExitFunding8hAnnualizedFloor = ""
	}
	return input, nil
}

func normalizeEarlyExitFunding8hFloor(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	switch strings.ToLower(trimmed) {
	case "nan", "+nan", "-nan", "inf", "+inf", "-inf", "infinity", "+infinity", "-infinity":
		return "", fmt.Errorf("%w: invalid 8h funding early-exit floor", ErrInvalidArgument)
	}
	value, err := decimal.NewFromString(trimmed)
	if err != nil {
		return "", fmt.Errorf("%w: invalid 8h funding early-exit floor", ErrInvalidArgument)
	}
	printed := strings.ToLower(value.String())
	if printed == "nan" || strings.Contains(printed, "inf") {
		return "", fmt.Errorf("%w: invalid 8h funding early-exit floor", ErrInvalidArgument)
	}
	return value.String(), nil
}

func validArbitrageInstrumentPair(
	accountA Credentials,
	instrumentA Instrument,
	accountB Credentials,
	instrumentB Instrument,
) bool {
	if instrumentA.Exchange != accountA.Exchange ||
		instrumentB.Exchange != accountB.Exchange ||
		instrumentA.ID == instrumentB.ID ||
		instrumentA.BaseAsset != instrumentB.BaseAsset {
		return false
	}
	stablecoinPerpetualPair :=
		instrumentA.ContractType == "perpetual" &&
			instrumentB.ContractType == "perpetual" &&
			((strings.EqualFold(instrumentA.QuoteAsset, "USDC") &&
				strings.EqualFold(instrumentB.QuoteAsset, "USDT")) ||
				(strings.EqualFold(instrumentA.QuoteAsset, "USDT") &&
					strings.EqualFold(instrumentB.QuoteAsset, "USDC")))
	if !strings.EqualFold(instrumentA.QuoteAsset, instrumentB.QuoteAsset) &&
		!stablecoinPerpetualPair {
		return false
	}
	if accountA.Exchange != accountB.Exchange {
		return true
	}
	return (instrumentA.ContractType == "spot" && instrumentB.ContractType == "perpetual") ||
		(instrumentA.ContractType == "perpetual" && instrumentB.ContractType == "spot")
}

func arbitrageLeg(account Credentials, instrument Instrument) ArbitrageLeg {
	return ArbitrageLeg{
		TradingAccountID: account.TradingAccountID, InstrumentID: instrument.ID,
		ProductName: account.ProductName, AccountName: account.AccountName,
		Exchange: account.Exchange, ContractType: instrument.ContractType,
		ExchangeSymbol: instrument.ExchangeSymbol, BaseAsset: instrument.BaseAsset,
		QuoteAsset: instrument.QuoteAsset,
	}
}

func arbitrageFingerprint(item ArbitrageCombination) string {
	askThreshold, bidThreshold := item.AskThresholdBps, item.BidThresholdBps
	if strings.EqualFold(strings.TrimSpace(item.RunMode), "one_shot") {
		askThreshold, bidThreshold = "0", "0"
	}
	values := []string{
		strconv.FormatInt(item.LegA.TradingAccountID, 10),
		strconv.FormatInt(item.LegA.InstrumentID, 10),
		strconv.FormatInt(item.LegB.TradingAccountID, 10),
		strconv.FormatInt(item.LegB.InstrumentID, 10),
		askThreshold, bidThreshold, item.TargetNotional,
		item.ExecutionMode, item.MakerLeg,
		item.RunMode, item.EntryDirection, item.LegALeverage, item.LegBLeverage,
		item.ExitPolicy, item.ExitAnnualizedRate, strconv.Itoa(item.ExitAfterSeconds),
		item.EarlyExitFunding8hAnnualizedFloor,
	}
	sum := sha256.Sum256([]byte(strings.Join(values, "|")))
	return hex.EncodeToString(sum[:])
}

func sameArbitrageRequest(left, right ArbitrageCombination) bool {
	if left.OwnerUsername != right.OwnerUsername ||
		left.LegA.TradingAccountID != right.LegA.TradingAccountID ||
		left.LegA.InstrumentID != right.LegA.InstrumentID ||
		left.LegB.TradingAccountID != right.LegB.TradingAccountID ||
		left.LegB.InstrumentID != right.LegB.InstrumentID ||
		left.TargetNotional != right.TargetNotional ||
		left.ExecutionMode != right.ExecutionMode ||
		left.MakerLeg != right.MakerLeg ||
		left.RunMode != right.RunMode ||
		left.EntryDirection != right.EntryDirection ||
		left.LegALeverage != right.LegALeverage ||
		left.LegBLeverage != right.LegBLeverage ||
		left.ExitPolicy != right.ExitPolicy ||
		left.ExitAnnualizedRate != right.ExitAnnualizedRate ||
		left.ExitAfterSeconds != right.ExitAfterSeconds ||
		left.EarlyExitFunding8hAnnualizedFloor != right.EarlyExitFunding8hAnnualizedFloor {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(left.RunMode), "one_shot") &&
		strings.EqualFold(strings.TrimSpace(right.RunMode), "one_shot") {
		return true
	}
	return left.AskThresholdBps == right.AskThresholdBps &&
		left.BidThresholdBps == right.BidThresholdBps
}
