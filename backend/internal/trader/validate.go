package trader

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
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
	qty, err := decimal.NewFromString(quantity)
	if err != nil || !qty.IsPositive() {
		return ErrInvalidArgument
	}
	if step := parsePositiveDecimal(instrument.QuantityStep); step.IsPositive() {
		if !qty.Mod(step).IsZero() {
			return ErrInvalidArgument
		}
	}
	if orderType == "limit" {
		px, priceErr := decimal.NewFromString(price)
		if priceErr != nil || !px.IsPositive() {
			return ErrInvalidArgument
		}
		if tick := parsePositiveDecimal(instrument.PriceTick); tick.IsPositive() {
			if !px.Mod(tick).IsZero() {
				return ErrInvalidArgument
			}
		}
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
