package jev

import (
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/config"
)

func NetIfExit(cfg config.Jev, position Position, long, short Quote, now time.Time) (decimal.Decimal, bool) {
	if !AtomicTwoLegQuotes(now, cfg.BBOMaxAge, long, short) {
		return decimal.Zero, false
	}
	fill := Fill{
		Qty: position.SharedBaseQty, LongPrice: long.Bid, ShortPrice: short.Ask,
		Notional: OneLegNotional(position.SharedBaseQty, long.Bid),
	}
	fill.Cost = HalfRoundTripCost(fill.Notional, cfg.PaperRoundTripCostBPS)
	spread := position.RealizedSpreadPnL.Add(exitSpreadPnL(position, fill))
	return spread.Add(position.FundingPnL).Sub(position.CostPaid).Sub(fill.Cost), true
}

func FirstFundingSettled(position Position) bool {
	checked := false
	if position.LongProduct == ProductPerpetual {
		checked = true
		if position.FundingCheckedThroughLong == nil || !position.FundingCheckedThroughLong.After(position.OpenedAt) {
			return false
		}
	}
	if position.ShortProduct == ProductPerpetual {
		checked = true
		if position.FundingCheckedThroughShort == nil || !position.FundingCheckedThroughShort.After(position.OpenedAt) {
			return false
		}
	}
	return checked
}

func ApplyExitState(cfg config.Jev, position Position, fundingReturn decimal.Decimal, long, short Quote, now time.Time) Position {
	hasBBO := AtomicTwoLegQuotes(now, cfg.BBOMaxAge, long, short)
	if hasBBO {
		if net, ok := NetIfExit(cfg, position, long, short, now); ok {
			position.NetIfExit = &net
			position.NetIfExitAt = &now
		}
		position.BBOMissingSince = nil
	} else if position.BBOMissingSince == nil {
		ts := now
		position.BBOMissingSince = &ts
	}

	maxHold := !position.OpenedAt.IsZero() && now.Sub(position.OpenedAt) >= cfg.MaxHoldDuration
	missingTimeout := !hasBBO && position.BBOMissingSince != nil &&
		now.Sub(*position.BBOMissingSince) >= cfg.MissingBBOForceAfter
	fundingReverse := !fundingReturn.IsPositive()
	stopLoss := hasBBO && position.NetIfExit != nil && !position.NetIfExit.GreaterThan(cfg.StopLossNetUSDT)

	if maxHold || missingTimeout || fundingReverse || stopLoss {
		position.ExitPendingReason = MandatoryExitPending
		position.ExitConfirmCount = 0
		return position
	}
	if position.ExitPendingReason == MandatoryExitPending {
		return position
	}

	takeProfit := hasBBO && FirstFundingSettled(position) && position.NetIfExit != nil &&
		!position.NetIfExit.LessThan(cfg.TakeProfitPadUSDT)
	if takeProfit {
		position.ExitConfirmCount++
	} else {
		position.ExitConfirmCount = 0
	}
	return position
}

func ShouldFlattenNow(cfg config.Jev, position Position, long, short Quote, now time.Time) bool {
	if !AtomicTwoLegQuotes(now, cfg.BBOMaxAge, long, short) {
		return false
	}
	if position.ExitPendingReason == MandatoryExitPending {
		return true
	}
	return cfg.ExitConfirmCycles > 0 && position.ExitConfirmCount >= cfg.ExitConfirmCycles
}

func TakeProfitReady(cfg config.Jev, position Position) bool {
	return FirstFundingSettled(position) && position.NetIfExit != nil &&
		!position.NetIfExit.LessThan(cfg.TakeProfitPadUSDT)
}
