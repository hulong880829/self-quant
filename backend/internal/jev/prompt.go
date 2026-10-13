package jev

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/config"
)

func BuildDecisionPrompt(cfg config.Jev, candidate Candidate, position *Position, now time.Time, forceExit bool) string {
	costExample := RoundTripCost(cfg.PositionNotionalUSDT, cfg.PaperRoundTripCostBPS)
	half := HalfRoundTripCost(cfg.PositionNotionalUSDT, cfg.PaperRoundTripCostBPS)
	var b strings.Builder
	b.WriteString("You are JEV paper-trading policy DecisionPromptV4.\n")
	b.WriteString("This is simulated cash-and-carry / cross-perp funding arb. No live orders, no borrowing, no shorting spot.\n")
	b.WriteString("Single-venue: buy spot and sell the same-exchange perpetual. Cross-venue: long the lower 24h-cumulative funding perp and short the higher 24h-cumulative funding perp.\n")
	fmt.Fprintf(&b, "Configured round-trip cost is %s bps of one-leg notional. At %s USDT notional that is %s USDT total; enter and exit each charge half (%s USDT).\n",
		cfg.PaperRoundTripCostBPS.String(), cfg.PositionNotionalUSDT.String(), costExample.String(), half.String())
	b.WriteString("Positions are delta-neutral with a shared base quantity: long_qty = +qty, short_qty = -qty, fixed after entry.\n")
	b.WriteString("Forecast fields are mid-based and already net of one round-trip cost at the combined layer. Do not recompute expected_net_return_24h.\n")
	b.WriteString("Prefer enter only when expected 24h net return after costs is positive, basis history is sufficient, and data is fresh. Hold when uncertain or data is degraded. Exit when the 24h edge is gone, reversed, or the hold is too long.\n")
	if forceExit {
		b.WriteString("SYSTEM: max hold duration has been reached. You must choose exit.\n")
	}
	fmt.Fprintf(&b, "Now (UTC): %s\n", now.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "Candidate JSON: %s\n", mustJSON(candidateView(candidate, position)))
	if position != nil {
		fmt.Fprintf(&b, "Open position JSON: %s\n", mustJSON(positionView(*position, now, candidate)))
	} else {
		b.WriteString("Open position: none. This is an entry-universe candidate only.\n")
	}
	return b.String()
}

func candidateView(candidate Candidate, position *Position) map[string]any {
	view := map[string]any{
		"key":                           candidate.Key,
		"kind":                          candidate.Kind,
		"base_asset":                    candidate.BaseAsset,
		"quote_asset":                   candidate.QuoteAsset,
		"global_symbol":                 candidate.GlobalSymbol,
		"long_exchange":                 candidate.LongExchange,
		"long_symbol":                   candidate.LongSymbol,
		"long_product":                  candidate.LongProduct,
		"long_canonical_symbol":         candidate.LongCanonicalSymbol,
		"short_exchange":                candidate.ShortExchange,
		"short_symbol":                  candidate.ShortSymbol,
		"short_product":                 candidate.ShortProduct,
		"short_canonical_symbol":        candidate.ShortCanonicalSymbol,
		"cumulative_24h_long":           candidate.Cumulative24hLong.String(),
		"cumulative_24h_short":          candidate.Cumulative24hShort.String(),
		"edge_24h":                      candidate.Edge24h.String(),
		"funding_return_24h":            candidate.FundingReturn24h.String(),
		"expected_net_return_24h":       candidate.ExpectedNetReturn24h.String(),
		"expected_total_net_return_24h": candidate.ExpectedNetReturn24h.String(),
		"holding_horizon_net_return":    candidate.HoldingHorizonNetReturn.String(),
		"expected_payback_minutes":      optionalDecimal(candidate.ExpectedPaybackMinutes),
		"current_funding_long":          candidate.CurrentFundingLong.String(),
		"current_funding_short":         candidate.CurrentFundingShort.String(),
		"next_funding_long":             optionalDecimal(candidate.NextFundingLong),
		"next_funding_short":            optionalDecimal(candidate.NextFundingShort),
		"long_bid":                      candidate.LongQuote.Bid.String(),
		"long_ask":                      candidate.LongQuote.Ask.String(),
		"short_bid":                     candidate.ShortQuote.Bid.String(),
		"short_ask":                     candidate.ShortQuote.Ask.String(),
		"current_mid_basis_bps":         optionalDecimal(candidate.CurrentMidBasisBPS),
		"expected_basis_return_24h":     optionalDecimal(candidate.ExpectedBasisReturn24h),
		"basis_return_p10":              optionalDecimal(candidate.BasisReturnP10),
		"basis_profit_probability":      optionalDecimal(candidate.BasisProfitProbability),
		"basis_forecast_confidence":     optionalDecimal(candidate.BasisForecastConfidence),
		"basis_change_1h":               optionalDecimal(candidate.BasisChange1hBPS),
		"basis_change_8h":               optionalDecimal(candidate.BasisChange8hBPS),
		"basis_sample_count":            optionalInt(candidate.BasisSampleCount),
		"basis_model_version":           optionalString(candidate.BasisModelVersion),
		"entry_executable_basis_bps":    executableBasisBPS(candidate.LongQuote, candidate.ShortQuote),
	}
	if position != nil {
		view["entry_mid_basis_bps"] = optionalDecimal(position.EntryMidBasisBPS)
		view["basis_change_since_entry_bps"] = optionalDecimal(BasisChangeSinceEntryBPS(candidate.CurrentMidBasisBPS, position.EntryMidBasisBPS))
	} else {
		view["entry_mid_basis_bps"] = nil
		view["basis_change_since_entry_bps"] = nil
	}
	return view
}

func positionView(position Position, now time.Time, candidate Candidate) map[string]any {
	return map[string]any{
		"id":                           fmt.Sprintf("%d", position.ID),
		"kind":                         position.Kind,
		"candidate_key":                position.CandidateKey,
		"shared_base_qty":              position.SharedBaseQty.String(),
		"long_qty":                     position.LongQty.String(),
		"short_qty":                    position.ShortQty.String(),
		"long_entry_price":             position.LongEntryPrice.String(),
		"short_entry_price":            position.ShortEntryPrice.String(),
		"entry_mid_basis_bps":          optionalDecimal(position.EntryMidBasisBPS),
		"current_mid_basis_bps":        optionalDecimal(candidate.CurrentMidBasisBPS),
		"basis_change_since_entry_bps": optionalDecimal(BasisChangeSinceEntryBPS(candidate.CurrentMidBasisBPS, position.EntryMidBasisBPS)),
		"unrealized_spread_pnl":        position.UnrealizedSpreadPnL.String(),
		"funding_pnl":                  position.FundingPnL.String(),
		"cost_paid":                    position.CostPaid.String(),
		"hold_duration_seconds":        int(now.Sub(position.OpenedAt).Seconds()),
		"exit_pending_reason":          emptyToNil(position.ExitPendingReason),
		"degraded_reason":              emptyToNil(position.DegradedReason),
	}
}

func executableBasisBPS(long, short Quote) any {
	if !long.Ask.IsPositive() || !short.Bid.IsPositive() {
		return nil
	}
	value := short.Bid.Div(long.Ask).Sub(decimal.NewFromInt(1)).Mul(decimal.NewFromInt(10000))
	return value.String()
}

func optionalDecimal(value *decimal.Decimal) any {
	if value == nil {
		return nil
	}
	return value.String()
}

func optionalInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func optionalString(value *string) any {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	return *value
}

func emptyToNil(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func mustJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(raw)
}
