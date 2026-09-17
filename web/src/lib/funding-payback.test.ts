import { describe, expect, it } from "vitest";

import {
  ESTIMATED_FEE_BPS,
  ESTIMATED_ROUND_TRIP_COST_BPS,
  ESTIMATED_ROUND_TRIP_COST_RATIO,
  ESTIMATED_SLIPPAGE_BPS,
  estimateCrossFundingPayback,
  estimateDirectionalFundingPayback,
  estimateSingleFundingPayback,
  formatFundingPayback,
  fundingPaybackLegFromState,
  fundingPaybackTitle,
  percentDisplayToRatio,
} from "./funding-payback";

describe("funding payback constants", () => {
  it("uses a 30bps round-trip cost of 0.003", () => {
    expect(ESTIMATED_FEE_BPS).toBe(20);
    expect(ESTIMATED_SLIPPAGE_BPS).toBe(10);
    expect(ESTIMATED_ROUND_TRIP_COST_BPS).toBe(30);
    expect(ESTIMATED_ROUND_TRIP_COST_RATIO).toBe(0.003);
    expect(ESTIMATED_FEE_BPS + ESTIMATED_SLIPPAGE_BPS).toBe(
      ESTIMATED_ROUND_TRIP_COST_BPS,
    );
  });
});

describe("estimateSingleFundingPayback", () => {
  it("returns one day when 24h net equals 30bps", () => {
    const estimate = estimateSingleFundingPayback({ cumulative24h: 0.3 });
    expect(estimate.status).toBe("ready");
    expect(estimate.paybackMinutes).toBeCloseTo(1_440);
    expect(formatFundingPayback(estimate)).toBe("1 天");
  });

  it("does not take abs of a negative 24h cumulative", () => {
    const estimate = estimateSingleFundingPayback({ cumulative24h: -0.3 });
    expect(estimate.status).toBe("non_positive_carry");
    expect(estimate.paybackMinutes).toBeNull();
    expect(formatFundingPayback(estimate)).toBe("当前24h累计收益无法覆盖成本");
  });

  it("computes when history completeness is absent", () => {
    expect(
      estimateSingleFundingPayback({ cumulative24h: 0.3 }).paybackMinutes,
    ).toBeCloseTo(1_440);
    expect(
      estimateDirectionalFundingPayback(
        {
          role: "perpetual",
          status: "ready",
          cumulative24h: 0.3,
          stale: false,
        },
        { role: "spot" },
      ).status,
    ).toBe("non_positive_carry");
  });

  it("ignores current and next funding rates", () => {
    const estimate = estimateSingleFundingPayback({
      cumulative24h: 0.3,
    });
    expect(estimate.paybackMinutes).toBeCloseTo(1_440);
    expect(estimate.costBps).toBe(ESTIMATED_ROUND_TRIP_COST_BPS);
  });

  it("treats zero net as unable to cover cost", () => {
    const estimate = estimateSingleFundingPayback({ cumulative24h: 0 });
    expect(estimate.status).toBe("non_positive_carry");
    expect(formatFundingPayback(estimate)).toBe("当前24h累计收益无法覆盖成本");
  });

  it("marks stale quotes without a payback", () => {
    const estimate = estimateSingleFundingPayback({
      cumulative24h: 0.3,
      stale: true,
    });
    expect(estimate.status).toBe("stale");
    expect(formatFundingPayback(estimate)).toBe("行情过期，暂不估算");
  });
});

describe("estimateCrossFundingPayback", () => {
  it("converts annualized 24h spread back to the 24h ratio", () => {
    const fromSpread = estimateCrossFundingPayback({
      spread24hAnnualized: 0.3 * 365,
    });
    const fromLegs = estimateDirectionalFundingPayback(
      { role: "spot" },
      {
        role: "perpetual",
        status: "ready",
        cumulative24h: 0.3,
        stale: false,
      },
    );
    expect(fromSpread.paybackMinutes).toBeCloseTo(1_440);
    expect(fromLegs.paybackMinutes).toBeCloseTo(fromSpread.paybackMinutes ?? 0);
  });
});

describe("estimateDirectionalFundingPayback", () => {
  it("subtracts 24h cumulatives for Ask without using intervals", () => {
    const estimate = estimateDirectionalFundingPayback(
      {
        role: "perpetual",
        status: "ready",
        cumulative24h: 0.1,
        stale: false,
      },
      {
        role: "perpetual",
        status: "ready",
        cumulative24h: 0.4,
        stale: false,
      },
    );
    expect(percentDisplayToRatio(0.4) - percentDisplayToRatio(0.1)).toBeCloseTo(
      0.003,
    );
    expect(estimate.paybackMinutes).toBeCloseTo(1_440);
  });

  it("only subtracts 24h cumulatives for mixed 1h/4h legs", () => {
    const estimate = estimateDirectionalFundingPayback(
      {
        role: "perpetual",
        status: "ready",
        cumulative24h: 0.1,
        stale: false,
      },
      {
        role: "perpetual",
        status: "ready",
        cumulative24h: 0.4,
        stale: false,
      },
    );
    expect(estimate.annualizedRatio).toBeCloseTo(0.003 * 365);
    expect(estimate.paybackMinutes).toBeCloseTo(1_440);
  });

  it("treats a spot leg as zero", () => {
    const estimate = estimateDirectionalFundingPayback(
      { role: "spot" },
      {
        role: "perpetual",
        status: "ready",
        cumulative24h: 0.3,
        stale: false,
      },
    );
    expect(estimate.paybackMinutes).toBeCloseTo(1_440);
  });

  it("does not treat a missing perpetual as zero", () => {
    const estimate = estimateDirectionalFundingPayback(
      {
        role: "perpetual",
        status: "missing",
      },
      {
        role: "perpetual",
        status: "ready",
        cumulative24h: 0.3,
        stale: false,
      },
    );
    expect(estimate.status).toBe("rate_unavailable");
    expect(estimate.paybackMinutes).toBeNull();
    expect(formatFundingPayback(estimate)).toBe("资金费数据不可用");
  });

  it("marks spot/spot as not applicable", () => {
    const estimate = estimateDirectionalFundingPayback(
      { role: "spot" },
      { role: "spot" },
    );
    expect(estimate.status).toBe("not_applicable");
    expect(formatFundingPayback(estimate)).toBe("不适用");
  });
});

describe("fundingPaybackLegFromState", () => {
  it("maps a missing perpetual lookup without defaulting to zero", () => {
    expect(
      fundingPaybackLegFromState("perpetual", {
        missing: true,
        fresh: true,
      }),
    ).toEqual({ role: "perpetual", status: "missing" });
  });

  it("maps spot without reading lookup data", () => {
    expect(fundingPaybackLegFromState("spot")).toEqual({ role: "spot" });
  });
});

describe("formatFundingPayback", () => {
  it("formats hours and multi-day values", () => {
    expect(
      formatFundingPayback({
        status: "ready",
        paybackMinutes: 360,
        costBps: 30,
        annualizedRatio: 1,
      }),
    ).toBe("6 小时");
    expect(
      formatFundingPayback({
        status: "ready",
        paybackMinutes: 3_120,
        costBps: 30,
        annualizedRatio: 1,
      }),
    ).toBe("2 天 4 小时");
  });

  it("caps extremely long estimates", () => {
    const estimate = {
      status: "ready" as const,
      paybackMinutes: 366 * 1_440,
      costBps: 30 as const,
      annualizedRatio: 1,
    };
    expect(formatFundingPayback(estimate)).toBe(">365天");
    expect(fundingPaybackTitle(estimate)).toContain("30bps");
    expect(fundingPaybackTitle(estimate)).toContain("366 天");
  });
});
