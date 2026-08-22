import { describe, expect, it } from "vitest";

import {
  computeChartMinSpan,
  computeYDomain,
  formatYTick,
} from "./polymarket-chart";

describe("Polymarket chart Y domain", () => {
  it("tracks the actual BTC window range", () => {
    const { yMin, yMax } = computeYDomain([64925, 64929, 64929], {
      minSpan: Math.max(64929 * 0.00005, 2),
    });

    expect(yMax - yMin).toBeCloseTo(4.96, 6);
    expect(yMin).toBeGreaterThan(64920);
    expect(yMax).toBeLessThan(64935);
  });

  it("keeps a useful range when the price is unchanged", () => {
    const { yMin, yMax } = computeYDomain([100, 100], { minSpan: 2 });

    expect(yMax - yMin).toBeCloseTo(2.48, 6);
    expect(yMin).toBeLessThan(100);
    expect(yMax).toBeGreaterThan(100);
  });

  it("scales the DOGE range to its price magnitude", () => {
    const values = [0.07123, 0.07124, 0.07122];
    const minSpan = computeChartMinSpan(values, 0.07123);
    const { yMin, yMax } = computeYDomain(values, { minSpan });

    expect(yMax - yMin).toBeLessThan(0.001);
    expect(yMin).toBeGreaterThanOrEqual(0);
    expect(yMin).toBeLessThan(Math.min(...values));
    expect(yMax).toBeGreaterThan(Math.max(...values));
  });

  it("scales the XRP range without introducing a negative axis", () => {
    const values = [1.0459, 1.0414, 1.043];
    const minSpan = computeChartMinSpan(values, 1.0459);
    const { yMin, yMax } = computeYDomain(values, { minSpan });

    expect(yMax - yMin).toBeGreaterThan(0.004);
    expect(yMax - yMin).toBeLessThan(0.006);
    expect(yMin).toBeGreaterThan(0);
  });

  it("uses a relative floor when a low-priced asset is unchanged", () => {
    const minSpan = computeChartMinSpan([0.07123, 0.07123], 0.07123);

    expect(minSpan).toBeCloseTo(0.07123 * 0.0002, 12);
  });

  it("shows more precision for narrow ranges", () => {
    expect(formatYTick(0.07123012, 0.00005)).toBe("0.07123012");
    expect(formatYTick(1.041412, 0.005)).toBe("1.041412");
    expect(formatYTick(1.03791, 0.5)).toBe("1.0379");
    expect(formatYTick(64929.123, 5)).toBe("64,929.12");
  });
});
