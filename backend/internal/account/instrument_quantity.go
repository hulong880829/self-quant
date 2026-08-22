package account

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/account/portfolio"
)

func (s *Service) normalizeCEXSnapshot(
	ctx context.Context,
	exchange string,
	snapshot *portfolio.Snapshot,
) error {
	var warnings []error
	for index := range snapshot.Positions {
		position := &snapshot.Positions[index]
		if position.Kind != "cex" {
			continue
		}
		raw, err := decimal.NewFromString(position.Size)
		if err != nil {
			warnings = append(warnings, fmt.Errorf("%s: invalid position size", position.Symbol))
			position.SignedContractSize = ""
			continue
		}
		raw = raw.Abs()
		var spec instrumentSpec
		var found bool
		if s.instruments != nil {
			wireSymbol := position.WireSymbol
			if wireSymbol == "" {
				wireSymbol = position.Symbol
			}
			spec, found, err = s.instruments.Lookup(ctx, exchange, wireSymbol)
			if err != nil {
				warnings = append(warnings, fmt.Errorf("%s: instrument lookup failed", position.Symbol))
			}
		}
		if found {
			position.BaseAsset = strings.ToUpper(strings.TrimSpace(spec.BaseAsset))
		}
		if position.BaseAsset == "" {
			position.BaseAsset = normalizedBaseAsset(position.Symbol)
		}
		baseQuantity, converted := convertBaseQuantity(*position, raw, spec, found)
		if !converted {
			warnings = append(warnings, fmt.Errorf("%s: contract specification unavailable", position.Symbol))
			position.SignedContractSize = ""
			continue
		}
		position.Size = baseQuantity.Abs().String()
		if strings.EqualFold(position.Side, "short") {
			position.SignedContractSize = baseQuantity.Abs().Neg().String()
		} else {
			position.SignedContractSize = baseQuantity.Abs().String()
		}
		position.SpotSize = snapshot.SpotBalances[position.BaseAsset]
		if position.SpotSize == "" {
			position.SpotSize = "0"
		}
	}
	return errors.Join(warnings...)
}

func convertBaseQuantity(
	position portfolio.Position,
	raw decimal.Decimal,
	spec instrumentSpec,
	found bool,
) (decimal.Decimal, bool) {
	category := strings.ToLower(position.ProductCategory)
	switch strings.ToLower(position.Exchange) {
	case "binance":
		if category == "um" {
			return raw, true
		}
	case "bybit":
		if category == "linear" {
			return raw, true
		}
	case "bitget":
		if category == "usdt-futures" || category == "usdc-futures" {
			return raw, true
		}
	}

	if found {
		model := strings.ToLower(instrumentMetadataString(spec, "contractModel"))
		sizeUnit := strings.ToLower(instrumentMetadataString(spec, "positionSizeUnit"))
		switch sizeUnit {
		case "base":
			return raw, true
		case "quote":
			return divideByMark(raw, position.MarkPrice)
		case "contracts":
			value := raw.Mul(spec.ContractSize)
			if strings.EqualFold(position.Exchange, "OKX") {
				if multiplier, err := decimal.NewFromString(
					instrumentMetadataString(spec, "ctMult"),
				); err == nil && multiplier.IsPositive() {
					value = value.Mul(multiplier)
				}
			}
			if model == "inverse" {
				return divideByMark(value, position.MarkPrice)
			}
			return value, true
		}
	}

	// These categories have stable documented units even if a newly listed
	// instrument has not reached the local catalog yet.
	switch {
	case strings.EqualFold(position.Exchange, "Binance") && category == "cm":
		return quantityFromNotional(position.NotionalUSD, position.MarkPrice)
	case strings.EqualFold(position.Exchange, "Bybit") && category == "inverse":
		return divideByMark(raw, position.MarkPrice)
	case strings.EqualFold(position.Exchange, "Bitget") && category == "coin-futures":
		return quantityFromNotional(position.NotionalUSD, position.MarkPrice)
	}
	return quantityFromNotional(position.NotionalUSD, position.MarkPrice)
}

func quantityFromNotional(notional, mark string) (decimal.Decimal, bool) {
	value, valueErr := decimal.NewFromString(notional)
	price, priceErr := decimal.NewFromString(mark)
	if valueErr != nil || priceErr != nil || !price.IsPositive() {
		return decimal.Zero, false
	}
	return value.Abs().Div(price), true
}

func divideByMark(value decimal.Decimal, mark string) (decimal.Decimal, bool) {
	price, err := decimal.NewFromString(mark)
	if err != nil || !price.IsPositive() {
		return decimal.Zero, false
	}
	return value.Abs().Div(price), true
}

func normalizedBaseAsset(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	for _, suffix := range []string{"-PERP", "-SWAP"} {
		value = strings.TrimSuffix(value, suffix)
	}
	for _, suffix := range []string{"USDT", "USDC", "USD"} {
		value = strings.TrimSuffix(value, suffix)
	}
	return strings.TrimRight(value, "-_")
}
