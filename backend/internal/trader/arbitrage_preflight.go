package trader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"golang.org/x/sync/errgroup"
	"selfquant/backend/internal/account/portfolio"
	"selfquant/backend/internal/trader/exchange"
)

const preflightUSDEquivalent = "USD_EQUIVALENT"

func knownPreflightDecimal(value, unit string) PreflightDecimal {
	if strings.TrimSpace(value) == "" {
		return PreflightDecimal{Status: "unknown", Unit: unit}
	}
	return PreflightDecimal{Value: value, Status: "known", Unit: unit}
}

func unknownPreflightDecimal(unit string) PreflightDecimal {
	return PreflightDecimal{Status: "unknown", Unit: unit}
}

func setPreflightUnit(value *PreflightDecimal, unit string) {
	if value.Unit == "" {
		value.Unit = unit
	}
}

func stampResolvedPreflightLeg(leg *PreflightLeg, instrument Instrument) {
	base := strings.TrimSpace(instrument.BaseAsset)
	quote := strings.TrimSpace(instrument.QuoteAsset)
	setPreflightUnit(&leg.AvailableFunds, preflightUSDEquivalent)
	setPreflightUnit(&leg.RequiredMargin, preflightUSDEquivalent)
	setPreflightUnit(&leg.MinQuantity, base)
	setPreflightUnit(&leg.MinNotional, quote)
	setPreflightUnit(&leg.QuantityStep, base)
	setPreflightUnit(&leg.RoundedBaseQuantity, base)
}

func preflightWarning(code, message, leg, exchange, reason, unit string) PreflightWarning {
	return PreflightWarning{
		Code: code, Message: message, Leg: leg, Exchange: exchange,
		Details: map[string]string{"reason": reason, "unit": unit},
	}
}

func (s *Service) PreflightArbitrageCombination(
	ctx context.Context,
	input CreateArbitrageInput,
) (ArbitragePreflight, error) {
	now := time.Now().UTC()
	result := ArbitragePreflight{
		CheckedAt: now, ExpiresAt: now.Add(3 * time.Second),
		LegA: PreflightLeg{EstimatedFee: unknownPreflightDecimal("")},
		LegB: PreflightLeg{EstimatedFee: unknownPreflightDecimal("")},
	}
	input.IdempotencyKey = "preflight-read-only"
	if strings.TrimSpace(input.AskThresholdBps) == "" {
		input.AskThresholdBps = "0"
	}
	if strings.TrimSpace(input.BidThresholdBps) == "" {
		input.BidThresholdBps = "0"
	}
	normalized, err := normalizeArbitrageInput(input)
	if err != nil {
		return result, err
	}
	if s.arbitrage == nil || s.portfolios == nil {
		result.Code, result.Message = "venue_unavailable", "preflight dependencies are unavailable"
		return result, nil
	}
	if _, err = s.credentials.Owner(ctx, normalized.Token); err != nil {
		return result, err
	}
	accountA, err := s.credentials.Get(ctx, normalized.Token, normalized.LegATradingAccountID)
	if err != nil {
		return result, err
	}
	accountB, err := s.credentials.Get(ctx, normalized.Token, normalized.LegBTradingAccountID)
	if err != nil {
		return result, err
	}
	if !exchangeEnabled(s.arbitrageExchanges, accountA.Exchange) ||
		!exchangeEnabled(s.arbitrageExchanges, accountB.Exchange) {
		result.Code, result.Message = "unsupported_exchange", "exchange is outside the execution allowlist"
		return result, nil
	}
	if accountA.TradingAccountID == accountB.TradingAccountID {
		accountB = accountA
	}
	if accountA.ProductName != accountB.ProductName {
		return result, ErrInvalidArgument
	}
	instrumentA, err := s.catalog.Get(ctx, normalized.LegAInstrumentID)
	if err != nil {
		return result, err
	}
	instrumentB, err := s.catalog.Get(ctx, normalized.LegBInstrumentID)
	if err != nil {
		return result, err
	}
	if !validArbitrageInstrumentPair(accountA, instrumentA, accountB, instrumentB) {
		return result, ErrInvalidArgument
	}
	stampResolvedPreflightLeg(&result.LegA, instrumentA)
	stampResolvedPreflightLeg(&result.LegB, instrumentB)
	levA, err := defaultLegLeverage(instrumentA.ContractType, normalized.LegALeverage)
	if err != nil {
		return preflightFailure(result, tagCreateErrorLeg(err, "a")), nil
	}
	levB, err := defaultLegLeverage(instrumentB.ContractType, normalized.LegBLeverage)
	if err != nil {
		return preflightFailure(result, tagCreateErrorLeg(err, "b")), nil
	}
	normalized.LegALeverage = levA.String()
	normalized.LegBLeverage = levB.String()
	requiredA, requiredB := preflightOrderTypes(normalized, instrumentA, instrumentB)
	if !instrumentRulesReady(instrumentA, requiredA) ||
		!instrumentRulesReady(instrumentB, requiredB) {
		result.Code, result.Message = "instrument_unavailable", "instrument trading rules are unavailable"
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
	}{
		{"a", accountA, adapterA}, {"b", accountB, adapterB},
	} {
		capabilities, capabilityErr := venueCapabilities(ctx, item.adapter, item.account)
		if capabilityErr != nil || !capabilities.Arbitrage {
			result.Code, result.Message = "unsupported_exchange", "exchange does not support arbitrage execution"
			result.Leg, result.Exchange = item.leg, item.account.Exchange
			return result, nil
		}
	}
	if err := preflightPositionModes(ctx, s.timeout, accountA, instrumentA, adapterA, accountB, instrumentB, adapterB); err != nil {
		return preflightFailure(result, err), nil
	}
	snapshotA, snapshotB, err := s.preflightSnapshots(ctx, accountA, accountB)
	if err != nil {
		return preflightFailure(result, err), nil
	}
	target := parsePositiveDecimal(normalized.TargetNotional)
	if err := readOnlyCreateLegCheck(accountA, instrumentA, "a", levA, target, snapshotA); err != nil {
		return preflightFailure(result, err), nil
	}
	if err := readOnlyCreateLegCheck(accountB, instrumentB, "b", levB, target, snapshotB); err != nil {
		return preflightFailure(result, err), nil
	}
	priceA, priceB := s.preflightPrices(
		ctx, accountA, instrumentA, adapterA, snapshotA,
		accountB, instrumentB, adapterB, snapshotB,
	)
	result.LegA = buildPreflightLeg(instrumentA, requiredA, levA, target, snapshotA, priceA)
	result.LegB = buildPreflightLeg(instrumentB, requiredB, levB, target, snapshotB, priceB)
	marginA, marginB, marginErr := s.preflightMargins(
		ctx, adapterA, accountA, instrumentA, "a", levA, target, snapshotA,
		adapterB, accountB, instrumentB, "b", levB, target, snapshotB,
	)
	if marginErr != nil {
		return preflightFailure(result, marginErr), nil
	}
	result.LegA.RequiredMargin = marginA
	result.LegB.RequiredMargin = marginB
	for _, item := range []struct {
		leg      string
		out      PreflightLeg
		exchange string
		base     string
	}{
		{"a", result.LegA, accountA.Exchange, instrumentA.BaseAsset},
		{"b", result.LegB, accountB.Exchange, instrumentB.BaseAsset},
	} {
		if item.out.RoundedBaseQuantity.Status == "unknown" {
			result.Warnings = append(result.Warnings, preflightWarning(
				"price_unknown", "rounded quantity is unavailable without a reliable current price",
				item.leg, item.exchange, "reliable_price_unavailable", strings.TrimSpace(item.base),
			))
		}
		if item.out.EstimatedFee.Status == "unknown" {
			result.Warnings = append(result.Warnings, preflightWarning(
				"fee_unknown", "an exact fee estimate is unavailable",
				item.leg, item.exchange, "fee_estimate_unavailable", "",
			))
		}
		if item.out.RequiredMargin.Status == "unknown" {
			result.Warnings = append(result.Warnings, preflightWarning(
				"margin_unknown", "an exact post-leverage margin requirement is unavailable",
				item.leg, item.exchange, "margin_preview_unavailable", preflightUSDEquivalent,
			))
		}
	}
	result.Executable = true
	return result, nil
}

func preflightFailure(result ArbitragePreflight, err error) ArbitragePreflight {
	var createErr *ArbitrageCreateError
	if errors.As(err, &createErr) {
		result.Code, result.Message, result.Leg = createErr.Code, createErr.Message, createErr.Leg
		result.Exchange = createErr.Details["exchange"]
		return result
	}
	result.Code, result.Message = "preflight_unavailable", err.Error()
	return result
}

func preflightOrderTypes(input CreateArbitrageInput, a, b Instrument) (string, string) {
	orderType := func(instrument Instrument) string {
		if strings.EqualFold(instrument.ContractType, "spot") {
			return "limit"
		}
		return "market"
	}
	aType, bType := orderType(a), orderType(b)
	if input.ExecutionMode == "maker_then_hedge" {
		if input.MakerLeg == "a" {
			aType = "limit"
		} else {
			bType = "limit"
		}
	}
	return aType, bType
}

func preflightPositionModes(
	ctx context.Context, timeout time.Duration,
	accountA Credentials, instrumentA Instrument, adapterA exchange.Adapter,
	accountB Credentials, instrumentB Instrument, adapterB exchange.Adapter,
) error {
	checked := map[int64]bool{}
	for _, item := range []struct {
		account    Credentials
		instrument Instrument
		adapter    exchange.Adapter
	}{{accountA, instrumentA, adapterA}, {accountB, instrumentB, adapterB}} {
		if strings.EqualFold(item.instrument.ContractType, "spot") || checked[item.account.TradingAccountID] {
			continue
		}
		reader, ok := item.adapter.(exchange.PositionModeReader)
		if !ok {
			return fmt.Errorf("%w: %s account mode cannot be verified", ErrPositionMode, item.account.Exchange)
		}
		readCtx, cancel := context.WithTimeout(ctx, timeout)
		mode, err := reader.GetPositionMode(readCtx, toVenueCredentials(item.account), toVenueInstrument(item.instrument))
		cancel()
		if err != nil {
			return fmt.Errorf("%w: verify %s account mode: %v", ErrPositionMode, item.account.Exchange, err)
		}
		if mode != exchange.PositionModeOneWay {
			return fmt.Errorf("%w: %s account is in hedge mode", ErrPositionMode, item.account.Exchange)
		}
		checked[item.account.TradingAccountID] = true
	}
	return nil
}

func (s *Service) preflightSnapshots(
	ctx context.Context, accountA, accountB Credentials,
) (portfolio.Snapshot, portfolio.Snapshot, error) {
	load := func(loadCtx context.Context, account Credentials) (portfolio.Snapshot, error) {
		queryCtx, cancel := context.WithTimeout(loadCtx, s.timeout)
		defer cancel()
		snapshot, err := s.portfolios.Snapshot(queryCtx, account.Exchange, portfolio.Credentials{
			APIKey: account.APIKey, APISecret: account.APISecret, Passphrase: account.Passphrase,
			AccountIndex: account.AccountIndex,
		})
		if err != nil {
			return portfolio.Snapshot{}, fmt.Errorf(
				"%w: capture %s account baseline: %v",
				ErrVenueUnavailable, account.Exchange, err,
			)
		}
		return snapshot, nil
	}
	var a, b portfolio.Snapshot
	if accountA.TradingAccountID == accountB.TradingAccountID {
		var err error
		a, err = load(ctx, accountA)
		return a, a, err
	}
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error { var err error; a, err = load(groupCtx, accountA); return err })
	group.Go(func() error { var err error; b, err = load(groupCtx, accountB); return err })
	return a, b, group.Wait()
}

func buildPreflightLeg(
	instrument Instrument, orderType string,
	leverage, target decimal.Decimal, snapshot portfolio.Snapshot, price decimal.Decimal,
) PreflightLeg {
	available := parseDecimal(snapshot.AvailableFundsUSD)
	required := target
	if !strings.EqualFold(instrument.ContractType, "spot") {
		required = target.Div(leverage)
	}
	step, minQty, minNotional := instrument.QuantityStep, instrument.MinQuantity, instrument.MinNotional
	stepStatus, minQtyStatus, minNotionalStatus := exchange.ConstraintKnown, instrument.MinQuantityStatus, instrument.MinNotionalStatus
	if orderType == "market" {
		step, minQty, minNotional = instrument.MarketQuantityStep, instrument.MarketMinQuantity, instrument.MarketMinNotional
		stepStatus, minQtyStatus, minNotionalStatus = instrument.MarketQuantityStepStatus, instrument.MarketMinQuantityStatus, instrument.MarketMinNotionalStatus
	}
	base := strings.TrimSpace(instrument.BaseAsset)
	quote := strings.TrimSpace(instrument.QuoteAsset)
	out := PreflightLeg{
		AvailableFunds:      knownPreflightDecimal(available.String(), preflightUSDEquivalent),
		RequiredMargin:      knownPreflightDecimal(required.String(), preflightUSDEquivalent),
		EstimatedFee:        unknownPreflightDecimal(""),
		MinQuantity:         constraintPreflightDecimal(minQty, minQtyStatus, base),
		MinNotional:         constraintPreflightDecimal(minNotional, minNotionalStatus, quote),
		QuantityStep:        constraintPreflightDecimal(step, stepStatus, base),
		RoundedBaseQuantity: unknownPreflightDecimal(base),
	}
	stepValue := parsePositiveDecimal(step)
	if price.IsPositive() && stepValue.IsPositive() {
		out.RoundedBaseQuantity = knownPreflightDecimal(floorToStep(target.Div(price), stepValue).String(), base)
	}
	return out
}

func (s *Service) preflightPrices(
	ctx context.Context,
	accountA Credentials, instrumentA Instrument, adapterA exchange.Adapter, snapshotA portfolio.Snapshot,
	accountB Credentials, instrumentB Instrument, adapterB exchange.Adapter, snapshotB portfolio.Snapshot,
) (decimal.Decimal, decimal.Decimal) {
	read := func(
		readCtx context.Context, account Credentials, instrument Instrument,
		adapter exchange.Adapter, snapshot portfolio.Snapshot,
	) decimal.Decimal {
		queryCtx, cancel := context.WithTimeout(readCtx, s.timeout)
		defer cancel()
		bbo, err := adapter.GetBBO(queryCtx, toVenueInstrument(instrument))
		if err == nil {
			if price := bboMidPrice(bbo, time.Now().UTC()); price.IsPositive() {
				return price
			}
		}
		return snapshotMarkPrice(snapshot, arbitrageLeg(account, instrument))
	}
	var a, b decimal.Decimal
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		a = read(groupCtx, accountA, instrumentA, adapterA, snapshotA)
		return nil
	})
	group.Go(func() error {
		b = read(groupCtx, accountB, instrumentB, adapterB, snapshotB)
		return nil
	})
	_ = group.Wait()
	return a, b
}

func (s *Service) preflightMargins(
	ctx context.Context,
	adapterA exchange.Adapter, accountA Credentials, instrumentA Instrument, legA string,
	leverageA, targetA decimal.Decimal, snapshotA portfolio.Snapshot,
	adapterB exchange.Adapter, accountB Credentials, instrumentB Instrument, legB string,
	leverageB, targetB decimal.Decimal, snapshotB portfolio.Snapshot,
) (PreflightDecimal, PreflightDecimal, error) {
	var marginA, marginB PreflightDecimal
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		var err error
		marginA, err = s.preflightMargin(
			groupCtx, adapterA, accountA, instrumentA, legA, leverageA, targetA, snapshotA,
		)
		return err
	})
	group.Go(func() error {
		var err error
		marginB, err = s.preflightMargin(
			groupCtx, adapterB, accountB, instrumentB, legB, leverageB, targetB, snapshotB,
		)
		return err
	})
	if err := group.Wait(); err != nil {
		return PreflightDecimal{}, PreflightDecimal{}, err
	}
	return marginA, marginB, nil
}

func (s *Service) preflightMargin(
	ctx context.Context, adapter exchange.Adapter, account Credentials,
	instrument Instrument, leg string, leverage, target decimal.Decimal,
	snapshot portfolio.Snapshot,
) (PreflightDecimal, error) {
	if strings.EqualFold(instrument.ContractType, "spot") {
		return knownPreflightDecimal(target.String(), preflightUSDEquivalent), nil
	}
	previewer, ok := adapter.(exchange.LeverageSetPreviewer)
	if !ok {
		return unknownPreflightDecimal(preflightUSDEquivalent), nil
	}
	queryCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	preview, err := previewer.PreviewSetLeverage(
		queryCtx, toVenueCredentials(account), toVenueInstrument(instrument), leverage,
	)
	if err != nil {
		return unknownPreflightDecimal(preflightUSDEquivalent), nil
	}
	targetMargin := target.Div(leverage)
	delta := preview.MarginChange
	if delta.IsNegative() {
		delta = decimal.Zero
	}
	required := targetMargin.Add(delta)
	available := parseDecimal(snapshot.AvailableFundsUSD)
	if required.IsPositive() && available.LessThan(required) {
		return PreflightDecimal{}, newArbitrageCreateError(
			"insufficient_margin",
			fmt.Sprintf("%s available margin is insufficient", account.Exchange),
			leg,
			map[string]string{
				"exchange": account.Exchange, "required": required.String(),
				"available": available.String(), "leverage": leverage.String(),
				"targetNotional": target.String(),
			},
		)
	}
	if strings.TrimSpace(preview.EstMaxOpenUnit) != exchange.EstMaxOpenUnitQuoteNotional {
		return unknownPreflightDecimal(preflightUSDEquivalent), nil
	}
	maxOpen := parsePositiveDecimal(preview.EstMaxOpen)
	if !maxOpen.IsPositive() {
		return unknownPreflightDecimal(preflightUSDEquivalent), nil
	}
	existing, existingErr := existingQuoteNotional(snapshot, account, instrument)
	if existingErr != nil {
		return PreflightDecimal{}, existingErr
	}
	if existing.Add(target).GreaterThan(maxOpen) {
		return PreflightDecimal{}, newArbitrageCreateError(
			"position_capacity_exceeded",
			fmt.Sprintf("%s remaining position capacity is insufficient", account.Exchange),
			leg,
			map[string]string{
				"exchange": account.Exchange, "required": existing.Add(target).String(),
				"remaining": maxOpen.String(), "leverage": leverage.String(),
			},
		)
	}
	return knownPreflightDecimal(required.String(), preflightUSDEquivalent), nil
}

func constraintPreflightDecimal(value, status, unit string) PreflightDecimal {
	if status != exchange.ConstraintKnown || strings.TrimSpace(value) == "" {
		return unknownPreflightDecimal(unit)
	}
	return knownPreflightDecimal(value, unit)
}
