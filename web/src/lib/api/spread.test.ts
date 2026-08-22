import { afterEach, describe, expect, it, vi } from "vitest";

import { fetchBasisSpreadHistory, mapBasisSpreadHistory } from "./spread";

function historyWire(overrides: Record<string, unknown> = {}) {
  return {
    venue: "binance",
    baseAsset: "BTC",
    quoteAsset: "USDT",
    canonicalSymbol: "BTCUSDT",
    range: "24h",
    resolutionSeconds: 60,
    availability: "available",
    asOf: "2026-08-22T07:00:00Z",
    points: [
      {
        ts: "2026-08-22T06:59:00Z",
        spreadBps: "12.5",
        spotAsk: "100",
        perpetualAsk: "100.125",
        samples: 2,
      },
    ],
    summary: {
      currentBps: "12.5",
      minBps: "8",
      maxBps: "15",
      avgBps: "11",
      coverage: "0.97",
    },
    ...overrides,
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("basis spread mapping", () => {
  it("maps decimal strings without treating them as percents", () => {
    const history = mapBasisSpreadHistory(historyWire());
    expect(history.points[0]?.spreadBps).toBe(12.5);
    expect(history.summary.coverage).toBe(0.97);
    expect(history.availability).toBe("available");
  });
});

describe("basis spread fetch", () => {
  it("returns unchanged without parsing a 304 body", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(null, {
        status: 304,
        headers: { ETag: '"spread-1"' },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);
    await expect(
      fetchBasisSpreadHistory("Binance", "btc", "usdt", "24h", '"spread-1"'),
    ).resolves.toEqual({
      status: "unchanged",
      etag: '"spread-1"',
    });
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/basis-spreads/binance/BTC/USDT/history?range=24h",
      expect.objectContaining({
        headers: {
          Accept: "application/json",
          "If-None-Match": '"spread-1"',
        },
      }),
    );
  });
});
