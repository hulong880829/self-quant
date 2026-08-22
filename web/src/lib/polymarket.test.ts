import { describe, expect, it } from "vitest";

import {
  getDefaultPolymarketAssetId,
  getDefaultPolymarketPeriodId,
  getPolymarketSnapshot,
  polymarketAssets,
  polymarketPeriods,
  polymarketPositions,
} from "./polymarket";

describe("polymarket static data", () => {
  it("defaults to BTC and the 5 minute period", () => {
    expect(getDefaultPolymarketAssetId()).toBe("BTC");
    expect(getDefaultPolymarketPeriodId()).toBe("5m");
    const snapshot = getPolymarketSnapshot();
    expect(snapshot.assetId).toBe("BTC");
    expect(snapshot.periodId).toBe("5m");
  });

  it("keeps asset and period ids unique", () => {
    const assetIds = polymarketAssets.map((asset) => asset.id);
    const periodIds = polymarketPeriods.map((period) => period.id);
    expect(new Set(assetIds).size).toBe(assetIds.length);
    expect(assetIds).toEqual([
      "BTC",
      "ETH",
      "SOL",
      "XRP",
      "DOGE",
      "HYPE",
      "BNB",
    ]);
    expect(new Set(periodIds).size).toBe(periodIds.length);
  });

  it("provides aligned three-line price series for every asset and period", () => {
    for (const asset of polymarketAssets) {
      for (const period of polymarketPeriods) {
        const snapshot = getPolymarketSnapshot(period.id, asset.id);
        expect(snapshot.priceSeries.length).toBeGreaterThan(10);
        for (const point of snapshot.priceSeries) {
          expect(point.open).toBe(snapshot.openPrice);
          expect(Number.isFinite(point.chainlink)).toBe(true);
          expect(Number.isFinite(point.fairPrice)).toBe(true);
        }
      }
    }
  });

  it("keeps headline prices aligned with the latest chart point", () => {
    for (const asset of polymarketAssets) {
      for (const period of polymarketPeriods) {
        const snapshot = getPolymarketSnapshot(period.id, asset.id);
        const last = snapshot.priceSeries.at(-1);
        expect(last).toBeDefined();
        expect(snapshot.chainlinkPrice).toBe(last!.chainlink);
        expect(snapshot.fairPrice).toBe(last!.fairPrice);
      }
    }
  });

  it("looks up snapshots by asset and period id", () => {
    expect(getPolymarketSnapshot("15m", "ETH").title).toContain("ETH");
    expect(getPolymarketSnapshot("5m", "SOL").assetId).toBe("SOL");
    expect(() => getPolymarketSnapshot("bad" as "5m", "BTC")).toThrow(
      /unknown polymarket period/,
    );
    expect(() => getPolymarketSnapshot("5m", "bad" as "BTC")).toThrow(
      /unknown polymarket asset/,
    );
  });

  it("provides scrollable mock positions with required fields", () => {
    expect(polymarketPositions.length).toBeGreaterThan(5);
    for (const position of polymarketPositions) {
      expect(position.market).toBeTruthy();
      expect(position.avgPriceCents).toBeGreaterThan(0);
      expect(position.currentPriceCents).toBeGreaterThan(0);
      expect(position.tradeAmountUsd).toBeGreaterThan(0);
      expect(Number.isFinite(position.profitUsd)).toBe(true);
      expect(position.valueUsd).toBeGreaterThan(0);
    }
  });
});
