package trader

import (
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestNormalizeEarlyExitFunding8hFloor(t *testing.T) {
	base := CreateArbitrageInput{
		Token: "token", IdempotencyKey: "request-123",
		LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		TargetNotional:     "1000",
		ExecutionMode:      "maker_then_hedge",
		RunMode:            "one_shot",
		ExitPolicy:         "annualized",
		ExitAnnualizedRate: "0.15",
	}

	empty, err := normalizeArbitrageInput(base)
	if err != nil {
		t.Fatal(err)
	}
	if empty.EarlyExitFunding8hAnnualizedFloor != "" {
		t.Fatalf("empty floor=%q", empty.EarlyExitFunding8hAnnualizedFloor)
	}

	cases := []struct {
		name, raw, want string
	}{
		{name: "percent-as-ratio", raw: "0.05", want: "0.05"},
		{name: "zero", raw: "0", want: "0"},
		{name: "negative", raw: "-0.10", want: "-0.1"},
		{name: "trimmed", raw: " 0.05 ", want: "0.05"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := base
			input.EarlyExitFunding8hAnnualizedFloor = tc.raw
			got, normErr := normalizeArbitrageInput(input)
			if normErr != nil {
				t.Fatal(normErr)
			}
			if got.EarlyExitFunding8hAnnualizedFloor != tc.want {
				t.Fatalf("floor=%q want=%q", got.EarlyExitFunding8hAnnualizedFloor, tc.want)
			}
		})
	}

	for _, raw := range []string{"abc", "NaN", "Inf", "+Infinity", "-inf"} {
		input := base
		input.EarlyExitFunding8hAnnualizedFloor = raw
		if _, normErr := normalizeArbitrageInput(input); !errors.Is(normErr, ErrInvalidArgument) {
			t.Fatalf("raw=%q err=%v", raw, normErr)
		}
	}

	spread := base
	spread.RunMode = "spread"
	spread.AskThresholdBps = "12"
	spread.BidThresholdBps = "-8"
	spread.EarlyExitFunding8hAnnualizedFloor = "0.05"
	normalizedSpread, err := normalizeArbitrageInput(spread)
	if err != nil {
		t.Fatal(err)
	}
	if normalizedSpread.EarlyExitFunding8hAnnualizedFloor != "" {
		t.Fatalf("spread floor=%q", normalizedSpread.EarlyExitFunding8hAnnualizedFloor)
	}
}

func TestArbitrageFingerprintIncludesFunding8hFloor(t *testing.T) {
	base := ArbitrageCombination{
		LegA:            ArbitrageLeg{TradingAccountID: 1, InstrumentID: 11},
		LegB:            ArbitrageLeg{TradingAccountID: 2, InstrumentID: 22},
		AskThresholdBps: "0", BidThresholdBps: "0",
		TargetNotional: "1000", ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		RunMode: "one_shot", EntryDirection: "ask",
		ExitPolicy: "annualized", ExitAnnualizedRate: "0.15",
	}
	withFloor := base
	withFloor.EarlyExitFunding8hAnnualizedFloor = "0.05"
	if arbitrageFingerprint(base) == arbitrageFingerprint(withFloor) {
		t.Fatal("floor must change fingerprint")
	}
	if sameArbitrageRequest(base, withFloor) {
		t.Fatal("different floors must not be the same request")
	}
	if !sameArbitrageRequest(withFloor, withFloor) {
		t.Fatal("identical floors must match")
	}
}

func TestEvaluateFunding8hAskBidAndSpot(t *testing.T) {
	end := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	start := end.Add(-8 * time.Hour)
	combo := funding8hCombo{
		EntryDirection: "ask",
		Floor:          "0.05",
		LegAType:       "perpetual",
		LegBType:       "perpetual",
		LegA: []funding8hPoint{
			funding8hPt(start, "1", "8"),
			funding8hPt(end, "0.0001", "8"),
		},
		LegB: []funding8hPoint{
			funding8hPt(start, "1", "8"),
			funding8hPt(end, "0.0004", "8"),
		},
	}
	ask := evaluateFunding8hExit(combo, start, end)
	wantAsk := decimal.RequireFromString("0.0003").Mul(decimal.NewFromInt(3)).Mul(decimal.NewFromInt(365))
	if !ask.OK || !ask.CumA.Equal(decimal.RequireFromString("0.0001")) ||
		!ask.CumB.Equal(decimal.RequireFromString("0.0004")) ||
		!ask.Observed.Equal(wantAsk) {
		t.Fatalf("ask=%+v wantObserved=%s", ask, wantAsk)
	}
	if ask.Trigger {
		t.Fatal("0.3285 must not trigger below 0.05")
	}
	combo.Floor = wantAsk.String()
	equal := evaluateFunding8hExit(combo, start, end)
	if !equal.OK || equal.Trigger {
		t.Fatalf("equal floor must not trigger: %+v", equal)
	}
	combo.Floor = "0.40"
	if result := evaluateFunding8hExit(combo, start, end); !result.OK || !result.Trigger {
		t.Fatalf("strictly less must trigger: %+v", result)
	}

	combo.EntryDirection = "bid"
	combo.Floor = "0"
	bid := evaluateFunding8hExit(combo, start, end)
	wantBid := decimal.RequireFromString("-0.0003").Mul(decimal.NewFromInt(3)).Mul(decimal.NewFromInt(365))
	if !bid.OK || !bid.Observed.Equal(wantBid) || !bid.Trigger {
		t.Fatalf("bid=%+v wantObserved=%s", bid, wantBid)
	}

	spot := combo
	spot.EntryDirection = "ask"
	spot.LegAType = "spot"
	spot.LegA = nil
	spot.Floor = "0.05"
	spotResult := evaluateFunding8hExit(spot, start, end)
	if !spotResult.OK || !spotResult.CumA.IsZero() ||
		!spotResult.CumB.Equal(decimal.RequireFromString("0.0004")) {
		t.Fatalf("spot=%+v", spotResult)
	}
}

func TestEvaluateFunding8hCoverage(t *testing.T) {
	end := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	start := end.Add(-8 * time.Hour)

	t.Run("3h periods", func(t *testing.T) {
		combo := perpCombo(
			funding8hPt(start, "1", "3"),
			funding8hPt(start.Add(3*time.Hour), "0.0001", "3"),
			funding8hPt(start.Add(6*time.Hour), "0.0002", "3"),
		)
		result := evaluateFunding8hExit(combo, start, end)
		if !result.OK || !result.CumA.Equal(decimal.RequireFromString("0.0003")) ||
			result.CumA.Equal(decimal.RequireFromString("1.0003")) {
			t.Fatalf("3h=%+v", result)
		}
	})

	t.Run("6h periods", func(t *testing.T) {
		combo := perpCombo(
			funding8hPt(start.Add(-4*time.Hour), "1", "6"),
			funding8hPt(start.Add(2*time.Hour), "0.0002", "6"),
		)
		result := evaluateFunding8hExit(combo, start, end)
		if !result.OK || !result.CumA.Equal(decimal.RequireFromString("0.0002")) {
			t.Fatalf("6h=%+v", result)
		}
	})

	t.Run("2 to 5 minute slack", func(t *testing.T) {
		combo := perpCombo(
			funding8hPt(start, "1", "4"),
			funding8hPt(start.Add(4*time.Hour+4*time.Minute), "0.0001", "4"),
			funding8hPt(end, "0.0001", "4"),
		)
		if result := evaluateFunding8hExit(combo, start, end); !result.OK {
			t.Fatalf("4m slack=%+v", result)
		}
		combo = perpCombo(
			funding8hPt(start, "1", "4"),
			funding8hPt(start.Add(4*time.Hour+5*time.Minute), "0.0001", "4"),
			funding8hPt(end, "0.0001", "4"),
		)
		if result := evaluateFunding8hExit(combo, start, end); !result.OK {
			t.Fatalf("5m slack=%+v", result)
		}
	})

	t.Run("period change 8h to 4h", func(t *testing.T) {
		combo := perpCombo(
			funding8hPt(start, "1", "8"),
			funding8hPt(end, "0.0004", "4"),
		)
		result := evaluateFunding8hExit(combo, start, end)
		if !result.OK || !result.CumA.Equal(decimal.RequireFromString("0.0004")) {
			t.Fatalf("period change=%+v", result)
		}
	})

	t.Run("anchor excluded from sum", func(t *testing.T) {
		combo := perpCombo(
			funding8hPt(start, "9", "8"),
			funding8hPt(end, "0.0001", "8"),
		)
		result := evaluateFunding8hExit(combo, start, end)
		if !result.OK || !result.CumA.Equal(decimal.RequireFromString("0.0001")) {
			t.Fatalf("anchor=%+v", result)
		}
	})
}

func TestEvaluateFunding8hDoesNotTriggerWhenIncomplete(t *testing.T) {
	end := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	start := end.Add(-8 * time.Hour)
	completeB := []funding8hPoint{
		funding8hPt(start, "0", "8"),
		funding8hPt(end, "0", "8"),
	}

	cases := []struct {
		name string
		legA []funding8hPoint
	}{
		{name: "no anchor", legA: []funding8hPoint{funding8hPt(end, "0.0001", "8")}},
		{name: "no window points", legA: []funding8hPoint{funding8hPt(start, "0.0001", "8")}},
		{
			name: "gap over 5 minutes",
			legA: []funding8hPoint{
				funding8hPt(start, "0.0001", "4"),
				funding8hPt(start.Add(4*time.Hour+6*time.Minute), "0.0001", "4"),
			},
		},
		{
			name: "stale last record",
			legA: []funding8hPoint{
				funding8hPt(start, "0.0001", "1"),
				funding8hPt(start.Add(time.Hour), "0.0001", "1"),
			},
		},
		{
			name: "non-positive interval",
			legA: []funding8hPoint{
				funding8hPt(start, "0.0001", "0"),
				funding8hPt(end, "0.0001", "8"),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			combo := funding8hCombo{
				EntryDirection: "ask", Floor: "0.05",
				LegAType: "perpetual", LegBType: "perpetual",
				LegA: tc.legA, LegB: completeB,
			}
			result := evaluateFunding8hExit(combo, start, end)
			if result.OK || result.Trigger {
				t.Fatalf("result=%+v", result)
			}
		})
	}

	missing := funding8hCombo{
		EntryDirection: "ask", Floor: "0.05",
		LegAType: "perpetual", LegBType: "perpetual",
		LegA: nil, LegB: completeB,
	}
	if result := evaluateFunding8hExit(missing, start, end); result.OK || result.Trigger {
		t.Fatalf("missing perp=%+v", result)
	}
}

func TestGroupFunding8hExitRowsSkipsDuplicateKeys(t *testing.T) {
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	rate := "0.0001"
	hours := "8"
	row := funding8hExitRow{
		CombinationID: "combo-1", Version: 3, EntryDirection: "ask", Floor: "0.05",
		Leg: "a", ContractType: "perpetual", FundingTime: &at, FundingRate: &rate,
		IntervalHours: &hours,
	}
	_, duplicates := groupFunding8hExitRows([]funding8hExitRow{row, row})
	if _, ok := duplicates["combo-1"]; !ok {
		t.Fatal("expected duplicate skip")
	}
}

func funding8hPt(at time.Time, rate, hours string) funding8hPoint {
	return funding8hPoint{
		At: at, Rate: decimal.RequireFromString(rate),
		IntervalHours: decimal.RequireFromString(hours),
	}
}

func perpCombo(points ...funding8hPoint) funding8hCombo {
	end := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	start := end.Add(-8 * time.Hour)
	return funding8hCombo{
		EntryDirection: "ask", Floor: "1",
		LegAType: "perpetual", LegBType: "perpetual",
		LegA: points,
		LegB: []funding8hPoint{
			funding8hPt(start, "0", "8"),
			funding8hPt(end, "0", "8"),
		},
	}
}
