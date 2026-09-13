package trader

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
)

func metricFill(
	leg, side, quantity, price string,
	at time.Time,
) arbitrageMetricFill {
	return arbitrageMetricFill{
		leg: leg, side: side,
		quantity: decimal.RequireFromString(quantity),
		price:    decimal.RequireFromString(price),
		at:       at,
	}
}

func TestReplayArbitrageCostsDoesNotCreateReversePnLInventory(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	fills := []arbitrageMetricFill{
		metricFill("a", "buy", "2", "100", base),
		metricFill("a", "buy", "2", "110", base.Add(time.Second)),
		metricFill("a", "sell", "1", "120", base.Add(2*time.Second)),
		metricFill("a", "sell", "4", "90", base.Add(3*time.Second)),
		metricFill("b", "sell", "3", "101", base),
	}
	legA, legB := replayArbitrageCosts(fills, time.Time{})
	if !legA.position.IsZero() || !legA.average.IsZero() {
		t.Fatalf("leg A position=%s average=%s", legA.position, legA.average)
	}
	// First close realizes 15; the reversal closes the remaining 3 at -15 each.
	if !legA.realized.Equal(decimal.NewFromInt(-30)) {
		t.Fatalf("leg A realized=%s", legA.realized)
	}
	if !legB.position.Equal(decimal.NewFromInt(-3)) ||
		!legB.average.Equal(decimal.NewFromInt(101)) {
		t.Fatalf("leg B position=%s average=%s", legB.position, legB.average)
	}
}

func TestReplayArbitrageCostsAttributesBidToComboBeforeBaseline(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	baselineA := decimal.NewFromInt(200)
	baselineB := decimal.NewFromInt(-200)
	ask := []arbitrageMetricFill{
		metricFill("a", "buy", "300", "100", base),
		metricFill("b", "sell", "300", "101", base),
	}
	cases := []struct {
		name, bid string
		want      string
	}{
		{name: "partial combo close", bid: "100", want: "200"},
		{name: "full combo close", bid: "300", want: "0"},
		{name: "combo then baseline close", bid: "500", want: "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fills := append([]arbitrageMetricFill{}, ask...)
			fills = append(fills,
				metricFill("a", "sell", tc.bid, "110", base.Add(time.Hour)),
				metricFill("b", "buy", tc.bid, "90", base.Add(time.Hour)),
			)
			legA, legB := replayArbitrageCosts(
				fills, time.Time{}, baselineA, baselineB,
			)
			want := decimal.RequireFromString(tc.want)
			if !legA.position.Equal(want) || !legB.position.Equal(want.Neg()) {
				t.Fatalf("positions=%s/%s want=%s/%s",
					legA.position, legB.position, want, want.Neg())
			}
		})
	}
}

func TestReplayArbitrageCostsExcludesPureBaselineClose(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	fills := []arbitrageMetricFill{
		metricFill("a", "sell", "100", "110", base),
		metricFill("b", "buy", "100", "90", base),
	}
	legA, legB := replayArbitrageCosts(
		fills, time.Time{}, decimal.NewFromInt(200), decimal.NewFromInt(-200),
	)
	if !legA.position.IsZero() || !legB.position.IsZero() ||
		!legA.realized.IsZero() || !legB.realized.IsZero() {
		t.Fatalf("baseline close leaked into PnL: A=%+v B=%+v", legA, legB)
	}
}

func TestReplayArbitrageCostsOnlyAttributesAskBeyondReverseBaseline(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	fills := []arbitrageMetricFill{
		metricFill("a", "buy", "300", "100", base),
		metricFill("b", "sell", "300", "101", base),
	}
	legA, legB := replayArbitrageCosts(
		fills, time.Time{}, decimal.NewFromInt(-200), decimal.NewFromInt(200),
	)
	if !legA.position.Equal(decimal.NewFromInt(100)) ||
		!legB.position.Equal(decimal.NewFromInt(-100)) {
		t.Fatalf("positions=%s/%s", legA.position, legB.position)
	}
}

func TestReplayArbitrageCostsStopsAtFundingTime(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	fills := []arbitrageMetricFill{
		metricFill("a", "buy", "1", "100", base),
		metricFill("a", "sell", "1", "105", base.Add(2*time.Hour)),
	}
	atSettlement, _ := replayArbitrageCosts(fills, base.Add(time.Hour))
	if !atSettlement.position.Equal(decimal.NewFromInt(1)) ||
		!atSettlement.average.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("settlement state=%+v", atSettlement)
	}
}

func TestReplayArbitrageExposureUsesPairedSingleLegNotional(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	fills := []arbitrageMetricFill{
		metricFill("a", "buy", "2", "100", base),
		metricFill("b", "sell", "1.5", "102", base),
		metricFill("b", "buy", "1.5", "103", base.Add(time.Hour)),
	}
	exposure, holding := replayArbitrageExposure(
		fills, base, base.Add(2*time.Hour),
	)
	if !holding.Equal(decimal.NewFromInt(3600)) {
		t.Fatalf("holding=%s", holding)
	}
	// min(2,1.5) * average(100,102) * 3600
	want := decimal.RequireFromString("545400")
	if !exposure.Equal(want) {
		t.Fatalf("exposure=%s want=%s", exposure, want)
	}
}

func TestCalculateCombinedAnnualizedClampsShortHoldingToFundingPeriod(t *testing.T) {
	holding := decimal.NewFromInt(60)
	exposure := decimal.NewFromInt(1000).Mul(holding)
	profit := decimal.RequireFromString("0.5") // 0.05% of notional.
	value, ok := calculateCombinedAnnualized(profit, exposure, holding, 4)
	if !ok {
		t.Fatal("annualized value unavailable")
	}
	// 0.05% * 365 days / 4 hours = 109.5%.
	if !value.Equal(decimal.RequireFromString("1.095")) {
		t.Fatalf("annualized=%s", value)
	}
}

func TestTradingFeeCostRatePreservesVenueSignConvention(t *testing.T) {
	rebate := decimal.RequireFromString("-0.0002")
	deduction := decimal.RequireFromString("0.0005")
	okxDeduction := decimal.RequireFromString("-0.0005")
	if got := tradingFeeCostRate("binance", rebate); !got.Equal(rebate) {
		t.Fatalf("binance rebate cost=%s", got)
	}
	if got := tradingFeeCostRate("binance", deduction); !got.Equal(deduction) {
		t.Fatalf("binance deduction cost=%s", got)
	}
	if got := tradingFeeCostRate("OKX", okxDeduction); !got.Equal(okxDeduction.Neg()) {
		t.Fatalf("okx deduction cost=%s", got)
	}
	if got := tradingFeeCostRate("okx", decimal.RequireFromString("0.0001")); !got.Equal(
		decimal.RequireFromString("-0.0001"),
	) {
		t.Fatalf("okx credit cost=%s", got)
	}
}

func TestEstimateTradingFeesUsesCurrentAccountRates(t *testing.T) {
	binanceMaker := "0.0002"
	okxTaker := "-0.0005"
	accounts := map[int64]accountFeeSnapshot{
		1: {exchange: "binance", contractMaker: &binanceMaker, contractTaker: &binanceMaker},
		2: {exchange: "okx", contractMaker: &okxTaker, contractTaker: &okxTaker},
	}
	total, ok := estimateTradingFees([]terminalFilledOrder{
		{accountID: 1, exchange: "binance", contractType: "perpetual", role: "maker",
			quantity: decimal.RequireFromString("0.01"), price: decimal.NewFromInt(100)},
		{accountID: 2, exchange: "okx", contractType: "perpetual", role: "hedge",
			quantity: decimal.RequireFromString("0.01"), price: decimal.NewFromInt(101)},
	}, accounts)
	if !ok {
		t.Fatal("fee estimate incomplete")
	}
	want := decimal.RequireFromString("0.01").Mul(decimal.NewFromInt(100)).Mul(
		decimal.RequireFromString("0.0002"),
	).Add(
		decimal.RequireFromString("0.01").Mul(decimal.NewFromInt(101)).Mul(
			decimal.RequireFromString("0.0005"),
		),
	)
	if !total.Equal(want) {
		t.Fatalf("fee=%s want=%s", total, want)
	}
}

func TestEstimateTradingFeesIncompleteWithoutAccountRate(t *testing.T) {
	_, ok := estimateTradingFees([]terminalFilledOrder{
		{accountID: 1, exchange: "binance", contractType: "perpetual", role: "market",
			quantity: decimal.NewFromInt(1), price: decimal.NewFromInt(100)},
	}, map[int64]accountFeeSnapshot{1: {exchange: "binance"}})
	if ok {
		t.Fatal("missing contract taker rate should be incomplete")
	}
}

func TestCalculateCombinedAnnualizedScalesWithProfit(t *testing.T) {
	holding := decimal.NewFromInt(8 * 3600)
	exposure := decimal.NewFromInt(1000).Mul(holding)
	withFees := decimal.NewFromInt(5)
	net := decimal.NewFromInt(1)
	withFeesValue, ok := calculateCombinedAnnualized(withFees, exposure, holding, 4)
	if !ok {
		t.Fatal("gross annualized unavailable")
	}
	netValue, ok := calculateCombinedAnnualized(net, exposure, holding, 4)
	if !ok {
		t.Fatal("net annualized unavailable")
	}
	if withFeesValue.Equal(netValue) {
		t.Fatal("annualized should scale with realized net profit")
	}
}

func TestCalculateCombinedAnnualizedUsesActualHoldingAfterThreshold(t *testing.T) {
	holding := decimal.NewFromInt(8 * 3600)
	exposure := decimal.NewFromInt(1000).Mul(holding)
	profit := decimal.NewFromInt(1)
	value, ok := calculateCombinedAnnualized(profit, exposure, holding, 4)
	if !ok {
		t.Fatal("annualized value unavailable")
	}
	want := decimal.NewFromInt(1).
		Mul(decimal.NewFromInt(annualSeconds)).
		Div(decimal.NewFromInt(1000 * 8 * 3600))
	if !value.Equal(want) {
		t.Fatalf("annualized=%s want=%s", value, want)
	}
}

func TestLoadArbitrageReplaySynthesizesOneFillPerTerminalOrder(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	queryer := stubReplayQuerier{rows: []stubReplayOrderRow{
		{id: "order-canceled", leg: "a", side: "buy", qty: "2", price: "100", at: at},
		{id: "order-filled", leg: "b", side: "sell", qty: "2", price: "101", at: at.Add(time.Second)},
	}}
	replay, err := loadArbitrageReplay(context.Background(), queryer, "combo", false)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.coverageComplete || replay.orderRowCount != 2 || replay.fillRowCount != 2 {
		t.Fatalf("replay=%+v", replay)
	}
	if len(replay.fills) != 2 {
		t.Fatalf("fills=%d", len(replay.fills))
	}
	if replay.fills[0].orderID != "order-canceled" ||
		!replay.fills[0].quantity.Equal(decimal.NewFromInt(2)) ||
		!replay.fills[0].price.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("first fill=%+v", replay.fills[0])
	}
	if !replay.grossTurnover.Equal(decimal.RequireFromString("402")) {
		t.Fatalf("turnover=%s", replay.grossTurnover)
	}
}

type stubReplayOrderRow struct {
	id, leg, side, qty, price string
	at                        time.Time
}

type stubReplayQuerier struct {
	rows []stubReplayOrderRow
}

func (s stubReplayQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return &stubReplayRows{rows: s.rows}, nil
}

type stubReplayRows struct {
	rows []stubReplayOrderRow
	idx  int
}

func (r *stubReplayRows) Close()                                       {}
func (r *stubReplayRows) Err() error                                   { return nil }
func (r *stubReplayRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *stubReplayRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *stubReplayRows) Conn() *pgx.Conn                              { return nil }
func (r *stubReplayRows) RawValues() [][]byte                          { return nil }
func (r *stubReplayRows) Values() ([]any, error)                       { return nil, nil }

func (r *stubReplayRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *stubReplayRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	*(dest[0].(*string)) = row.id
	*(dest[1].(*string)) = row.leg
	*(dest[2].(*string)) = row.side
	*(dest[3].(*string)) = row.qty
	*(dest[4].(*string)) = row.price
	*(dest[5].(*time.Time)) = row.at
	return nil
}

func persistMetrics(
	t *testing.T,
	ctx context.Context,
	repo *Repository,
	id, midA, midB string,
	now time.Time,
) arbitragePositionMetricsPersistResult {
	t.Helper()
	result, err := repo.PersistRunningArbitragePositionMetrics(ctx, id, midA, midB, now)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
