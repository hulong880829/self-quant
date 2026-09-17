export const ESTIMATED_FEE_BPS = 20;
export const ESTIMATED_SLIPPAGE_BPS = 10;
export const ESTIMATED_ROUND_TRIP_COST_BPS = 30;
export const ESTIMATED_ROUND_TRIP_COST_RATIO = 0.003;

export const FUNDING_PAYBACK_TOOLTIP =
  "按过去24h已结算资金费估算；含开平仓手续费20bps和滑点10bps（合计30bps）；假设平仓基差与入场基差相同。";

export type FundingPaybackStatus =
  | "ready"
  | "non_positive_carry"
  | "rate_unavailable"
  | "stale"
  | "not_applicable";

export type FundingPaybackEstimate = {
  status: FundingPaybackStatus;
  paybackMinutes: number | null;
  costBps: typeof ESTIMATED_ROUND_TRIP_COST_BPS;
  annualizedRatio: number | null;
};

export type FundingPaybackLegInput =
  | { role: "spot" }
  | { role: "perpetual"; status: "missing" | "pending" }
  | {
      role: "perpetual";
      status: "ready";
      cumulative24h: number;
      stale: boolean;
    };

const emptyEstimate = {
  paybackMinutes: null,
  costBps: ESTIMATED_ROUND_TRIP_COST_BPS,
  annualizedRatio: null,
} as const;

export function percentDisplayToRatio(percent: number): number {
  return percent / 100;
}

export function estimateSingleFundingPayback(input: {
  cumulative24h: number;
  stale?: boolean;
}): FundingPaybackEstimate {
  return estimateFromNet24hRatio(
    percentDisplayToRatio(input.cumulative24h),
    Boolean(input.stale),
  );
}

export function estimateCrossFundingPayback(input: {
  spread24hAnnualized: number;
  stale?: boolean;
}): FundingPaybackEstimate {
  return estimateFromNet24hRatio(
    percentDisplayToRatio(input.spread24hAnnualized) / 365,
    Boolean(input.stale),
  );
}

export function estimateDirectionalFundingPayback(
  legA: FundingPaybackLegInput,
  legB: FundingPaybackLegInput,
): FundingPaybackEstimate {
  if (legA.role === "spot" && legB.role === "spot") {
    return { status: "not_applicable", ...emptyEstimate };
  }
  const resolvedA = resolvePaybackLeg(legA);
  const resolvedB = resolvePaybackLeg(legB);
  if (resolvedA.status === "unavailable" || resolvedB.status === "unavailable") {
    return { status: "rate_unavailable", ...emptyEstimate };
  }
  return estimateFromNet24hRatio(
    resolvedB.ratio - resolvedA.ratio,
    resolvedA.stale || resolvedB.stale,
  );
}

export function fundingPaybackLegFromState(
  contractType: string,
  state?: {
    missing: boolean;
    fresh: boolean;
    item?: { cumulative24h: number; stale?: boolean };
  },
): FundingPaybackLegInput {
  if (contractType === "spot") {
    return { role: "spot" };
  }
  if (!state || state.missing || !state.item) {
    return { role: "perpetual", status: state?.missing ? "missing" : "pending" };
  }
  return {
    role: "perpetual",
    status: "ready",
    cumulative24h: state.item.cumulative24h,
    stale: !state.fresh || Boolean(state.item.stale),
  };
}

export function formatFundingPayback(estimate: FundingPaybackEstimate): string {
  if (estimate.status === "not_applicable") return "不适用";
  if (estimate.status === "rate_unavailable") return "资金费数据不可用";
  if (estimate.status === "stale") return "行情过期，暂不估算";
  if (estimate.status === "non_positive_carry") {
    return "当前24h累计收益无法覆盖成本";
  }
  const minutes = estimate.paybackMinutes;
  if (minutes === null || !Number.isFinite(minutes) || minutes <= 0) {
    return "当前24h累计收益无法覆盖成本";
  }
  const rounded = Math.max(1, Math.round(minutes));
  if (Math.floor(rounded / 1_440) > 365) return ">365天";
  return formatFundingPaybackDuration(rounded);
}

export function fundingPaybackTitle(estimate: FundingPaybackEstimate): string {
  if (
    estimate.status === "ready" &&
    estimate.paybackMinutes != null &&
    Number.isFinite(estimate.paybackMinutes)
  ) {
    const rounded = Math.max(1, Math.round(estimate.paybackMinutes));
    if (Math.floor(rounded / 1_440) > 365) {
      return `${FUNDING_PAYBACK_TOOLTIP} 实际约 ${formatFundingPaybackDuration(rounded)}。`;
    }
  }
  return FUNDING_PAYBACK_TOOLTIP;
}

function formatFundingPaybackDuration(rounded: number): string {
  if (rounded < 60) return `${rounded} 分钟`;
  if (rounded < 1_440) {
    const hours = Math.floor(rounded / 60);
    const remainder = rounded % 60;
    return remainder > 0 ? `${hours} 小时 ${remainder} 分钟` : `${hours} 小时`;
  }
  const days = Math.floor(rounded / 1_440);
  const hours = Math.floor((rounded % 1_440) / 60);
  return hours > 0 ? `${days} 天 ${hours} 小时` : `${days} 天`;
}

function estimateFromNet24hRatio(
  net24hRatio: number,
  stale: boolean,
): FundingPaybackEstimate {
  if (stale) {
    return { status: "stale", ...emptyEstimate };
  }
  if (!Number.isFinite(net24hRatio) || net24hRatio <= 0) {
    return {
      status: "non_positive_carry",
      ...emptyEstimate,
      annualizedRatio: Number.isFinite(net24hRatio) ? net24hRatio * 365 : null,
    };
  }
  return {
    status: "ready",
    paybackMinutes: (ESTIMATED_ROUND_TRIP_COST_RATIO / net24hRatio) * 1_440,
    costBps: ESTIMATED_ROUND_TRIP_COST_BPS,
    annualizedRatio: net24hRatio * 365,
  };
}

function resolvePaybackLeg(leg: FundingPaybackLegInput): {
  status: "ok" | "unavailable";
  ratio: number;
  stale: boolean;
} {
  if (leg.role === "spot") {
    return { status: "ok", ratio: 0, stale: false };
  }
  if (leg.status !== "ready") {
    return { status: "unavailable", ratio: 0, stale: false };
  }
  return {
    status: "ok",
    ratio: percentDisplayToRatio(leg.cumulative24h),
    stale: leg.stale,
  };
}
