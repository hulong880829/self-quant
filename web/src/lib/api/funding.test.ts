import { afterEach, describe, expect, it, vi } from "vitest";

import {
  fetchFundingRates,
  fetchFundingSpreads,
  mapFundingDto,
  mapFundingHistoryResponse,
  mapFundingSpreadsResponse,
} from "./funding";

function fundingWire(overrides: Record<string, unknown> = {}) {
  return {
    id: "binance-btcusdt",
    exchange: "Binance",
    exchangeSymbol: "BTCUSDT",
    symbol: "BTCUSDT",
    baseAsset: "BTC",
    quoteAsset: "USDT",
    positionQuantity: "100",
    positionNotional: "6000000",
    dailyVolume: "120000000",
    annualizedRate: "0.1",
    currentFundingRate: null,
    nextFundingRate: null,
    settlementIntervalHours: 8,
    nextFundingAt: "2026-08-07T20:00:00Z",
    cumulative24h: "0.0003",
    cumulative7d: "0.0021",
    latestPrice: "60000",
    priceChange24h: "0.01",
    sourceUpdatedAt: "2026-08-07T12:00:00Z",
    stale: false,
    index: { name: "BINANCE_INDEX", value: "60000", weight: "100" },
    ...overrides,
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("funding list mapping", () => {
  it("accepts lightweight rows without history", () => {
    const item = mapFundingDto(fundingWire(), 0);
    expect(item.exchangeSymbol).toBe("BTCUSDT");
    expect(item.currentFundingRate).toBeNull();
    expect(item.fundingHistory).toEqual([]);
  });

  it("maps on-demand funding history", () => {
    const history = mapFundingHistoryResponse({
      data: [{ rate: "0.0001", settledAt: "2026-08-07T08:00:00Z" }],
      meta: { exchange: "binance", exchangeSymbol: "BTCUSDT", total: 1 },
    });
    expect(history).toEqual([
      { rate: 0.01, settledAt: "2026-08-07T08:00:00Z" },
    ]);
  });
});

describe("funding list ETag", () => {
  it("returns unchanged without parsing a 304 body", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(null, {
        status: 304,
        headers: { ETag: '"snapshot-1"' },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await expect(fetchFundingRates('"snapshot-1"')).resolves.toEqual({
      status: "unchanged",
      etag: '"snapshot-1"',
    });
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/funding-rates",
      expect.objectContaining({
        headers: {
          Accept: "application/json",
          "If-None-Match": '"snapshot-1"',
        },
      }),
    );
  });
});

describe("funding spread mapping and ETag", () => {
  const leg = (exchange: string, rate: string) => ({
    exchange,
    exchangeSymbol: "BTCUSDT",
    fundingRate: rate,
    settlementIntervalHours: exchange === "Binance" ? 8 : 1,
    nextFundingAt: "2026-08-07T20:00:00Z",
    positionNotional: "5000000",
    dailyVolume: "90000000",
    latestPrice: "60000",
    sourceUpdatedAt: "2026-08-07T12:00:00Z",
    stale: false,
  });

  it("strictly maps ratios, legs, and minimum liquidity", () => {
    const snapshot = mapFundingSpreadsResponse({
      data: [{
        id: "btcusdt:binance:okx",
        symbol: "BTCUSDT",
        baseAsset: "BTC",
        quoteAsset: "USDT",
        longLeg: leg("Binance", "0.0001"),
        shortLeg: leg("OKX", "0.0002"),
        spreadAnnualized: "0.7665",
        spread24hAnnualized: "0.0365",
        spread7dAnnualized: "-0.052",
        minPositionNotional: "5000000",
        minDailyVolume: "90000000",
        sourceUpdatedAt: "2026-08-07T12:00:00Z",
        stale: false,
      }],
      meta: {
        total: 1,
        snapshotVersion: "42",
        serverTime: "2026-08-07T12:00:01Z",
      },
    });
    expect(snapshot.data[0]).toMatchObject({
      spreadAnnualized: 76.64999999999999,
      spread24hAnnualized: 3.65,
      spread7dAnnualized: -5.2,
      minPositionNotional: 5_000_000,
      longLeg: { exchange: "Binance", fundingRate: 0.01 },
      shortLeg: { exchange: "OKX", fundingRate: 0.02 },
    });
  });

  it("sends the spread ETag and does not parse a 304 body", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(null, { status: 304, headers: { ETag: '"spreads-1"' } }),
    );
    vi.stubGlobal("fetch", fetchMock);
    await expect(fetchFundingSpreads('"spreads-1"')).resolves.toEqual({
      status: "unchanged",
      etag: '"spreads-1"',
    });
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/funding-spreads",
      expect.objectContaining({
        headers: {
          Accept: "application/json",
          "If-None-Match": '"spreads-1"',
        },
      }),
    );
  });
});
