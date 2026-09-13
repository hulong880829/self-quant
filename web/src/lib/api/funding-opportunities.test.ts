import { afterEach, describe, expect, it, vi } from "vitest";

import {
  fetchFundingOpportunities,
  mapFundingOpportunitiesResponse,
} from "./funding-opportunities";

const leg = (exchange: string) => ({
  exchange,
  exchangeSymbol: "BTCUSDT",
  globalSymbol: "BTCUSDT",
  baseAsset: "BTC",
  quoteAsset: "USDT",
  fundingRate: "0.0001",
  settlementIntervalHours: 8,
  nextFundingAt: "2026-08-23T00:00:00Z",
  positionNotional: "5000000",
  dailyVolume: "90000000",
  latestPrice: "60000",
  sourceUpdatedAt: "2026-08-22T12:00:00Z",
  stale: false,
});

function responseBody() {
  return {
    data: [{
      id: "btc-binance-okx-1h",
      rank: 1,
      symbol: "BTCUSDT",
      baseAsset: "BTC",
      quoteAsset: "USDT",
      period: "8h",
      longLeg: leg("Binance"),
      shortLeg: leg("OKX"),
      currentMidSpreadBps: "3.1",
      currentExecutableSpreadBps: "4.2",
      targetSpreadBps: "1.5",
      periodExpectedReturn: "0.0012",
      fundingExpectedAnnualized: "0.10",
      spreadExpectedAnnualized: "0.05",
      combinedExpectedAnnualized: "0.15",
      firstPassageProbability: "0.75",
      profitProbability: "0.8",
      expectedExitMinutes: "42",
      p5Return: "-0.0005",
      minPositionNotional: "5000000",
      minDailyVolume: "90000000",
      coverage: "0.95",
      confidence: "0.9",
      modelState: "replay_7d",
      sampleCount: 150,
      expectedPaybackMinutes: "75",
      paybackStatus: "ready",
      sourceUpdatedAt: "2026-08-22T12:00:00Z",
      stale: false,
    }],
    meta: {
      total: 1,
      snapshotVersion: "rank-1",
      serverTime: "2026-08-22T12:00:01Z",
      calculatedAt: "2026-08-22T12:00:00Z",
      stale: false,
      status: "ready",
      lastSuccessfulAt: "2026-08-22T12:00:00Z",
      dataThrough: "2026-08-22T11:59:00Z",
      generation: 4,
    },
  };
}

afterEach(() => vi.unstubAllGlobals());

describe("funding opportunity ranking API", () => {
  it("maps ratios, bps, legs and ranking metadata", () => {
    const snapshot = mapFundingOpportunitiesResponse(responseBody());
    expect(snapshot.data[0]).toMatchObject({
      rank: 1,
      period: "8h",
      periodExpectedReturn: 0.12,
      combinedExpectedAnnualized: 15,
      profitProbability: 80,
      currentExecutableSpreadBps: 4.2,
      sampleCount: 150,
      expectedPaybackMinutes: 75,
      paybackStatus: "ready",
      longLeg: { exchange: "Binance", fundingRate: 0.01 },
      shortLeg: { exchange: "OKX" },
    });
    expect(snapshot.meta).toMatchObject({
      snapshotVersion: "rank-1",
      stale: false,
      status: "ready",
      generation: 4,
      dataThrough: "2026-08-22T11:59:00Z",
    });
  });

  it("accepts all three DEX venues in ranking detail rows", () => {
    for (const [longExchange, shortExchange] of [
      ["Hyperliquid", "Aster"],
      ["Lighter", "Binance"],
    ]) {
      const body = responseBody();
      body.data[0]!.longLeg = leg(longExchange);
      body.data[0]!.shortLeg = leg(shortExchange);
      const item = mapFundingOpportunitiesResponse(body).data[0]!;
      expect(item.longLeg.exchange).toBe(longExchange);
      expect(item.shortLeg.exchange).toBe(shortExchange);
    }
  });

  it("rejects Entropy ranking legs", () => {
    const body = responseBody();
    body.data[0]!.longLeg = leg("Entropy");
    expect(() => mapFundingOpportunitiesResponse(body)).toThrow("不是受支持的交易所");
  });

  it("sends server filters and ETag, then handles 304 without parsing", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(null, { status: 304, headers: { ETag: '"rank-1"' } }),
    );
    vi.stubGlobal("fetch", fetchMock);
    await expect(fetchFundingOpportunities({
      period: "24h",
      minPositionNotional: 1_000_000,
      minDailyVolume: 2_000_000,
    }, '"rank-1"')).resolves.toEqual({ status: "unchanged", etag: '"rank-1"' });
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/funding-opportunities?period=24h&minLegNotionalUsd=1000000&minLegVolume24hUsd=2000000&limit=100",
      expect.objectContaining({
        headers: {
          Accept: "application/json",
          "If-None-Match": '"rank-1"',
        },
      }),
    );
  });
});
