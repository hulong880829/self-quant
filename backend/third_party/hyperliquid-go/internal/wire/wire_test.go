package wire

import (
	"math"
	"testing"
)

func TestSizeToWireDecimalsPreservesIntegerTrailingZeros(t *testing.T) {
	tests := []struct {
		name       string
		value      float64
		szDecimals int
		want       string
	}{
		{name: "ninety integer", value: 90, szDecimals: 0, want: "90"},
		{name: "one hundred integer", value: 100, szDecimals: 0, want: "100"},
		{name: "fractional trailing zeros", value: 90.1, szDecimals: 3, want: "90.1"},
		{name: "negative zero integer", value: math.Copysign(0, -1), szDecimals: 0, want: "0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := SizeToWireDecimals(test.value, test.szDecimals)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("SizeToWireDecimals(%v, %d)=%q want=%q",
					test.value, test.szDecimals, got, test.want)
			}
		})
	}
}

func TestSizeToWirePreservesIntegerTrailingZeros(t *testing.T) {
	got, err := SizeToWire(90)
	if err != nil {
		t.Fatal(err)
	}
	if got != "90" {
		t.Fatalf("SizeToWire(90)=%q want=%q", got, "90")
	}
}
