import { afterEach, describe, expect, it, vi } from "vitest";

import {
  fetchFundingHistory,
  fetchFundingRates,
  fetchFundingRatesLookup,
  fetchFundingSpreads,
  fundingRequestKey,
  mapFundingDto,
  mapFundingHistoryResponse,
  mapFundingRatesLookupResponse,
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
  it("accepts Aster and Lighter and optional coverage fields", () => {
    const aster = mapFundingDto(
      fundingWire({
        id: "aster-btcusdt",
        exchange: "Aster",
        history24hComplete: false,
        history7dComplete: true,
      }),
      0,
    );
    expect(aster.exchange).toBe("Aster");
    expect(aster.history24hComplete).toBe(false);
    expect(aster.history7dComplete).toBe(true);
    const legacy = mapFundingDto(fundingWire(), 0);
    expect(legacy.history24hComplete).toBeUndefined();
    expect(legacy.history7dComplete).toBeUndefined();
  });

  it("accepts lightweight rows without history", () => {
    const item = mapFundingDto(fundingWire(), 0);
    expect(item.exchangeSymbol).toBe("BTCUSDT");
    expect(item.currentFundingRate).toBeNull();
    expect(item.fundingHistory).toEqual([]);
    expect(item.venueContractType).toBe("PERPETUAL");
  });

  it("maps venue contract types", () => {
    expect(mapFundingDto(fundingWire({ venueContractType: "TRADIFI_PERPETUAL" }), 0).venueContractType).toBe(
      "TRADIFI_PERPETUAL",
    );
    expect(
      mapFundingDto(
        fundingWire({
          exchange: "Hyperliquid",
          exchangeSymbol: "xyz:ZHIPU",
          venueContractType: "HIP3",
        }),
        0,
      ).venueContractType,
    ).toBe("HIP3");
    const entropy = mapFundingDto(
      fundingWire({
        id: "entropy-io-anth",
        exchange: "Entropy",
        exchangeSymbol: "io:ANTH",
        symbol: "ANTHUSDC",
        baseAsset: "ANTH",
        quoteAsset: "USDC",
        venueContractType: "HIP3",
      }),
      0,
    );
    expect(entropy.exchange).toBe("Entropy");
    expect(entropy.exchangeSymbol).toBe("io:ANTH");
    expect(entropy.venueContractType).toBe("HIP3");
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

  it("encodes Entropy HIP-3 history symbols", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({
        data: [{ rate: "0.0001", settledAt: "2026-08-07T08:00:00Z" }],
        meta: { exchange: "entropy", exchangeSymbol: "io:ANTH", total: 1 },
      })),
    );
    vi.stubGlobal("fetch", fetchMock);
    await fetchFundingHistory("entropy", "io:ANTH");
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/funding-rates/entropy/io%3AANTH/history?limit=10",
      expect.objectContaining({ method: "GET" }),
    );
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

describe("funding lookup", () => {
  it("maps results aligned to request keys", () => {
    const requested = [
      {
        exchange: "binance",
        exchangeSymbol: "BTCUSDT",
        baseAsset: "BTC",
        quoteAsset: "USDT",
      },
      {
        exchange: "okx",
        exchangeSymbol: "MISSING",
        baseAsset: "BTC",
        quoteAsset: "USDT",
      },
    ];
    const mapped = mapFundingRatesLookupResponse(
      {
        results: [
          {
            key: {
              exchange: "binance",
              exchangeSymbol: "BTCUSDT",
              baseAsset: "BTC",
              quoteAsset: "USDT",
            },
            status: "hit",
            item: fundingWire(),
          },
          {
            key: {
              exchange: "okx",
              exchangeSymbol: "MISSING",
              baseAsset: "BTC",
              quoteAsset: "USDT",
            },
            status: "missing",
          },
        ],
        meta: {
          snapshotVersion: "9",
          serverTime: "2026-08-07T12:00:00Z",
        },
      },
      requested,
    );
    expect(mapped.results[0]).toEqual({
      key: "binance|btcusdt",
      status: "hit",
      item: expect.objectContaining({ exchange: "Binance", exchangeSymbol: "BTCUSDT" }),
    });
    expect(mapped.results[1]).toEqual({
      key: "okx|missing",
      status: "missing",
    });
    expect(mapped.snapshotVersion).toBe("9");
  });

  it("posts credentials and maps 401 to login required", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ error: "authentication required" }), {
        status: 401,
        headers: { "Content-Type": "application/json" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);
    await expect(
      fetchFundingRatesLookup([
        {
          exchange: "binance",
          exchangeSymbol: "BTCUSDT",
          baseAsset: "BTC",
          quoteAsset: "USDT",
        },
      ]),
    ).rejects.toThrow("请先登录");
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/funding-rates/lookup",
      expect.objectContaining({
        method: "POST",
        credentials: "include",
      }),
    );
    expect(fundingRequestKey({ exchange: "Binance", exchangeSymbol: "BTCUSDT" })).toBe(
      "binance|btcusdt",
    );
  });
});

describe("funding spread mapping and ETag", () => {
  const leg = (exchange: string, rate: string) => ({
    exchange,
    exchangeSymbol: "BTCUSDT",
    globalSymbol: "BTCUSDT",
    baseAsset: "BTC",
    quoteAsset: "USDT",
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
