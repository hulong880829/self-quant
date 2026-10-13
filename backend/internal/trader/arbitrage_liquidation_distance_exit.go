package trader

import (
	"context"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

func (a *ArbitragePositionAuditor) maybeTriggerLiquidationDistanceExit(
	ctx context.Context,
	item ArbitrageCombination,
) {
	if a == nil || a.store == nil {
		return
	}
	thresholdRaw := strings.TrimSpace(item.EarlyExitLiquidationDistancePct)
	if thresholdRaw == "" ||
		!strings.EqualFold(strings.TrimSpace(item.RunMode), "one_shot") ||
		(item.OneShotPhase != "building_target" && item.OneShotPhase != "waiting_exit") ||
		item.PositionUncertain ||
		strings.EqualFold(strings.TrimSpace(item.RuntimeState), "manual_intervention") {
		return
	}
	threshold, err := decimal.NewFromString(thresholdRaw)
	if err != nil || !threshold.IsPositive() {
		return
	}
	legAHit := liquidationDistanceHit(item.LegALiquidationDistancePct, threshold)
	legBHit := liquidationDistanceHit(item.LegBLiquidationDistancePct, threshold)
	if !legAHit && !legBHit {
		return
	}
	triggeredLeg := "a"
	switch {
	case legAHit && legBHit:
		triggeredLeg = "both"
	case legBHit:
		triggeredLeg = "b"
	}
	observedAt := time.Now().UTC().Format(time.RFC3339Nano)
	details := map[string]any{
		"reason":                     "liquidation_distance_threshold",
		"configuredThresholdPct":     threshold.String(),
		"triggeredLeg":               triggeredLeg,
		"legALiquidationDistancePct": item.LegALiquidationDistancePct,
		"legBLiquidationDistancePct": item.LegBLiquidationDistancePct,
		"observedAt":                 observedAt,
	}
	marked, applied, markErr := a.store.MarkOneShotExiting(
		ctx, item.ID, item.Version, "liquidation_distance_threshold", details,
	)
	if markErr != nil {
		if ctx.Err() == nil {
			a.logger.Error("mark liquidation distance exiting failed",
				"combination_id", item.ID,
				"triggered_leg", triggeredLeg,
				"threshold_pct", threshold.String(),
				"leg_a_liquidation_distance_pct", item.LegALiquidationDistancePct,
				"leg_b_liquidation_distance_pct", item.LegBLiquidationDistancePct,
				"error", markErr,
			)
		}
		return
	}
	a.logger.Info("liquidation distance early exit",
		"combination_id", item.ID,
		"triggered_leg", triggeredLeg,
		"threshold_pct", threshold.String(),
		"leg_a_liquidation_distance_pct", item.LegALiquidationDistancePct,
		"leg_b_liquidation_distance_pct", item.LegBLiquidationDistancePct,
		"cas_applied", applied,
	)
	if !applied || a.exitSignal == nil {
		return
	}
	a.exitSignal.SignalOneShotExit(item.ID, marked.Version, "liquidation_distance_threshold")
}

func liquidationDistanceHit(raw string, threshold decimal.Decimal) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	distance, err := decimal.NewFromString(trimmed)
	if err != nil {
		return false
	}
	return distance.Cmp(threshold) <= 0
}
