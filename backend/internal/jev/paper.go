package jev

import (
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/config"
)

type Fill struct {
	Qty          decimal.Decimal
	LongPrice    decimal.Decimal
	ShortPrice   decimal.Decimal
	Notional     decimal.Decimal
	Cost         decimal.Decimal
	LongQuoteTS  time.Time
	ShortQuoteTS time.Time
}

func AtomicTwoLegQuotes(now time.Time, maxAge time.Duration, long, short Quote) bool {
	return long.Tradable() && short.Tradable() && long.Fresh(now, maxAge) && short.Fresh(now, maxAge)
}

func EnterFill(cfg config.Jev, now time.Time, long, short Quote) (Fill, error) {
	if !AtomicTwoLegQuotes(now, cfg.BBOMaxAge, long, short) {
		return Fill{}, fmt.Errorf("missing fresh two-leg bbo")
	}
	qty, err := SharedBaseQty(cfg.PositionNotionalUSDT, long.Ask, short.Bid)
	if err != nil {
		return Fill{}, err
	}
	if !fillQtyOK(long, short, qty, true) {
		return Fill{}, fmt.Errorf("bbo_depth_insufficient")
	}
	notional := OneLegNotional(qty, long.Ask)
	return Fill{
		Qty: qty, LongPrice: long.Ask, ShortPrice: short.Bid,
		Notional: notional, Cost: HalfRoundTripCost(notional, cfg.PaperRoundTripCostBPS),
		LongQuoteTS: long.TS, ShortQuoteTS: short.TS,
	}, nil
}

func ExitFill(cfg config.Jev, now time.Time, position Position, long, short Quote) (Fill, error) {
	if !AtomicTwoLegQuotes(now, cfg.BBOMaxAge, long, short) {
		return Fill{}, fmt.Errorf("missing fresh two-leg bbo")
	}
	qty := position.SharedBaseQty
	if !qty.IsPositive() {
		return Fill{}, fmt.Errorf("shared base qty missing")
	}
	if !fillQtyOK(long, short, qty, false) {
		return Fill{}, fmt.Errorf("bbo_depth_insufficient")
	}
	notional := OneLegNotional(qty, long.Bid)
	return Fill{
		Qty: qty, LongPrice: long.Bid, ShortPrice: short.Ask,
		Notional: notional, Cost: HalfRoundTripCost(notional, cfg.PaperRoundTripCostBPS),
		LongQuoteTS: long.TS, ShortQuoteTS: short.TS,
	}, nil
}

func ApplyEnter(runID int64, candidate Candidate, fill Fill, now time.Time, long, short Quote) Position {
	opened := now
	position := Position{
		RunID: runID, StrategyVersion: StrategyVersion, Kind: candidate.Kind, Status: PositionOpen, CandidateKey: candidate.Key,
		BaseAsset: candidate.BaseAsset, QuoteAsset: candidate.QuoteAsset, GlobalSymbol: candidate.GlobalSymbol,
		LongExchange: candidate.LongExchange, LongSymbol: candidate.LongSymbol, LongProduct: candidate.LongProduct,
		LongCanonicalSymbol: candidate.LongCanonicalSymbol, LongQuoteAsset: candidate.LongQuoteAsset,
		ShortExchange: candidate.ShortExchange, ShortSymbol: candidate.ShortSymbol, ShortProduct: candidate.ShortProduct,
		ShortCanonicalSymbol: candidate.ShortCanonicalSymbol, ShortQuoteAsset: candidate.ShortQuoteAsset,
		EntryMidBasisBPS: MidBasisBPS(long, short),
		SharedBaseQty:    fill.Qty, LongQty: fill.Qty, ShortQty: fill.Qty.Neg(),
		LongEntryPrice: fill.LongPrice, ShortEntryPrice: fill.ShortPrice,
		LongMarkPrice: fill.LongPrice, ShortMarkPrice: fill.ShortPrice,
		CostPaid: fill.Cost, OpenedAt: opened, LastMarkAt: &opened,
	}
	if candidate.LongProduct == ProductPerpetual {
		position.FundingCheckedThroughLong = &opened
	}
	if candidate.ShortProduct == ProductPerpetual {
		position.FundingCheckedThroughShort = &opened
	}
	return position
}

func ApplyExit(position Position, fill Fill, now time.Time) Position {
	realized := position.RealizedSpreadPnL.Add(exitSpreadPnL(position, fill))
	closed := now
	position.Status = PositionClosed
	position.ClosedAt = &closed
	position.RealizedSpreadPnL = realized
	position.UnrealizedSpreadPnL = decimal.Zero
	position.CostPaid = position.CostPaid.Add(fill.Cost)
	position.LongMarkPrice = fill.LongPrice
	position.ShortMarkPrice = fill.ShortPrice
	position.LastMarkAt = &closed
	position.ExitPendingReason = ""
	position.DegradedReason = ""
	return position
}

func exitSpreadPnL(position Position, fill Fill) decimal.Decimal {
	longPnL := position.LongQty.Mul(fill.LongPrice.Sub(position.LongEntryPrice))
	shortPnL := position.ShortQty.Mul(fill.ShortPrice.Sub(position.ShortEntryPrice))
	return longPnL.Add(shortPnL)
}

func MarkToMarket(position Position, long, short Quote, now time.Time, maxAge time.Duration) Position {
	if !AtomicTwoLegQuotes(now, maxAge, long, short) {
		if position.DegradedReason == "" {
			position.DegradedReason = "missing_mark_bbo"
		}
		return position
	}
	longMid := long.Mid()
	shortMid := short.Mid()
	longPnL := position.LongQty.Mul(longMid.Sub(position.LongEntryPrice))
	shortPnL := position.ShortQty.Mul(shortMid.Sub(position.ShortEntryPrice))
	position.LongMarkPrice = longMid
	position.ShortMarkPrice = shortMid
	position.UnrealizedSpreadPnL = longPnL.Add(shortPnL)
	position.LastMarkAt = &now
	if position.DegradedReason == "missing_mark_bbo" {
		position.DegradedReason = ""
	}
	return position
}

func FundingPnLDelta(qty, settlementPrice, rate decimal.Decimal) decimal.Decimal {
	return qty.Neg().Mul(settlementPrice).Mul(rate)
}

func SettlementMidValid(settledAt, bboTS time.Time, maxAge time.Duration) bool {
	if settledAt.IsZero() || bboTS.IsZero() || maxAge <= 0 {
		return false
	}
	delta := settledAt.Sub(bboTS)
	return delta >= 0 && delta <= maxAge
}

func FundingBoundary(watermark *time.Time, openedAt time.Time) time.Time {
	if watermark != nil {
		return *watermark
	}
	return openedAt
}

func SortHistoryAscending(points []HistoryPoint) []HistoryPoint {
	out := append([]HistoryPoint(nil), points...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].SettledAt.Before(out[j].SettledAt)
	})
	return out
}

func HistoryTruncated(watermark *time.Time, openedAt time.Time, points []HistoryPoint, limit int) bool {
	if limit <= 0 || len(points) < limit {
		return false
	}
	oldest := points[0].SettledAt
	for _, point := range points {
		if point.SettledAt.Before(oldest) {
			oldest = point.SettledAt
		}
	}
	return oldest.After(FundingBoundary(watermark, openedAt))
}

func NextWatermark(current *time.Time, settledAt time.Time) *time.Time {
	if current == nil || settledAt.After(*current) {
		value := settledAt
		return &value
	}
	return current
}

func RecomputeRunTotals(run Run, positions []Position) Run {
	realized := run.ArchivedRealizedSpreadPnL
	unrealized := decimal.Zero
	funding := run.ArchivedFundingPnL
	cost := run.ArchivedCostPaid
	for _, position := range positions {
		realized = realized.Add(position.RealizedSpreadPnL)
		unrealized = unrealized.Add(position.UnrealizedSpreadPnL)
		funding = funding.Add(position.FundingPnL)
		cost = cost.Add(position.CostPaid)
	}
	net := realized.Add(unrealized).Add(funding).Sub(cost)
	run.RealizedSpreadPnL = realized
	run.UnrealizedSpreadPnL = unrealized
	run.FundingPnL = funding
	run.CumulativeCost = cost
	run.NetPnL = net
	run.Equity = run.InitialEquity.Add(net)
	if run.Equity.GreaterThan(run.PeakEquity) {
		run.PeakEquity = run.Equity
	}
	drawdown := run.PeakEquity.Sub(run.Equity)
	if drawdown.GreaterThan(run.MaxDrawdown) {
		run.MaxDrawdown = drawdown
	}
	return run
}
