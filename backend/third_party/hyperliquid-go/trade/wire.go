package trade

import (
	"math"

	"github.com/Simon-Busch/hyperliquid-go/info"
	"github.com/Simon-Busch/hyperliquid-go/internal/wire"
	"github.com/Simon-Busch/hyperliquid-go/types"
)

// sizeToWireWithAsset converts x to a wire-compatible string using the
// asset-specific size-decimals precision.
func sizeToWireWithAsset(x float64, asset int, infoC *info.Client) (string, error) {
	szDecimals, exists := infoC.AssetToDecimalMap()[asset]
	if !exists {
		return wire.SizeToWire(x)
	}
	return wire.SizeToWireDecimals(x, szDecimals)
}

// PriceToWire converts x to a wire-compatible price string for the supplied
// asset and asset class, applying Hyperliquid's five-significant-figure cap
// and the MaxPriceDecimals(class) - szDecimals decimal-place limit.
func PriceToWire(x float64, asset int, infoC *info.Client, class types.AssetClass) (string, error) {
	szDecimals, exists := infoC.AssetToDecimalMap()[asset]
	if !exists {
		return wire.FloatToWire(x)
	}
	allowed := class.MaxPriceDecimals() - szDecimals
	return wire.PriceToWireDecimals(x, allowed)
}

// FormatPriceToTickSize rounds price to satisfy both significant-figure
// and decimal-place constraints documented at
// https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/tick-and-lot-size
//
// Exported so the root package can re-export it via compat.go for legacy
// test coverage; in-package callers use the lowercase formatPriceToTickSize
// alias defined below.
func FormatPriceToTickSize(price float64, szDecimals int, class types.AssetClass) float64 {
	sigFigsRounded, err := wire.RoundToSignificantFigures(price, 5)
	if err != nil {
		return price
	}

	maxPriceDecimals := class.MaxPriceDecimals() - szDecimals
	if maxPriceDecimals < 0 {
		maxPriceDecimals = 0
	}
	multiplier := math.Pow(10, float64(maxPriceDecimals))
	return math.Round(sigFigsRounded*multiplier) / multiplier
}

// formatPriceToTickSize is the package-internal alias used by place.go.
func formatPriceToTickSize(price float64, szDecimals int, class types.AssetClass) float64 {
	return FormatPriceToTickSize(price, szDecimals, class)
}

// roundToTickSize rounds price to the nearest multiple of tickSize.
func roundToTickSize(price, tickSize float64) float64 {
	return math.Round(price/tickSize) * tickSize
}

// getAssetTickSize returns the conservative tick size used for fallback
// rounding when the wire metadata cannot be consulted.
func getAssetTickSize(assetID int) float64 {
	if assetID < 10000 {
		switch assetID {
		case 0: // BTC
			return 0.1
		case 1: // ETH
			return 0.01
		case 2: // SOL
			return 0.01
		default:
			return 0.01
		}
	}
	return 0.0001
}

// validateAndAdjustPrice silently rounds price to the asset's tick grid.
// Tick-violation surfacing now lives in validate() as
// ValidationError{Code:"tick_violation"}.
func validateAndAdjustPrice(price float64, assetID int) (float64, error) {
	tickSize := getAssetTickSize(assetID)
	return roundToTickSize(price, tickSize), nil
}

// formatUsdAmount renders an amount the way the Python SDK does for
// user-signed actions.
func formatUsdAmount(f float64) string {
	return wire.FormatUsdAmount(f)
}
