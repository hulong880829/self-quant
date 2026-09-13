package trader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/account/portfolio"
	"selfquant/backend/internal/trader/exchange"
)

const createMarkPriceStaleAfter = 15 * time.Second

func defaultLegLeverage(contractType, raw string) (decimal.Decimal, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if strings.EqualFold(contractType, "spot") {
			return decimal.NewFromInt(1), nil
		}
		return decimal.NewFromInt(4), nil
	}
	value, err := decimal.NewFromString(raw)
	if err != nil || !value.IsPositive() || !value.Equal(value.Truncate(0)) {
		return decimal.Zero, newArbitrageCreateError(
			"invalid_leverage",
			fmt.Sprintf("invalid leverage %q", raw),
			"",
			nil,
		)
	}
	if strings.EqualFold(contractType, "spot") && !value.Equal(decimal.NewFromInt(1)) {
		return decimal.Zero, newArbitrageCreateError(
			"invalid_leverage",
			"spot leverage must be 1",
			"",
			map[string]string{"contractType": "spot"},
		)
	}
	if strings.EqualFold(contractType, "perpetual") && value.LessThan(decimal.NewFromInt(1)) {
		return decimal.Zero, newArbitrageCreateError(
			"invalid_leverage",
			"contract leverage must be at least 1",
			"",
			nil,
		)
	}
	return value, nil
}

func tagCreateErrorLeg(err error, leg string) error {
	var createErr *ArbitrageCreateError
	if errors.As(err, &createErr) && strings.TrimSpace(createErr.Leg) == "" {
		createErr.Leg = leg
	}
	return err
}

func existingDirectionNotional(
	snapshot portfolio.Snapshot,
	leg ArbitrageLeg,
	instrument Instrument,
	mid decimal.Decimal,
) decimal.Decimal {
	base, err := venueBasePosition(snapshot, leg, instrument)
	if err != nil || !base.Abs().IsPositive() {
		return decimal.Zero
	}
	if !mid.IsPositive() {
		return decimal.Zero
	}
	return base.Abs().Mul(mid)
}

func existingQuoteNotional(
	snapshot portfolio.Snapshot,
	account Credentials,
	instrument Instrument,
) (decimal.Decimal, error) {
	leg := arbitrageLeg(account, instrument)
	notional := decimal.Zero
	hasNotional := false
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
		value := parseDecimal(position.NotionalUSD)
		if value.IsPositive() {
			notional = notional.Add(value)
			hasNotional = true
		}
	}
	if hasNotional {
		return notional, nil
	}
	base, err := venueBasePosition(snapshot, leg, instrument)
	if err != nil {
		return decimal.Zero, err
	}
	if !base.Abs().IsPositive() {
		return decimal.Zero, nil
	}
	mid := snapshotMarkPrice(snapshot, leg)
	if !mid.IsPositive() {
		return decimal.Zero, fmt.Errorf("existing position mark price is unavailable")
	}
	return base.Abs().Mul(mid), nil
}

func snapshotMarkPrice(
	snapshot portfolio.Snapshot,
	leg ArbitrageLeg,
) decimal.Decimal {
	if strings.EqualFold(leg.ContractType, "spot") {
		return decimal.Zero
	}
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
		mark := parsePositiveDecimal(position.MarkPrice)
		if mark.IsPositive() {
			return mark
		}
	}
	return decimal.Zero
}

func bboMidPrice(bbo exchange.BBO, now time.Time) decimal.Decimal {
	bid := parsePositiveDecimal(bbo.BidPrice)
	ask := parsePositiveDecimal(bbo.AskPrice)
	if !bid.IsPositive() || !ask.IsPositive() {
		return decimal.Zero
	}
	if !bbo.Timestamp.IsZero() && now.Sub(bbo.Timestamp) > createMarkPriceStaleAfter {
		return decimal.Zero
	}
	return bid.Add(ask).Div(decimal.NewFromInt(2))
}

func readOnlyCreateLegCheck(
	account Credentials,
	instrument Instrument,
	legName string,
	leverage decimal.Decimal,
	targetNotional decimal.Decimal,
	snapshot portfolio.Snapshot,
) error {
	if strings.EqualFold(instrument.ContractType, "spot") {
		if !leverage.Equal(decimal.NewFromInt(1)) {
			return newArbitrageCreateError(
				"invalid_leverage",
				"spot leverage must be 1",
				legName,
				nil,
			)
		}
		if targetNotional.IsPositive() {
			availableQuote := parseDecimal(snapshot.SpotBalances[instrument.QuoteAsset])
			availableBase := parseDecimal(snapshot.SpotBalances[instrument.BaseAsset])
			availableFunds := parseDecimal(snapshot.AvailableFundsUSD)
			if availableQuote.IsZero() && availableFunds.IsPositive() {
				availableQuote = availableFunds
			}
			if availableQuote.LessThan(targetNotional) && availableBase.LessThan(targetNotional) {
				if availableQuote.LessThan(targetNotional) {
					return newArbitrageCreateError(
						"insufficient_spot_balance",
						fmt.Sprintf("%s spot balance is insufficient for target notional", account.Exchange),
						legName,
						map[string]string{
							"exchange":  account.Exchange,
							"required":  targetNotional.String(),
							"available": availableQuote.String(),
						},
					)
				}
			}
		}
		return nil
	}
	if !leverage.IsPositive() {
		return newArbitrageCreateError(
			"invalid_leverage",
			"contract leverage must be at least 1",
			legName,
			nil,
		)
	}
	targetMargin := targetNotional.Div(leverage)
	available := parseDecimal(snapshot.AvailableFundsUSD)
	if targetMargin.IsPositive() && available.LessThan(targetMargin) {
		return newArbitrageCreateError(
			"insufficient_margin",
			fmt.Sprintf("%s available margin is insufficient", account.Exchange),
			legName,
			map[string]string{
				"exchange":       account.Exchange,
				"required":       targetMargin.String(),
				"available":      available.String(),
				"leverage":       leverage.String(),
				"targetNotional": targetNotional.String(),
			},
		)
	}
	return nil
}

func applyCreateLeg(
	ctx context.Context,
	adapter exchange.Adapter,
	account Credentials,
	instrument Instrument,
	legName string,
	leverage decimal.Decimal,
	targetNotional decimal.Decimal,
	snapshot portfolio.Snapshot,
	applied []string,
) ([]string, error) {
	if strings.EqualFold(instrument.ContractType, "spot") {
		return applied, nil
	}
	if previewer, ok := adapter.(exchange.LeverageSetPreviewer); ok {
		preview, err := previewer.PreviewSetLeverage(
			ctx, toVenueCredentials(account), toVenueInstrument(instrument), leverage,
		)
		if err != nil {
			return applied, newArbitrageCreateError(
				"venue_unavailable",
				fmt.Sprintf("query %s leverage preview failed: %v", account.Exchange, err),
				legName,
				map[string]string{
					"exchange":    account.Exchange,
					"appliedLegs": appliedLegsJSON(applied),
				},
			)
		}
		targetMargin := targetNotional.Div(leverage)
		delta := preview.MarginChange
		if delta.IsNegative() {
			delta = decimal.Zero
		}
		required := targetMargin.Add(delta)
		available := parseDecimal(snapshot.AvailableFundsUSD)
		if required.IsPositive() && available.LessThan(required) {
			details := map[string]string{
				"exchange":       account.Exchange,
				"required":       required.String(),
				"available":      available.String(),
				"leverage":       leverage.String(),
				"targetNotional": targetNotional.String(),
				"targetMargin":   targetMargin.String(),
				"marginChange":   preview.MarginChange.String(),
				"appliedLegs":    appliedLegsJSON(applied),
			}
			if preview.RequiredMargin.IsPositive() {
				details["requiredMargin"] = preview.RequiredMargin.String()
			}
			return applied, newArbitrageCreateError(
				"insufficient_margin",
				fmt.Sprintf("%s available margin is insufficient", account.Exchange),
				legName,
				details,
			)
		}
		if strings.TrimSpace(preview.EstMaxOpenUnit) != exchange.EstMaxOpenUnitQuoteNotional {
			return applied, newArbitrageCreateError(
				"venue_unavailable",
				fmt.Sprintf("%s estMaxOpen unit is unknown", account.Exchange),
				legName,
				map[string]string{
					"exchange":       account.Exchange,
					"estMaxOpen":     preview.EstMaxOpen,
					"estMaxOpenUnit": preview.EstMaxOpenUnit,
					"appliedLegs":    appliedLegsJSON(applied),
				},
			)
		}
		maxOpen, err := decimal.NewFromString(strings.TrimSpace(preview.EstMaxOpen))
		if err != nil || !maxOpen.IsPositive() {
			return applied, newArbitrageCreateError(
				"venue_unavailable",
				fmt.Sprintf("%s estMaxOpen is invalid", account.Exchange),
				legName,
				map[string]string{
					"exchange":    account.Exchange,
					"estMaxOpen":  preview.EstMaxOpen,
					"appliedLegs": appliedLegsJSON(applied),
				},
			)
		}
		existing, err := existingQuoteNotional(snapshot, account, instrument)
		if err != nil {
			return applied, newArbitrageCreateError(
				"venue_unavailable",
				fmt.Sprintf("%s existing position notional is unavailable: %v", account.Exchange, err),
				legName,
				map[string]string{
					"exchange":    account.Exchange,
					"estMaxOpen":  preview.EstMaxOpen,
					"appliedLegs": appliedLegsJSON(applied),
				},
			)
		}
		requiredNotional := existing.Add(targetNotional)
		if targetNotional.IsPositive() && requiredNotional.GreaterThan(maxOpen) {
			return applied, newArbitrageCreateError(
				"position_capacity_exceeded",
				fmt.Sprintf("%s remaining position capacity is insufficient", account.Exchange),
				legName,
				map[string]string{
					"exchange":    account.Exchange,
					"existing":    existing.String(),
					"target":      targetNotional.String(),
					"required":    requiredNotional.String(),
					"remaining":   maxOpen.String(),
					"estMaxOpen":  preview.EstMaxOpen,
					"leverage":    leverage.String(),
					"appliedLegs": appliedLegsJSON(applied),
				},
			)
		}
	}
	applied, result, err := applyLeverage(ctx, adapter, account, instrument, legName, leverage, applied)
	if err != nil {
		return applied, err
	}
	if err := checkAppliedLeverageCapacity(
		account, instrument, legName, leverage, targetNotional, snapshot, applied, result,
	); err != nil {
		return applied, err
	}
	return applied, nil
}

func applyLeverage(
	ctx context.Context,
	adapter exchange.Adapter,
	account Credentials,
	instrument Instrument,
	legName string,
	leverage decimal.Decimal,
	applied []string,
) ([]string, exchange.LeverageApplyResult, error) {
	if strings.EqualFold(instrument.ContractType, "spot") {
		return applied, exchange.LeverageApplyResult{}, nil
	}
	setter, ok := adapter.(exchange.LeverageSetter)
	if !ok {
		return applied, exchange.LeverageApplyResult{}, newArbitrageCreateError(
			"leverage_apply_failed",
			fmt.Sprintf("%s does not support setting leverage; combination was not created", account.Exchange),
			legName,
			map[string]string{
				"exchange":    account.Exchange,
				"appliedLegs": appliedLegsJSON(applied),
				"target":      leverage.String(),
			},
		)
	}
	result, err := setter.SetLeverage(ctx, toVenueCredentials(account), toVenueInstrument(instrument), leverage)
	if err != nil {
		return applied, exchange.LeverageApplyResult{}, leverageApplyError(
			account.Exchange, legName, leverage, applied, err,
		)
	}
	return append(applied, legName), result, nil
}

func leverageApplyError(
	exchangeName, legName string,
	leverage decimal.Decimal,
	applied []string,
	err error,
) error {
	details := map[string]string{
		"exchange":    exchangeName,
		"appliedLegs": appliedLegsJSON(applied),
		"target":      leverage.String(),
		"error":       err.Error(),
	}
	if errors.Is(err, exchange.ErrUncertain) || errors.Is(err, context.DeadlineExceeded) {
		details["uncertain"] = "true"
		return newArbitrageCreateError(
			"leverage_apply_failed",
			fmt.Sprintf("%s leverage apply result is uncertain; combination was not created", exchangeName),
			legName,
			details,
		)
	}
	return newArbitrageCreateError(
		"leverage_apply_failed",
		fmt.Sprintf("%s leverage was not applied; combination was not created", exchangeName),
		legName,
		details,
	)
}

func checkAppliedLeverageCapacity(
	account Credentials,
	instrument Instrument,
	legName string,
	leverage decimal.Decimal,
	targetNotional decimal.Decimal,
	snapshot portfolio.Snapshot,
	applied []string,
	result exchange.LeverageApplyResult,
) error {
	if !result.CapacityKnown {
		return nil
	}
	existing := existingDirectionNotional(
		snapshot, arbitrageLeg(account, instrument), instrument,
		snapshotMarkPrice(snapshot, arbitrageLeg(account, instrument)),
	)
	required := existing.Add(targetNotional)
	if required.GreaterThan(result.MaxNotional) {
		return newArbitrageCreateError(
			"position_capacity_exceeded",
			fmt.Sprintf("%s position capacity is insufficient", account.Exchange),
			legName,
			map[string]string{
				"exchange":    account.Exchange,
				"existing":    existing.String(),
				"target":      targetNotional.String(),
				"required":    required.String(),
				"max":         result.MaxNotional.String(),
				"leverage":    leverage.String(),
				"appliedLegs": appliedLegsJSON(applied),
			},
		)
	}
	return nil
}

func appliedLegsJSON(applied []string) string {
	if len(applied) == 0 {
		return "[]"
	}
	return `["` + strings.Join(applied, `","`) + `"]`
}
