import { describe, expect, it } from "vitest";

import {
  annualize24h,
  annualize7d,
  formatPaybackPeriod,
  resolveFundingRate,
} from "./market-format";

describe("resolveFundingRate", () => {
  it("prefers next funding rate", () => {
    expect(resolveFundingRate(0.02, 0.01)).toBe(0.02);
  });

  it("falls back to current funding rate", () => {
    expect(resolveFundingRate(null, 0.01)).toBe(0.01);
  });

  it("returns null when both rates are missing", () => {
    expect(resolveFundingRate(null, null)).toBeNull();
  });
});

describe("formatPaybackPeriod", () => {
  it("formats ready periods using minutes, hours, and days", () => {
    expect(formatPaybackPeriod("ready", 35)).toBe("35 分钟");
    expect(formatPaybackPeriod("ready", 150)).toBe("2 小时 30 分钟");
    expect(formatPaybackPeriod("ready", 3_000)).toBe("2 天 2 小时");
  });

  it("formats terminal payback states", () => {
    expect(formatPaybackPeriod("never", null)).toBe("无法回本");
    expect(formatPaybackPeriod("insufficient_sample", null)).toBe("样本不足");
  });
});

describe("funding window annualization", () => {
  it("annualizes the 24 hour cumulative rate", () => {
    expect(annualize24h(0.03)).toBeCloseTo(10.95);
  });

  it("annualizes the 7 day cumulative rate", () => {
    expect(annualize7d(0.21)).toBeCloseTo(10.95);
  });
});
