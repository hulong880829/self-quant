import { describe, expect, it } from "vitest";

import {
  computeChartXDomain,
  computeChartMinSpan,
  computeYDomain,
  fairPriceChartTimeMs,
  filterFairPricePointsToWindow,
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

describe("Polymarket chart market window", () => {
  const start = "2026-08-24T10:35:00Z";
  const end = "2026-08-24T10:40:00Z";

  it("anchors the x domain to the active market window", () => {
    const domain = computeChartXDomain(
      start,
      end,
      new Date("2026-08-24T10:37:00Z").getTime(),
    );
    expect(domain).toEqual({
      startMs: new Date(start).getTime(),
      endMs: new Date("2026-08-24T10:37:00Z").getTime(),
      spanMs: 120_000,
    });
  });

  it("uses source wall time and rejects a stale fair point", () => {
    const staleSource = BigInt(new Date("2026-08-24T09:59:59Z").getTime()) *
      1_000_000n;
    const point = {
      timestamp: "2026-08-24T10:36:00Z",
      sourceWallNS: staleSource.toString(),
      ringEpoch: "1",
      sequence: "1",
      modelId: "fp-v1",
      price: 77_554,
      degraded: false,
      degradedReasons: [],
    };
    expect(fairPriceChartTimeMs(point)).toBe(
      new Date("2026-08-24T09:59:59Z").getTime(),
    );
    expect(filterFairPricePointsToWindow(
      [point],
      start,
      end,
      new Date(end).getTime(),
    )).toEqual([]);
  });
});
