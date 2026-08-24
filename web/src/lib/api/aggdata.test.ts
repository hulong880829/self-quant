import { afterEach, describe, expect, it, vi } from "vitest";

import {
  aggdataMarketKey,
  aggdataProductFromProfile,
  decodeFairPriceMessage,
  decodeAggdataFrame,
  fetchAggdataHistory,
  fetchAggdataSnapshot,
  fixedToSafeNumber,
  mapFairPriceHistoryResponse,
  mapHistoryResponse,
  mapMarketsResponse,
  mapSnapshotResponse,
  resolveFairPriceMarket,
} from "./aggdata";

afterEach(() => vi.unstubAllGlobals());

const market = {
  profile: "binance-okx",
  symbol: "BTCUSDT",
  baseAsset: "BTC",
  quoteAsset: "USDT",
  priceScale: 2,
  quantityScale: 3,
  hasOrderBook: true,
};

describe("aggdata REST mapping", () => {
  it("maps the Go market schema", () => {
    expect(
      mapMarketsResponse({
        markets: [{
          profile: "binance-okx",
          symbol: "BTCUSDT",
          base: "BTC",
          quote: "USDT",
          price_scale: 2,
          quantity_scale: 3,
          has_order_book: true,
        }],
      }),
    ).toEqual([market]);
  });

  it("builds a stable profile-and-symbol identity and classifies products", () => {
    expect(aggdataMarketKey({
      profile: "AGG_SPOT_USDT_BINANCE",
      symbol: "btcusdt",
    })).toBe("agg_spot_usdt_binance::BTCUSDT");
    expect(aggdataProductFromProfile("agg_spot_usdt_binance")).toBe("SPOT");
    expect(aggdataProductFromProfile("agg_perp_usdt_binance")).toBe(
      "PERPETUAL",
    );
    expect(aggdataProductFromProfile("custom_profile")).toBeNull();
  });

  it("adds profile to snapshot and spread-history requests", async () => {
    const profiledMarket = {
      ...market,
      profile: "agg_perp_usdt_binance-okx",
    };
    const fetchMock = vi.fn()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          symbol: "BTCUSDT",
          orderbook: {
            sequence: "1",
            generation: "1",
            wall_ns: "1786512000000000000",
            bids: [],
            asks: [],
          },
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          start_ns: "1786512000000000000",
          end_ns: "1786512060000000000",
          resolution_ns: "60000000000",
          buckets: [],
          gaps: [],
          distribution_bps: [],
        }),
      });
    vi.stubGlobal("fetch", fetchMock);

    await fetchAggdataSnapshot(profiledMarket);
    await fetchAggdataHistory(profiledMarket);

    expect(String(fetchMock.mock.calls[0]?.[0])).toContain(
      "snapshot?profile=agg_perp_usdt_binance-okx&depth=50",
    );
    expect(String(fetchMock.mock.calls[1]?.[0])).toContain(
      "spread-history?profile=agg_perp_usdt_binance-okx&range=24h&type=gated",
    );
  });

  it("expands snapshot contributions without losing bigint precision", () => {
    const snapshot = mapSnapshotResponse(
      {
        symbol: "BTCUSDT",
        orderbook: {
          sequence: 42,
          generation: 9,
          wall_ns: "1786512000000000000",
          bids: [{
            price: { mantissa: "9007199254740993", scale: 2 },
            quantity: { mantissa: "1200", scale: 3 },
            contributions: [
              { venue: "binance", quantity: { mantissa: "700", scale: 3 } },
              { venue: "okx", quantity: { mantissa: "500", scale: 3 } },
            ],
          }],
          asks: [],
        },
      },
      market,
    );
    expect(snapshot.sequence).toBe(42n);
    expect(snapshot.generation).toBe(9n);
    expect(snapshot.levels).toEqual([
      {
        side: "bid",
        price: { mantissa: 9007199254740993n, scale: 2 },
        quantity: { mantissa: 700n, scale: 3 },
        exchange: "binance",
      },
      {
        side: "bid",
        price: { mantissa: 9007199254740993n, scale: 2 },
        quantity: { mantissa: 500n, scale: 3 },
        exchange: "okx",
      },
    ]);
  });

  it("maps minute buckets, gaps, and coverage without hourly collapsing", () => {
    const start = BigInt(Date.UTC(2026, 7, 12, 9)) * 1_000_000n;
    const minuteNS = 60_000_000_000n;
    const result = mapHistoryResponse({
      start_ns: String(start),
      end_ns: String(start + 3n * minuteNS),
      resolution_ns: String(minuteNS),
      buckets: [
        { start_ns: String(start), close: 2, coverage: 1 },
        { start_ns: String(start + 2n * minuteNS), close: 3, coverage: 0.5 },
      ],
      gaps: [{
        start_ns: String(start + minuteNS),
        end_ns: String(start + 2n * minuteNS),
      }],
      distribution_bps: [-2, 0, 3],
    });
    expect(result.points).toEqual([
      { timestamp: "2026-08-12T09:00:00.000Z", spreadBps: 2 },
      { timestamp: "2026-08-12T09:01:00.000Z", spreadBps: null },
      { timestamp: "2026-08-12T09:02:00.000Z", spreadBps: 3 },
    ]);
    expect(result.coverage).toBe(0.5);
    expect(result.gapCount).toBe(1);
    expect(result.distribution).toEqual([-2, 0, 3]);
  });
});

describe("aggdata fair price mapping", () => {
  it("decodes realtime JSON without converting the mantissa first", () => {
    const message = decodeFairPriceMessage(JSON.stringify({
      type: "data",
      channel: "fairprice",
      profile: "binance-okx",
      symbol: "BTCUSDT",
      model_id: "fp-v1:test",
      ring_epoch: "18446744073709551615",
      seq: "7",
      wall_ns: "1786512000000000000",
      price: { mantissa: "9007199254740993", scale: 5 },
      degraded: true,
      degraded_reasons: ["book_crossed"],
    }));
    expect(message.type).toBe("data");
    if (message.type !== "data") throw new Error("expected data message");
    expect(message.ringEpoch).toBe(18446744073709551615n);
    expect(message.priceFixed.mantissa).toBe(9007199254740993n);
    expect(message.price).toBe(Number("90071992547.40993"));
    expect(message.degradedReasons).toEqual(["book_crossed"]);
  });

  it("maps history and keeps the response model id", () => {
    const history = mapFairPriceHistoryResponse({
      profile: "binance-okx",
      symbol: "BTCUSDT",
      model_id: "fp-v1:test",
      resolution_ms: 1000,
      points: [{
        observed_at: "2026-08-13T13:40:01Z",
        source_wall_ns: "1786512001000000000",
        ring_epoch: "9",
        seq: "10",
        price: { mantissa: "6374667000", scale: 5 },
        degraded: false,
        degraded_reasons: [],
      }],
    });
    expect(history.points[0]).toMatchObject({
      price: 63746.67,
      modelId: "fp-v1:test",
      sequence: 10n,
    });
  });

  it("resolves an unambiguous configured market", () => {
    const second = { ...market, profile: "other-profile" };
    expect(resolveFairPriceMarket(
      [market, second],
      "BTC",
      "binance-okx",
      "USDT",
    )).toEqual(market);
    expect(resolveFairPriceMarket([market, second], "BTC", "", "USDT")).toBeNull();
  });

  it("formats fixed decimals through an exact decimal string", () => {
    expect(fixedToSafeNumber({
      mantissa: 9007199254740993n,
      scale: 5,
    })).toBe(Number("90071992547.40993"));
  });
});

describe("SQAB compact decoder", () => {
  it("decodes a backend-compatible full orderbook snapshot", () => {
    const symbol = new TextEncoder().encode("BTCUSDT");
    const bodyLength = symbol.length + 1 + 4 + 21 + 9;
    const buffer = new ArrayBuffer(42 + bodyLength);
    const view = new DataView(buffer);
    view.setUint32(0, 0x42415153, true);
    view.setUint8(4, 1);
    view.setUint8(6, 2);
    view.setUint16(8, 42, true);
    view.setUint32(10, bodyLength, true);
    view.setUint8(14, symbol.length);
    view.setUint8(15, 2);
    view.setUint8(16, 3);
    view.setUint8(17, 1);
    view.setBigUint64(18, 7n, true);
    view.setBigUint64(26, 9n, true);
    view.setBigUint64(34, 1_786_512_000_000_000_000n, true);
    let offset = 42;
    new Uint8Array(buffer, offset, symbol.length).set(symbol);
    offset += symbol.length;
    view.setUint8(offset, 2);
    offset += 1;
    view.setUint16(offset, 1, true);
    view.setUint16(offset + 2, 0, true);
    offset += 4;
    view.setBigInt64(offset, 6_500_123n, true);
    view.setBigInt64(offset + 8, 456n, true);
    view.setUint32(offset + 16, 1, true);
    view.setUint8(offset + 20, 1);
    offset += 21;
    view.setUint8(offset, 0);
    view.setBigInt64(offset + 1, 456n, true);

    expect(decodeAggdataFrame(buffer)).toEqual({
      kind: "snapshot",
      sequence: 7n,
      generation: 9n,
      timestampMs: 1_786_512_000_000n,
      priceScale: 2,
      quantityScale: 3,
      levels: [{
        side: "bid",
        price: { mantissa: 6_500_123n, scale: 2 },
        quantity: { mantissa: 456n, scale: 3 },
        exchange: "okx",
      }],
    });
  });

  it("rejects unknown wire versions", () => {
    const buffer = new ArrayBuffer(42);
    const view = new DataView(buffer);
    view.setUint32(0, 0x42415153, true);
    view.setUint8(4, 2);
    expect(() => decodeAggdataFrame(buffer)).toThrow(
      "unsupported SQAB wire version",
    );
  });
});
