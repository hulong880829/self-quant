package exchange

import (
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

// ToVenueQuantity converts the public base-asset quantity into the venue's
// wire unit. V1 only supports spot and USDT/USDC linear perpetuals.
func ToVenueQuantity(instrument Instrument, baseQuantity string) (string, error) {
	quantity, err := decimal.NewFromString(strings.TrimSpace(baseQuantity))
	if err != nil || !quantity.IsPositive() {
		return "", fmt.Errorf("%w: invalid quantity", ErrInvalidQuantity)
	}
	factor, contracts, err := quantityFactor(instrument)
	if err != nil {
		return "", err
	}
	if !contracts {
		return quantity.String(), nil
	}
	wire := quantity.Div(factor)
	step := venueQuantityStep(instrument, factor)
	if !step.IsPositive() {
		step = decimal.NewFromInt(1)
	}
	if !wire.Div(step).Equal(wire.Div(step).Truncate(0)) {
		return "", fmt.Errorf(
			"%w: quantity is not an exact multiple of venue step %s",
			ErrInvalidQuantity,
			step.String(),
		)
	}
	return wire.String(), nil
}

// FromVenueQuantity converts venue fills back to base-asset quantity.
func FromVenueQuantity(instrument Instrument, venueQuantity string) (string, error) {
	quantity, err := decimal.NewFromString(strings.TrimSpace(venueQuantity))
	if err != nil {
		return "", fmt.Errorf("invalid venue quantity: %w", err)
	}
	factor, contracts, err := quantityFactor(instrument)
	if err != nil {
		return "", err
	}
	if contracts {
		quantity = quantity.Mul(factor)
	}
	return quantity.Abs().String(), nil
}

// BaseQuantityStep converts a venue-native lot step into base units.
func BaseQuantityStep(instrument Instrument, venueStep string) (string, error) {
	step, err := decimal.NewFromString(strings.TrimSpace(venueStep))
	if err != nil || !step.IsPositive() {
		return "", fmt.Errorf("invalid venue quantity step")
	}
	factor, contracts, err := quantityFactor(instrument)
	if err != nil {
		return "", err
	}
	if contracts {
		step = step.Mul(factor)
	}
	return step.String(), nil
}

func quantityFactor(instrument Instrument) (decimal.Decimal, bool, error) {
	if instrument.ContractType != "perpetual" {
		return decimal.NewFromInt(1), false, nil
	}
	exchange := strings.ToLower(strings.TrimSpace(instrument.Exchange))
	if exchange != "okx" && exchange != "gate" {
		return decimal.NewFromInt(1), false, nil
	}
	size, err := decimal.NewFromString(strings.TrimSpace(instrument.ContractSize))
	if err != nil || !size.IsPositive() {
		return decimal.Zero, true, fmt.Errorf("%w: contract size unavailable", ErrInvalidQuantity)
	}
	if exchange == "okx" {
		if multiplier := metadataDecimal(instrument, "ctMult"); multiplier.IsPositive() {
			size = size.Mul(multiplier)
		}
	}
	return size, true, nil
}

func venueQuantityStep(instrument Instrument, factor decimal.Decimal) decimal.Decimal {
	baseStep, err := decimal.NewFromString(strings.TrimSpace(instrument.QuantityStep))
	if err != nil || !baseStep.IsPositive() || !factor.IsPositive() {
		return decimal.Zero
	}
	return baseStep.Div(factor)
}

func metadataDecimal(instrument Instrument, name string) decimal.Decimal {
	value := metadataString(instrument, name)
	parsed, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil {
		return decimal.Zero
	}
	return parsed
}
