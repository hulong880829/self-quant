package trader

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
)

func normalizePlaceInput(input PlaceOrderInput) (PlaceOrderInput, error) {
	input.Token = strings.TrimSpace(input.Token)
	input.Side = strings.ToLower(strings.TrimSpace(input.Side))
	input.OrderType = strings.ToLower(strings.TrimSpace(input.OrderType))
	input.Quantity = strings.TrimSpace(input.Quantity)
	input.Price = strings.TrimSpace(input.Price)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if input.Token == "" || input.TradingAccountID <= 0 || input.InstrumentID <= 0 ||
		input.IdempotencyKey == "" || (input.Side != "buy" && input.Side != "sell") ||
		(input.OrderType != "market" && input.OrderType != "limit") {
		return PlaceOrderInput{}, ErrInvalidArgument
	}
	quantity, err := decimal.NewFromString(input.Quantity)
	if err != nil || !quantity.IsPositive() {
		return PlaceOrderInput{}, ErrInvalidArgument
	}
	if input.OrderType == "market" && input.Price != "" {
		return PlaceOrderInput{}, ErrInvalidArgument
	}
	if input.OrderType == "limit" {
		price, priceErr := decimal.NewFromString(input.Price)
		if priceErr != nil || !price.IsPositive() {
			return PlaceOrderInput{}, ErrInvalidArgument
		}
		input.Price = price.String()
	}
	input.Quantity = quantity.String()
	return input, nil
}

func validateInstrumentRules(instrument Instrument, orderType, quantity, price string) error {
	_, _, err := prepareOrder(instrument, orderType, quantity, price, price)
	return err
}

func instrumentRulesReady(instrument Instrument, orderType string) bool {
	statusKnown := func(status string) bool {
		return status == exchange.ConstraintKnown ||
			status == exchange.ConstraintNotApplicable
	}
	switch strings.ToLower(strings.TrimSpace(orderType)) {
	case "limit":
		return parsePositiveDecimal(instrument.QuantityStep).IsPositive() &&
			parsePositiveDecimal(instrument.PriceTick).IsPositive() &&
			statusKnown(instrument.MinQuantityStatus) &&
			statusKnown(instrument.MaxQuantityStatus) &&
			statusKnown(instrument.MinNotionalStatus)
	case "market":
		return ruleDecimal(
			instrument.MarketQuantityStep,
			instrument.MarketQuantityStepStatus,
		).IsPositive() &&
			statusKnown(instrument.MarketMinQuantityStatus) &&
			statusKnown(instrument.MarketMaxQuantityStatus) &&
			statusKnown(instrument.MarketMinNotionalStatus)
	default:
		return false
	}
}

func allowBelowMinNotional(execution ArbitrageExecution, instrument Instrument) bool {
	return execution.LastCloseClip &&
		strings.EqualFold(strings.TrimSpace(execution.PositionEffect), "close") &&
		execution.ReduceOnly &&
		strings.EqualFold(instrument.ContractType, "perpetual")
}

func allowBelowMinNotionalCombo(
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	instrument Instrument,
) bool {
	return allowBelowMinNotional(execution, instrument) &&
		arbitrageFlattening(combination)
}

func lastCloseClipMustMatchFills(
	combination ArbitrageCombination,
	execution ArbitrageExecution,
) bool {
	return arbitrageFlattening(combination) &&
		execution.LastCloseClip &&
		strings.EqualFold(strings.TrimSpace(execution.PositionEffect), "close") &&
		execution.ReduceOnly
}

func prepareOrder(
	instrument Instrument,
	orderType, quantity, price, referencePrice string,
) (string, string, error) {
	return prepareOrderMode(instrument, orderType, quantity, price, referencePrice, false)
}

func prepareOrderSkippingMinNotional(
	instrument Instrument,
	orderType, quantity, price, referencePrice string,
) (string, string, error) {
	return prepareOrderMode(instrument, orderType, quantity, price, referencePrice, true)
}

func prepareArbitrageOrder(
	instrument Instrument,
	orderType, quantity, price, referencePrice string,
	execution ArbitrageExecution,
) (string, string, error) {
	if allowBelowMinNotional(execution, instrument) {
		return prepareOrderSkippingMinNotional(instrument, orderType, quantity, price, referencePrice)
	}
	return prepareOrder(instrument, orderType, quantity, price, referencePrice)
}

func prepareOrderMode(
	instrument Instrument,
	orderType, quantity, price, referencePrice string,
	skipMinNotional bool,
) (string, string, error) {
	qty, err := decimal.NewFromString(quantity)
	if err != nil || !qty.IsPositive() {
		return "", "", fmt.Errorf("%w: quantity %q must be positive", ErrInvalidArgument, quantity)
	}
	orderType = strings.ToLower(strings.TrimSpace(orderType))
	step := parsePositiveDecimal(instrument.QuantityStep)
	minQuantity, minQuantityStatus := instrument.MinQuantity, instrument.MinQuantityStatus
	maxQuantity, maxQuantityStatus := instrument.MaxQuantity, instrument.MaxQuantityStatus
	minNotional, minNotionalStatus := instrument.MinNotional, instrument.MinNotionalStatus
	if orderType == "market" {
		step = ruleDecimal(instrument.MarketQuantityStep, instrument.MarketQuantityStepStatus)
		minQuantity, minQuantityStatus = instrument.MarketMinQuantity, instrument.MarketMinQuantityStatus
		maxQuantity, maxQuantityStatus = instrument.MarketMaxQuantity, instrument.MarketMaxQuantityStatus
		minNotional, minNotionalStatus = instrument.MarketMinNotional, instrument.MarketMinNotionalStatus
	} else if orderType != "limit" {
		return "", "", fmt.Errorf("%w: unsupported order type %q", ErrInvalidArgument, orderType)
	}
	if !step.IsPositive() || !qty.Mod(step).IsZero() {
		return "", "", fmt.Errorf(
			"%w: quantity %s does not satisfy %s step %s",
			ErrInvalidArgument, qty, orderType, step,
		)
	}
	if err := validateMinimumRule(qty, minQuantity, minQuantityStatus); err != nil {
		return "", "", err
	}
	if err := validateMaximumRule(qty, maxQuantity, maxQuantityStatus); err != nil {
		return "", "", err
	}
	canonicalPrice := ""
	if orderType == "limit" {
		px, priceErr := decimal.NewFromString(price)
		if priceErr != nil || !px.IsPositive() {
			return "", "", fmt.Errorf("%w: limit price %q must be positive", ErrInvalidArgument, price)
		}
		tick := parsePositiveDecimal(instrument.PriceTick)
		if !tick.IsPositive() || !px.Mod(tick).IsZero() {
			return "", "", fmt.Errorf(
				"%w: price %s does not satisfy tick %s",
				ErrInvalidArgument, px, tick,
			)
		}
		canonicalPrice = px.String()
		referencePrice = canonicalPrice
	}
	if !skipMinNotional {
		if err := validateNotionalRule(
			qty, referencePrice, minNotional, minNotionalStatus,
		); err != nil {
			return "", "", err
		}
	}
	return qty.String(), canonicalPrice, nil
}

func ruleDecimal(value, status string) decimal.Decimal {
	if status != exchange.ConstraintKnown {
		return decimal.Zero
	}
	return parsePositiveDecimal(value)
}

func validateMinimumRule(quantity decimal.Decimal, value, status string) error {
	switch status {
	case exchange.ConstraintKnown:
		minimum := parsePositiveDecimal(value)
		if !minimum.IsPositive() {
			return ErrInstrumentUnavailable
		}
		if quantity.LessThan(minimum) {
			return fmt.Errorf(
				"%w: quantity %s is below minimum %s: %w",
				ErrInvalidArgument, quantity, minimum, ErrOrderBelowMinimum,
			)
		}
	case exchange.ConstraintNotApplicable:
	default:
		return ErrInstrumentUnavailable
	}
	return nil
}

func validateMaximumRule(quantity decimal.Decimal, value, status string) error {
	switch status {
	case exchange.ConstraintKnown:
		maximum := parsePositiveDecimal(value)
		if !maximum.IsPositive() {
			return ErrInstrumentUnavailable
		}
		if quantity.GreaterThan(maximum) {
			return fmt.Errorf(
				"%w: quantity %s exceeds maximum %s",
				ErrInvalidArgument, quantity, maximum,
			)
		}
	case exchange.ConstraintNotApplicable:
	default:
		return ErrInstrumentUnavailable
	}
	return nil
}

func validateNotionalRule(
	quantity decimal.Decimal,
	referencePrice, value, status string,
) error {
	switch status {
	case exchange.ConstraintKnown:
		minimum := parsePositiveDecimal(value)
		price := parsePositiveDecimal(referencePrice)
		if !minimum.IsPositive() || !price.IsPositive() {
			return ErrInstrumentUnavailable
		}
		if quantity.Mul(price).LessThan(minimum) {
			return fmt.Errorf(
				"%w: notional %s is below minimum %s: %w",
				ErrInvalidArgument, quantity.Mul(price), minimum, ErrOrderBelowMinimum,
			)
		}
	case exchange.ConstraintNotApplicable:
	default:
		return ErrInstrumentUnavailable
	}
	return nil
}

func requestFingerprint(
	accountID, instrumentID int64,
	side, orderType, quantity, price string,
	extras ...string,
) string {
	parts := []string{itoa(accountID), itoa(instrumentID), side, orderType, quantity, price}
	parts = append(parts, extras...)
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}

func normalizeTwapInput(input CreateTwapInput, now time.Time) (CreateTwapInput, error) {
	input.Token = strings.TrimSpace(input.Token)
	input.Side = strings.ToLower(strings.TrimSpace(input.Side))
	input.TotalQuantity = strings.TrimSpace(input.TotalQuantity)
	input.LimitPrice = strings.TrimSpace(input.LimitPrice)
	input.MaxQuantity = strings.TrimSpace(input.MaxQuantity)
	input.ExecutionType = strings.ToLower(strings.TrimSpace(input.ExecutionType))
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if input.Token == "" || input.TradingAccountID <= 0 || input.InstrumentID <= 0 ||
		input.IdempotencyKey == "" || (input.Side != "buy" && input.Side != "sell") ||
		(input.ExecutionType != "maker" && input.ExecutionType != "market") ||
		input.StartAt.IsZero() || input.EndAt.IsZero() || !input.EndAt.After(input.StartAt) ||
		input.IntervalSeconds <= 0 || input.EndAt.Sub(input.StartAt) > 7*24*time.Hour ||
		input.StartAt.After(now.Add(30*24*time.Hour)) {
		return CreateTwapInput{}, ErrInvalidArgument
	}
	if input.StartAt.Before(now.Add(-time.Minute)) {
		return CreateTwapInput{}, ErrInvalidArgument
	}
	total, err := decimal.NewFromString(input.TotalQuantity)
	if err != nil || !total.IsPositive() {
		return CreateTwapInput{}, ErrInvalidArgument
	}
	input.TotalQuantity = total.String()
	if input.LimitPrice != "" {
		price, err := decimal.NewFromString(input.LimitPrice)
		if err != nil || !price.IsPositive() {
			return CreateTwapInput{}, ErrInvalidArgument
		}
		input.LimitPrice = price.String()
	}
	if input.MaxQuantity != "" {
		maxQty, err := decimal.NewFromString(input.MaxQuantity)
		if err != nil || !maxQty.IsPositive() {
			return CreateTwapInput{}, ErrInvalidArgument
		}
		slices := int64((input.EndAt.Sub(input.StartAt) + time.Duration(input.IntervalSeconds)*time.Second - 1) /
			(time.Duration(input.IntervalSeconds) * time.Second))
		if maxQty.Mul(decimal.NewFromInt(slices)).LessThan(total) {
			return CreateTwapInput{}, ErrInvalidArgument
		}
		input.MaxQuantity = maxQty.String()
	}
	if input.ExecutionType == "maker" {
		if input.OrderTimeoutSeconds <= 0 || input.OrderTimeoutSeconds >= input.IntervalSeconds {
			return CreateTwapInput{}, ErrInvalidArgument
		}
	} else if input.OrderTimeoutSeconds != 0 {
		return CreateTwapInput{}, ErrInvalidArgument
	}
	return input, nil
}

func validateTwapInstrument(instrument Instrument, input CreateTwapInput) error {
	if err := validateInstrumentRules(instrument, "market", input.TotalQuantity, ""); err != nil {
		return err
	}
	if input.MaxQuantity != "" {
		if err := validateInstrumentRules(instrument, "market", input.MaxQuantity, ""); err != nil {
			return err
		}
	}
	if input.LimitPrice != "" {
		if err := validateInstrumentRules(instrument, "limit", input.TotalQuantity, input.LimitPrice); err != nil {
			return err
		}
	}
	return nil
}

func twapFingerprint(input CreateTwapInput) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		itoa(input.TradingAccountID), itoa(input.InstrumentID), input.Side,
		input.TotalQuantity, input.StartAt.UTC().Format(time.RFC3339Nano),
		input.EndAt.UTC().Format(time.RFC3339Nano), strconv.Itoa(input.IntervalSeconds),
		input.LimitPrice, input.MaxQuantity, input.ExecutionType,
		strconv.Itoa(input.OrderTimeoutSeconds),
	}, "|")))
	return hex.EncodeToString(sum[:])
}

func parsePositiveDecimal(value string) decimal.Decimal {
	parsed, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil || !parsed.IsPositive() {
		return decimal.Zero
	}
	return parsed
}

func itoa(value int64) string {
	return decimal.NewFromInt(value).String()
}
