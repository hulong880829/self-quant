// @vitest-environment jsdom

import * as React from "react";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const polymarketMocks = vi.hoisted(() => ({
  fetchMarkets: vi.fn(),
  fetchSnapshot: vi.fn(),
}));
const aggdataMocks = vi.hoisted(() => ({
  fetchMarkets: vi.fn(),
  fetchHistory: vi.fn(),
}));

vi.mock("@/components/auth/auth-provider", () => ({
  useAuth: () => ({ status: "unauthenticated" }),
}));
vi.mock("@/lib/api/polymarket", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api/polymarket")>();
  return {
    ...actual,
    fetchPolymarketMarkets: polymarketMocks.fetchMarkets,
    fetchPolymarketSnapshot: polymarketMocks.fetchSnapshot,
  };
});
vi.mock("@/lib/api/aggdata", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api/aggdata")>();
  return {
    ...actual,
    aggdataWebSocketUrl: () => "ws://aggdata.test/v1/stream",
    fetchAggdataMarkets: aggdataMocks.fetchMarkets,
    fetchFairPriceHistory: aggdataMocks.fetchHistory,
  };
});

import type { PolymarketMarket, PolymarketMarketSnapshot } from "@/types/polymarket";
import { PolymarketProvider, usePolymarket } from "./polymarket-provider";

class MockWebSocket {
  static instances: MockWebSocket[] = [];

  sent: string[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  onclose: (() => void) | null = null;

  constructor(public readonly url: string) {
    MockWebSocket.instances.push(this);
  }

  send(value: string) {
    this.sent.push(value);
  }

  close() {
    this.onclose?.();
  }

  open() {
    this.onopen?.();
  }

  message(value: unknown) {
    this.onmessage?.(
      new MessageEvent("message", { data: JSON.stringify(value) }),
    );
  }
}

class MockEventSource {
  onerror: (() => void) | null = null;

  addEventListener() {}

  close() {}
}

function Probe() {
  const value = usePolymarket();
  return (
    <div>
      <span data-testid="status">{value.fairPriceStatus}</span>
      <span data-testid="current">{value.currentFairPrice?.sequence ?? ""}</span>
      <span data-testid="points">{value.fairPricePoints.length}</span>
      <span data-testid="last-point">
        {value.fairPricePoints.at(-1)?.sequence ?? ""}
      </span>
      <span data-testid="fair-error">{value.fairPriceError ?? ""}</span>
    </div>
  );
}

describe("PolymarketProvider fair price reset", () => {
  beforeEach(() => {
    const now = Date.now();
    const market: PolymarketMarket = {
      id: "btc-5m",
      conditionId: "condition",
      slug: "btc-updown-5m",
      asset: "BTC",
      period: "5m",
      title: "Bitcoin Up or Down",
      windowStart: new Date(now - 60_000).toISOString(),
      windowEnd: new Date(now + 240_000).toISOString(),
      upTokenId: "up",
      downTokenId: "down",
      tickSize: "0.01",
      negativeRisk: false,
      active: true,
    };
    const snapshot: PolymarketMarketSnapshot = {
      market,
      openPrice: 78_000,
      chainlinkPrice: 78_010,
      upBid: 0.49,
      upAsk: 0.51,
      downBid: 0.49,
      downAsk: 0.51,
      priceSeries: [],
      sourceUpdatedAt: new Date(now).toISOString(),
      stale: false,
      version: "1",
    };
    MockWebSocket.instances = [];
    polymarketMocks.fetchMarkets.mockReset().mockResolvedValue([market]);
    polymarketMocks.fetchSnapshot.mockReset().mockResolvedValue({
      snapshot,
      etag: "1",
    });
    aggdataMocks.fetchMarkets.mockReset().mockResolvedValue([{
      profile: "agg_spot_usdt_binance-bitget-bybit-gate-okx",
      symbol: "BTCUSDT",
      baseAsset: "BTC",
      quoteAsset: "USDT",
      priceScale: 2,
      quantityScale: 8,
      hasOrderBook: true,
    }]);
    aggdataMocks.fetchHistory.mockReset().mockResolvedValue({
      profile: "agg_spot_usdt_binance-bitget-bybit-gate-okx",
      symbol: "BTCUSDT",
      modelId: "fair-v1",
      resolutionMs: 1_000,
      points: [{
        timestamp: new Date(now - 10_000).toISOString(),
        sourceWallNS: BigInt(now - 10_000) * 1_000_000n,
        ringEpoch: 1n,
        sequence: 1n,
        modelId: "fair-v1",
        price: 78_005,
        priceFixed: { mantissa: 7_800_500n, scale: 2 },
        degraded: false,
        degradedReasons: [],
      }],
    });
    vi.stubGlobal("WebSocket", MockWebSocket);
    vi.stubGlobal("EventSource", MockEventSource);
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("keeps history through reset and recovers on a new sequence", async () => {
    render(
      <PolymarketProvider>
        <Probe />
      </PolymarketProvider>,
    );
    await waitFor(() => expect(MockWebSocket.instances).toHaveLength(1));
    const socket = MockWebSocket.instances[0]!;
    act(() => socket.open());

    const wallNS = (BigInt(Date.now() + 4_000) * 1_000_000n).toString();
    act(() => socket.message({
      type: "data",
      channel: "fairprice",
      profile: "agg_spot_usdt_binance-bitget-bybit-gate-okx",
      symbol: "BTCUSDT",
      model_id: "fair-v1",
      ring_epoch: "1",
      seq: "2",
      wall_ns: wallNS,
      price: { mantissa: "7800600", scale: 2 },
      degraded: false,
      degraded_reasons: [],
    }));
    await waitFor(() => {
      expect(screen.getByTestId("status").textContent).toBe("live");
      expect(screen.getByTestId("current").textContent).toBe("2");
      expect(screen.getByTestId("points").textContent).not.toBe("0");
      expect(screen.getByTestId("last-point").textContent).toBe("2");
    }, { timeout: 2_500 });

    act(() => socket.message({
      op: "reset",
      channel: "fairprice",
      profile: "agg_spot_usdt_binance-bitget-bybit-gate-okx",
      symbol: "BTCUSDT",
      reason: "source_stale",
    }));
    expect(screen.getByTestId("status").textContent).toBe("stale");
    expect(screen.getByTestId("current").textContent).toBe("");
    expect(screen.getByTestId("points").textContent).not.toBe("0");
    expect(screen.getByTestId("last-point").textContent).toBe("2");

    act(() => socket.message({
      type: "data",
      channel: "fairprice",
      profile: "agg_spot_usdt_binance-bitget-bybit-gate-okx",
      symbol: "BTCUSDT",
      model_id: "fair-v1",
      ring_epoch: "1",
      seq: "3",
      wall_ns: wallNS,
      price: { mantissa: "7800700", scale: 2 },
      degraded: false,
      degraded_reasons: [],
    }));
    await waitFor(() => {
      expect(screen.getByTestId("status").textContent).toBe("live");
      expect(screen.getByTestId("current").textContent).toBe("3");
      expect(screen.getByTestId("points").textContent).not.toBe("0");
      expect(screen.getByTestId("last-point").textContent).toBe("3");
    });

    const beyondWindowNS = (
      BigInt(Date.now() + 300_000) * 1_000_000n
    ).toString();
    act(() => socket.message({
      type: "data",
      channel: "fairprice",
      profile: "agg_spot_usdt_binance-bitget-bybit-gate-okx",
      symbol: "BTCUSDT",
      model_id: "fair-v1",
      ring_epoch: "1",
      seq: "4",
      wall_ns: beyondWindowNS,
      price: { mantissa: "7800800", scale: 2 },
      degraded: false,
      degraded_reasons: [],
    }));
    expect(screen.getByTestId("status").textContent).toBe("live");
    expect(screen.getByTestId("current").textContent).toBe("3");
    expect(screen.getByTestId("last-point").textContent).toBe("3");
    expect(screen.getByTestId("fair-error").textContent).toBe("out_of_window");
  });
});
