// Package wire holds pure number-formatting helpers used by the public
// price/size serialisation surface. Nothing in here depends on the
// hyperliquid root package — that lets the root keep these helpers
// arms-length from its public API.
package wire

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// RoundToDecimals rounds value to the given number of decimal places.
func RoundToDecimals(value float64, decimals int) float64 {
	pow := math.Pow(10, float64(decimals))
	return math.Round(value*pow) / pow
}

// ParseFloat parses s as float64; returns 0 on parse failure.
func ParseFloat(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0.0
	}
	return f
}

// Abs returns the absolute value of x.
func Abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// FormatFloat formats f with six decimal places.
func FormatFloat(f float64) string {
	return fmt.Sprintf("%.6f", f)
}

// FormatUsdAmount renders an amount the way the Python SDK does for
// user-signed actions (str(amount) in Python). Examples:
//
//	100.0 -> "100.0"
//	1.5   -> "1.5"
//	0.001 -> "0.001"
//
// The exact representation matters because the same string is hashed
// during signing and sent in the request body.
func FormatUsdAmount(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// SizeToWire converts x to a wire-compatible string at the default
// three-decimal lot precision. Used when no asset-specific size precision
// is available.
func SizeToWire(x float64) (string, error) {
	rounded := fmt.Sprintf("%.3f", x)
	if rounded == "-0.000" {
		rounded = "0.000"
	}
	return trimFractionalZeros(rounded), nil
}

// SizeToWireDecimals converts x to a wire-compatible string with the
// supplied size-decimals precision.
func SizeToWireDecimals(x float64, szDecimals int) (string, error) {
	rounded := fmt.Sprintf("%.*f", szDecimals, x)
	if rounded == "-0" ||
		(strings.HasPrefix(rounded, "-0.") &&
			strings.TrimRight(strings.TrimPrefix(rounded, "-0."), "0") == "") {
		rounded = "0" + strings.TrimPrefix(rounded, "-0")
	}
	return trimFractionalZeros(rounded), nil
}

func trimFractionalZeros(value string) string {
	if !strings.Contains(value, ".") {
		return value
	}
	return strings.TrimSuffix(strings.TrimRight(value, "0"), ".")
}

// FloatToWire converts x to a wire-compatible string with up to eight
// decimal places, returning an error if rounding would change the value.
func FloatToWire(x float64) (string, error) {
	rounded := fmt.Sprintf("%.8f", x)
	parsed, err := strconv.ParseFloat(rounded, 64)
	if err != nil {
		return "", err
	}
	if math.Abs(parsed-x) >= 1e-12 {
		return "", fmt.Errorf("float_to_wire causes rounding: %f", x)
	}
	if rounded == "-0.00000000" {
		rounded = "0.00000000"
	}
	result := strings.TrimRight(rounded, "0")
	result = strings.TrimRight(result, ".")
	return result, nil
}

// RoundToSignificantFigures rounds x to n significant figures.
func RoundToSignificantFigures(x float64, n int) (float64, error) {
	if x == 0 {
		return 0, nil
	}
	if n <= 0 {
		return 0, fmt.Errorf("significant figures must be > 0")
	}
	d := math.Ceil(math.Log10(math.Abs(x)))
	power := n - int(d)
	magnitude := math.Pow(10, float64(power))
	shifted := math.Round(x * magnitude)
	return shifted / magnitude, nil
}

// PriceToWireDecimals converts x to a wire-compatible price string given a
// pre-computed allowed-decimals count (typically MaxPriceDecimals - szDecimals)
// and the five-significant-figure cap.
func PriceToWireDecimals(x float64, allowedDecimals int) (string, error) {
	if allowedDecimals < 0 {
		allowedDecimals = 0
	}
	roundedSig, err := RoundToSignificantFigures(x, 5)
	if err != nil {
		return "", err
	}
	rounded := fmt.Sprintf("%.*f", allowedDecimals, roundedSig)
	if strings.HasPrefix(rounded, "-0.") && strings.TrimRight(strings.TrimPrefix(rounded, "-0."), "0") == "" {
		rounded = "0" + strings.TrimPrefix(rounded, "-0")
	}
	result := strings.TrimRight(rounded, "0")
	result = strings.TrimRight(result, ".")
	return result, nil
}
