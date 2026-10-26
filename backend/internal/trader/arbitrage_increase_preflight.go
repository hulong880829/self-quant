package trader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
)

func (s *Service) PreflightIncreaseTarget(
	ctx context.Context,
	token, combinationID, newTargetRaw string,
) (IncreaseTargetPreflight, error) {
	now := time.Now().UTC()
	result := IncreaseTargetPreflight{
		CheckedAt: now, ExpiresAt: now.Add(3 * time.Second),
		TargetNotionalUnit: preflightUSDEquivalent,
	}
	if s.arbitrage == nil || strings.TrimSpace(token) == "" || !validUUID(combinationID) {
		return result, ErrInvalidArgument
	}
	newTarget, err := decimal.NewFromString(strings.TrimSpace(newTargetRaw))
	if err != nil || !newTarget.IsPositive() {
		result.Code, result.Message = "invalid_target", "new target notional must be positive"
		return result, nil
	}
	owner, err := s.credentials.Owner(ctx, token)
	if err != nil {
		return result, err
	}
	combo, err := s.arbitrage.GetArbitrageCombinationByOwner(ctx, owner, combinationID)
	if err != nil {
		return result, err
	}
	oldTarget, err := decimal.NewFromString(strings.TrimSpace(combo.TargetNotional))
	if err != nil || !oldTarget.IsPositive() {
		result.Code, result.Message = "invalid_target", "current target notional is unavailable"
		return result, nil
	}
	result.CurrentTargetNotional = oldTarget.String()
	result.NewTargetNotional = newTarget.String()
	result.RequestedTargetDelta = newTarget.Sub(oldTarget).String()
	liveWork := false
	if strings.EqualFold(strings.TrimSpace(combo.RunMode), "one_shot") {
		reader, ok := s.arbitrage.(interface {
			HasUnfinishedArbitrageWork(context.Context, string) (bool, error)
		})
		if !ok {
			result.Code, result.Message = "preflight_unavailable", "one-shot work state is unavailable"
			return result, nil
		}
		liveWork, err = reader.HasUnfinishedArbitrageWork(ctx, combo.ID)
		if err != nil {
			return result, err
		}
	}
	if err := increaseTargetGate(combo, newTarget, liveWork); err != nil {
		return increasePreflightFailure(result, err), nil
	}
	accountA, err := s.credentials.Get(ctx, token, combo.LegA.TradingAccountID)
	if err != nil {
		return result, err
	}
	accountB, err := s.credentials.Get(ctx, token, combo.LegB.TradingAccountID)
	if err != nil {
		return result, err
	}
	instrumentA, err := s.catalog.Get(ctx, combo.LegA.InstrumentID)
	if err != nil {
		return result, err
	}
	instrumentB, err := s.catalog.Get(ctx, combo.LegB.InstrumentID)
	if err != nil {
		return result, err
	}
	stampResolvedIncreaseLeg(&result.LegA, instrumentA)
	stampResolvedIncreaseLeg(&result.LegB, instrumentB)
	levA, err := defaultLegLeverage(instrumentA.ContractType, combo.LegALeverage)
	if err != nil {
		return increasePreflightFailure(result, tagCreateErrorLeg(err, "a")), nil
	}
	levB, err := defaultLegLeverage(instrumentB.ContractType, combo.LegBLeverage)
	if err != nil {
		return increasePreflightFailure(result, tagCreateErrorLeg(err, "b")), nil
	}
	additionalA := legAdditionalNotional(newTarget, actualLegNotional(
		combo.LegAVenueNotional, combo.LegABasePosition, combo.LegAVenueValuationPrice,
	))
	additionalB := legAdditionalNotional(newTarget, actualLegNotional(
		combo.LegBVenueNotional, combo.LegBBasePosition, combo.LegBVenueValuationPrice,
	))
	result.LegA.AdditionalNotional = additionalA.String()
	result.LegB.AdditionalNotional = additionalB.String()
	input := CreateArbitrageInput{
		ExecutionMode: combo.ExecutionMode, MakerLeg: combo.MakerLeg,
		LegALeverage: levA.String(), LegBLeverage: levB.String(),
	}
	requiredA, requiredB := preflightOrderTypes(input, instrumentA, instrumentB)
	if !instrumentRulesReady(instrumentA, requiredA) || !instrumentRulesReady(instrumentB, requiredB) {
		result.Code, result.Message = "instrument_unavailable", "instrument trading rules are unavailable"
		return result, nil
	}
	if !exchangeEnabled(s.arbitrageExchanges, accountA.Exchange) ||
		!exchangeEnabled(s.arbitrageExchanges, accountB.Exchange) {
		result.Code, result.Message = "unsupported_exchange", "exchange is outside the execution allowlist"
		return result, nil
	}
	adapterA, ok := s.venues.Adapter(accountA.Exchange)
	if !ok {
		result.Code, result.Message = "unsupported_exchange", "leg A exchange adapter is unavailable"
		result.Leg, result.Exchange = "a", accountA.Exchange
		return result, nil
	}
	adapterB, ok := s.venues.Adapter(accountB.Exchange)
	if !ok {
		result.Code, result.Message = "unsupported_exchange", "leg B exchange adapter is unavailable"
		result.Leg, result.Exchange = "b", accountB.Exchange
		return result, nil
	}
	for _, item := range []struct {
		leg     string
		account Credentials
		adapter exchange.Adapter
	}{{"a", accountA, adapterA}, {"b", accountB, adapterB}} {
		capabilities, capabilityErr := venueCapabilities(ctx, item.adapter, item.account)
		if capabilityErr != nil || !capabilities.Arbitrage {
			result.Code, result.Message = "unsupported_exchange", "exchange does not support arbitrage execution"
			result.Leg, result.Exchange = item.leg, item.account.Exchange
			return result, nil
		}
	}
	if err := preflightPositionModes(ctx, s.timeout, accountA, instrumentA, adapterA, accountB, instrumentB, adapterB); err != nil {
		return increasePreflightFailure(result, err), nil
	}
	snapshotA, snapshotB, err := s.preflightSnapshots(ctx, accountA, accountB)
	if err != nil {
		return increasePreflightFailure(result, err), nil
	}
	if err := readOnlyCreateLegCheck(accountA, instrumentA, "a", levA, additionalA, snapshotA); err != nil {
		return increasePreflightFailure(result, err), nil
	}
	if err := readOnlyCreateLegCheck(accountB, instrumentB, "b", levB, additionalB, snapshotB); err != nil {
		return increasePreflightFailure(result, err), nil
	}
	priceA, priceB := s.preflightPrices(
		ctx, accountA, instrumentA, adapterA, snapshotA,
		accountB, instrumentB, adapterB, snapshotB,
	)
	builtA := buildPreflightLeg(instrumentA, requiredA, levA, additionalA, snapshotA, priceA)
	builtB := buildPreflightLeg(instrumentB, requiredB, levB, additionalB, snapshotB, priceB)
	marginA, marginB, marginErr := s.preflightMargins(
		ctx, adapterA, accountA, instrumentA, "a", levA, additionalA, snapshotA,
		adapterB, accountB, instrumentB, "b", levB, additionalB, snapshotB,
	)
	if marginErr != nil {
		return increasePreflightFailure(result, marginErr), nil
	}
	result.LegA = increaseLegFromPreflight(additionalA, builtA, marginA)
	result.LegB = increaseLegFromPreflight(additionalB, builtB, marginB)
	for _, item := range []struct {
		leg      string
		out      IncreaseTargetLeg
		exchange string
	}{{"a", result.LegA, accountA.Exchange}, {"b", result.LegB, accountB.Exchange}} {
		if item.out.EstimatedFee.Status == "unknown" {
			result.Warnings = append(result.Warnings, preflightWarning(
				"fee_unknown", "an exact fee estimate is unavailable",
				item.leg, item.exchange, "fee_estimate_unavailable", "",
			))
		}
		if item.out.IncrementalMargin.Status == "unknown" {
			result.Warnings = append(result.Warnings, preflightWarning(
				"margin_unknown", "an exact post-leverage margin requirement is unavailable",
				item.leg, item.exchange, "margin_preview_unavailable", preflightUSDEquivalent,
			))
		}
	}
	result.Executable = true
	return result, nil
}

func increaseLegFromPreflight(
	additional decimal.Decimal, built PreflightLeg, margin PreflightDecimal,
) IncreaseTargetLeg {
	quantity := ""
	if built.RoundedBaseQuantity.Status == "known" {
		quantity = built.RoundedBaseQuantity.Value
	}
	return IncreaseTargetLeg{
		AdditionalNotional:      additional.String(),
		IncrementalQuantity:     quantity,
		IncrementalQuantityUnit: built.RoundedBaseQuantity.Unit,
		IncrementalMargin:       margin,
		EstimatedFee:            built.EstimatedFee,
		AvailableFunds:          built.AvailableFunds,
		MinQuantity:             built.MinQuantity,
		MinNotional:             built.MinNotional,
		QuantityStep:            built.QuantityStep,
	}
}

func stampResolvedIncreaseLeg(leg *IncreaseTargetLeg, instrument Instrument) {
	base := strings.TrimSpace(instrument.BaseAsset)
	quote := strings.TrimSpace(instrument.QuoteAsset)
	if leg.IncrementalQuantityUnit == "" {
		leg.IncrementalQuantityUnit = base
	}
	setPreflightUnit(&leg.IncrementalMargin, preflightUSDEquivalent)
	setPreflightUnit(&leg.AvailableFunds, preflightUSDEquivalent)
	setPreflightUnit(&leg.MinQuantity, base)
	setPreflightUnit(&leg.MinNotional, quote)
	setPreflightUnit(&leg.QuantityStep, base)
}

func increasePreflightFailure(result IncreaseTargetPreflight, err error) IncreaseTargetPreflight {
	var createErr *ArbitrageCreateError
	if errors.As(err, &createErr) {
		result.Code, result.Message, result.Leg = createErr.Code, createErr.Message, createErr.Leg
		result.Exchange = createErr.Details["exchange"]
		return result
	}
	if errors.Is(err, ErrArbitrageConflict) {
		result.Code, result.Message = "arbitrage_conflict", err.Error()
		return result
	}
	if errors.Is(err, ErrArbitrageConfigInvalid) {
		result.Code, result.Message = "invalid_target", err.Error()
		return result
	}
	result.Code, result.Message = "preflight_unavailable", err.Error()
	return result
}

func increaseTargetGate(combo ArbitrageCombination, newTarget decimal.Decimal, liveWork bool) error {
	if combo.Status != "running" {
		return ErrArbitrageConflict
	}
	position := decimal.Zero
	if raw := strings.TrimSpace(combo.PositionNotional); raw != "" {
		parsed, err := decimal.NewFromString(raw)
		if err != nil {
			return fmt.Errorf("%w: current position notional is invalid", ErrArbitrageConfigInvalid)
		}
		position = parsed
	}
	if newTarget.LessThan(position.Abs()) {
		return fmt.Errorf(
			"%w: target notional cannot be below current absolute position %s",
			ErrArbitrageConfigInvalid, position.Abs().String(),
		)
	}
	if !strings.EqualFold(strings.TrimSpace(combo.RunMode), "one_shot") {
		return nil
	}
	phase := strings.TrimSpace(combo.OneShotPhase)
	if phase != "building_target" && phase != "waiting_exit" {
		return ErrArbitrageConflict
	}
	if combo.PositionUncertain || combo.RuntimeState == "manual_intervention" {
		return ErrArbitrageConflict
	}
	if liveWork {
		return ErrArbitrageConflict
	}
	oldTarget, err := decimal.NewFromString(strings.TrimSpace(combo.TargetNotional))
	if err != nil {
		return fmt.Errorf("%w: current target notional is invalid", ErrArbitrageConfigInvalid)
	}
	occupancy := position.Abs()
	if occupancy.IsPositive() && (!newTarget.GreaterThan(oldTarget) || newTarget.LessThan(occupancy)) {
		return fmt.Errorf(
			"%w: one_shot target with inventory must exceed the previous target and cover occupancy %s",
			ErrArbitrageConfigInvalid, occupancy.String(),
		)
	}
	return nil
}

func legAdditionalNotional(newTarget decimal.Decimal, actual string) decimal.Decimal {
	current := decimal.Zero
	if raw := strings.TrimSpace(actual); raw != "" {
		parsed, err := decimal.NewFromString(raw)
		if err == nil {
			current = parsed.Abs()
		}
	}
	extra := newTarget.Sub(current)
	if extra.IsNegative() {
		return decimal.Zero
	}
	return extra
}

func actualLegNotional(venueNotional, basePosition, valuationPrice string) string {
	if strings.TrimSpace(venueNotional) != "" {
		return venueNotional
	}
	base, baseErr := decimal.NewFromString(strings.TrimSpace(basePosition))
	price, priceErr := decimal.NewFromString(strings.TrimSpace(valuationPrice))
	if baseErr != nil || priceErr != nil {
		return "0"
	}
	return base.Abs().Mul(price).String()
}
