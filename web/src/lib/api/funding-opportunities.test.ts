import { afterEach, describe, expect, it, vi } from "vitest";

import {
  fetchFundingOpportunities,
  mapFundingOpportunitiesResponse,
} from "./funding-opportunities";

const leg = (exchange: string) => ({
  exchange,
  exchangeSymbol: "BTCUSDT",
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
      period: "1h",
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
      modelState: "ready",
      sourceUpdatedAt: "2026-08-22T12:00:00Z",
      stale: false,
    }],
    meta: {
      total: 1,
      snapshotVersion: "rank-1",
      serverTime: "2026-08-22T12:00:01Z",
      calculatedAt: "2026-08-22T12:00:00Z",
      stale: false,
    },
  };
}

afterEach(() => vi.unstubAllGlobals());

describe("funding opportunity ranking API", () => {
  it("maps ratios, bps, legs and ranking metadata", () => {
    const snapshot = mapFundingOpportunitiesResponse(responseBody());
    expect(snapshot.data[0]).toMatchObject({
      rank: 1,
      period: "1h",
      periodExpectedReturn: 0.12,
      combinedExpectedAnnualized: 15,
      profitProbability: 80,
      currentExecutableSpreadBps: 4.2,
      longLeg: { exchange: "Binance", fundingRate: 0.01 },
      shortLeg: { exchange: "OKX" },
    });
    expect(snapshot.meta).toMatchObject({ snapshotVersion: "rank-1", stale: false });
  });

  it("sends server filters and ETag, then handles 304 without parsing", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(null, { status: 304, headers: { ETag: '"rank-1"' } }),
    );
    vi.stubGlobal("fetch", fetchMock);
    await expect(fetchFundingOpportunities({
      period: "4h",
      minPositionNotional: 1_000_000,
      minDailyVolume: 2_000_000,
    }, '"rank-1"')).resolves.toEqual({ status: "unchanged", etag: '"rank-1"' });
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/funding-opportunities?period=4h&minLegNotionalUsd=1000000&minLegVolume24hUsd=2000000&limit=200",
      expect.objectContaining({
        headers: {
          Accept: "application/json",
          "If-None-Match": '"rank-1"',
        },
      }),
    );
  });
});
