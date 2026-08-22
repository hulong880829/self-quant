import { describe, expect, it } from "vitest";

import {
  aggregateBookLevels,
  calculateSpreadCcdf,
  chooseVisibleIncrement,
  fillHistoryTimeline,
  fixed,
  formatFixed,
  generate125Increments,
  inferEffectiveTick,
  orderBookDisplayLevels,
  spreadChartDomain,
  type AggregatedBookLevel,
} from "./orderbook";

const levels: AggregatedBookLevel[] = [
  { side: "bid", price: fixed(9998n, 2), quantity: fixed(10n, 1), exchange: "A" },
  { side: "bid", price: fixed(10000n, 2), quantity: fixed(20n, 1), exchange: "B" },
  { side: "ask", price: fixed(10003n, 2), quantity: fixed(30n, 1), exchange: "A" },
  { side: "ask", price: fixed(10001n, 2), quantity: fixed(40n, 1), exchange: "B" },
];

describe("fixed decimal orderbook", () => {
  it("formats mantissa and scale without Number precision loss", () => {
    expect(formatFixed(fixed(9007199254740993123n, 4))).toBe(
      "900719925474099.3123",
    );
    expect(formatFixed(fixed(-1200n, 3), true)).toBe("-1.2");
  });

  it("ceil-buckets asks, floor-buckets bids and preserves contributions", () => {
    const asks = aggregateBookLevels(levels, "ask", fixed(10n, 2));
    const bids = aggregateBookLevels(levels, "bid", fixed(10n, 2));
    expect(asks.map((level) => formatFixed(level.price))).toEqual(["100.10"]);
    expect(bids.map((level) => formatFixed(level.price))).toEqual([
      "100.00",
      "99.90",
    ]);
    expect(asks[0].quantity).toEqual(fixed(70n, 1));
    expect(asks[0].contributions).toEqual([
      { exchange: "A", quantity: fixed(30n, 1) },
      { exchange: "B", quantity: fixed(40n, 1) },
    ]);
  });

  it("displays asks from high to best while keeping bids best-first", () => {
    const asks = aggregateBookLevels(levels, "ask", fixed(1n, 2));
    const bids = aggregateBookLevels(levels, "bid", fixed(1n, 2));
    expect(
      orderBookDisplayLevels(asks, "ask").map((level) =>
        formatFixed(level.price),
      ),
    ).toEqual(["100.03", "100.01"]);
    expect(
      orderBookDisplayLevels(bids, "bid").map((level) =>
        formatFixed(level.price),
      ),
    ).toEqual(["100.00", "99.98"]);
  });

  it("infers adjacent-price tick and generates 1/2/5 increments", () => {
    const tick = inferEffectiveTick(levels);
    expect(tick).toEqual(fixed(1n, 2));
    expect(
      generate125Increments(tick!, 6).map((increment) =>
        formatFixed(increment),
      ),
    ).toEqual([
      "0.01",
      "0.02",
      "0.05",
      "0.10",
      "0.20",
      "0.50",
    ]);
  });

  it("chooses an increment yielding the requested visible range", () => {
    const dense: AggregatedBookLevel[] = Array.from({ length: 40 }, (_, index) => [
      {
        side: "ask" as const,
        price: fixed(10_001n + BigInt(index), 2),
        quantity: fixed(1n, 0),
        exchange: "A",
      },
      {
        side: "bid" as const,
        price: fixed(9_999n - BigInt(index), 2),
        quantity: fixed(1n, 0),
        exchange: "A",
      },
    ]).flat();
    expect(
      formatFixed(
        chooseVisibleIncrement(
          dense,
          generate125Increments(fixed(1n, 2)),
        ),
      ),
    ).toBe("0.02");
  });
});

describe("24 hour history", () => {
  const start = Date.UTC(2026, 7, 12, 9);
  const minute = 60_000;

  it("retains every minute bucket and explicit gaps", () => {
    const points = fillHistoryTimeline(
      [
        { timestamp: "2026-08-12T09:00:00.000Z", spreadBps: 1.4 },
        { timestamp: "2026-08-12T09:02:00.000Z", spreadBps: 1.2 },
      ],
      start,
      start + 4 * minute,
      minute,
    );
    expect(points).toEqual([
      { timestamp: "2026-08-12T09:00:00.000Z", spreadBps: 1.4 },
      { timestamp: "2026-08-12T09:01:00.000Z", spreadBps: null },
      { timestamp: "2026-08-12T09:02:00.000Z", spreadBps: 1.2 },
      { timestamp: "2026-08-12T09:03:00.000Z", spreadBps: null },
    ]);
  });

  it("ignores gaps in the CCDF denominator", () => {
    expect(
      calculateSpreadCcdf(
        [
          { timestamp: "a", spreadBps: 1 },
          { timestamp: "b", spreadBps: null },
          { timestamp: "c", spreadBps: 2 },
        ],
        [1],
      ),
    ).toEqual([{ thresholdBps: 1, probability: 0.5 }]);
  });

  it("keeps negative spreads inside the chart domain", () => {
    const domain = spreadChartDomain([-14.06, -5.21, -1.73]);
    expect(domain).not.toBeNull();
    expect(domain!.minimum).toBeLessThan(-14.06);
    expect(domain!.maximum).toBeGreaterThan(-1.73);
  });

  it("keeps positive and mixed spreads inside the chart domain", () => {
    expect(spreadChartDomain([1, 2])).toEqual({
      minimum: 0,
      maximum: 2.15,
    });
    const mixed = spreadChartDomain([-2, 3]);
    expect(mixed!.minimum).toBeLessThan(-2);
    expect(mixed!.maximum).toBeGreaterThan(3);
  });
});
