// @vitest-environment jsdom

import * as React from "react";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const apiMocks = vi.hoisted(() => ({
  fetchMarkets: vi.fn(),
  fetchSnapshot: vi.fn(),
  fetchHistory: vi.fn(),
  decodeFrame: vi.fn(),
}));

vi.mock("@/lib/api/aggdata", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api/aggdata")>();
  return {
    ...actual,
    aggdataWebSocketUrl: () => "ws://aggdata.test/v1/stream",
    fetchAggdataMarkets: apiMocks.fetchMarkets,
    fetchAggdataSnapshot: apiMocks.fetchSnapshot,
    fetchAggdataHistory: apiMocks.fetchHistory,
    decodeAggdataFrame: apiMocks.decodeFrame,
  };
});

import {
  clearOrderbookCachesForTest,
  isNewerBookVersion,
  OrderbookProvider,
  useOrderbook,
} from "./orderbook-provider";

const market = {
  profile: "agg_spot_usdt_binance-bitget-bybit-gate-okx",
  symbol: "BTCUSDT",
  baseAsset: "BTC",
  quoteAsset: "USDT",
  priceScale: 1,
  quantityScale: 1,
  hasOrderBook: true,
};
const perpetualMarket = {
  ...market,
  profile: "agg_perp_usdt_binance-bitget-bybit-gate-okx",
};

const initialLevel = {
  side: "bid" as const,
  price: { mantissa: 100n, scale: 1 },
  quantity: { mantissa: 20n, scale: 1 },
  exchange: "binance",
};

const snapshot = {
  symbol: market.symbol,
  sequence: 10n,
  generation: 2n,
  timestamp: "2026-08-14T06:00:00.000Z",
  levels: [initialLevel],
};
const historyResult = {
  points: [{ timestamp: "2026-08-14T05:59:00.000Z", spreadBps: 1 }],
  distribution: [1],
  startMs: Date.parse("2026-08-13T06:00:00.000Z"),
  endMs: Date.parse("2026-08-14T06:00:00.000Z"),
  resolutionMs: 60_000,
  coverage: 0.75,
  gapCount: 2,
};

class MockWebSocket {
  static instances: MockWebSocket[] = [];

  binaryType = "";
  sent: string[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onerror: (() => void) | null = null;
  onclose: (() => void) | null = null;
  closed = false;

  constructor(public readonly url: string) {
    MockWebSocket.instances.push(this);
  }

  send(value: string) {
    this.sent.push(value);
  }

  close() {
    this.closed = true;
    this.onclose?.();
  }

  open() {
    this.onopen?.();
  }

  message(data: string | ArrayBuffer) {
    this.onmessage?.(new MessageEvent("message", { data }));
  }
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((nextResolve, nextReject) => {
    resolve = nextResolve;
    reject = nextReject;
  });
  return { promise, resolve, reject };
}

function Probe() {
  const value = useOrderbook();
  return (
    <div>
      <span data-testid="profile">{value.selectedMarket?.profile ?? ""}</span>
      <span data-testid="product">{value.selectedProduct}</span>
      <button type="button" onClick={() => value.selectProduct("PERPETUAL")}>
        perpetual
      </button>
      <span data-testid="loading">{String(value.loading)}</span>
      <span data-testid="levels">{value.levels.length}</span>
      <span data-testid="exchange">{value.levels[0]?.exchange ?? ""}</span>
      <span data-testid="history-loading">
        {String(value.historyLoading)}
      </span>
      <span data-testid="history-error">{value.historyError ?? ""}</span>
      <span data-testid="history-points">{value.history.length}</span>
      <span data-testid="connection">{value.connection}</span>
    </div>
  );
}

describe("OrderbookProvider", () => {
  beforeEach(() => {
    clearOrderbookCachesForTest();
    MockWebSocket.instances = [];
    apiMocks.fetchMarkets.mockReset().mockResolvedValue([market]);
    apiMocks.fetchSnapshot.mockReset().mockResolvedValue(snapshot);
    apiMocks.fetchHistory.mockReset();
    apiMocks.decodeFrame.mockReset();
    vi.stubGlobal("WebSocket", MockWebSocket);
    vi.stubGlobal(
      "requestAnimationFrame",
      (callback: FrameRequestCallback) =>
        window.setTimeout(() => callback(performance.now()), 0),
    );
    vi.stubGlobal("cancelAnimationFrame", (id: number) =>
      window.clearTimeout(id),
    );
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("shows the snapshot and connects before 24h history resolves", async () => {
    const history = deferred<{
      points: [];
      distribution: [];
    }>();
    apiMocks.fetchHistory.mockReturnValue(history.promise);

    render(
      <OrderbookProvider>
        <Probe />
      </OrderbookProvider>,
    );

    await waitFor(() => {
      expect(screen.getByTestId("loading").textContent).toBe("false");
      expect(screen.getByTestId("levels").textContent).toBe("1");
      expect(MockWebSocket.instances).toHaveLength(1);
    });
    expect(screen.getByTestId("history-loading").textContent).toBe("true");

    await act(async () => {
      history.resolve({ points: [], distribution: [] });
      await history.promise;
    });
    await waitFor(() =>
      expect(screen.getByTestId("history-loading").textContent).toBe("false"),
    );
  });

  it("keeps a live book when history fails", async () => {
    apiMocks.fetchHistory.mockRejectedValue(new Error("history offline"));

    render(
      <OrderbookProvider>
        <Probe />
      </OrderbookProvider>,
    );

    await waitFor(() => {
      expect(screen.getByTestId("levels").textContent).toBe("1");
      expect(screen.getByTestId("history-loading").textContent).toBe("false");
    });
    expect(screen.getByTestId("history-error").textContent).toContain(
      "history offline",
    );
    expect(MockWebSocket.instances).toHaveLength(1);
  });

  it("refreshes visible history every 60 seconds and keeps the last success", async () => {
    vi.useFakeTimers();
    try {
      apiMocks.fetchHistory
        .mockResolvedValueOnce(historyResult)
        .mockRejectedValueOnce(new Error("refresh offline"));
      render(
        <OrderbookProvider>
          <Probe />
        </OrderbookProvider>,
      );

      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(apiMocks.fetchHistory).toHaveBeenCalledTimes(1);
      expect(screen.getByTestId("history-points").textContent).toBe("1");

      await act(async () => {
        await vi.advanceTimersByTimeAsync(60_000);
      });
      expect(apiMocks.fetchHistory).toHaveBeenCalledTimes(2);
      expect(screen.getByTestId("history-points").textContent).toBe("1");
      expect(screen.getByTestId("history-error").textContent).toContain(
        "refresh offline",
      );
    } finally {
      vi.useRealTimers();
    }
  });

  it("coalesces realtime frames and ignores an old sequence", async () => {
    apiMocks.fetchHistory.mockResolvedValue({ points: [], distribution: [] });
    render(
      <OrderbookProvider>
        <Probe />
      </OrderbookProvider>,
    );
    await waitFor(() => expect(MockWebSocket.instances).toHaveLength(1));
    const socket = MockWebSocket.instances[0]!;
    act(() => socket.open());
    expect(JSON.parse(socket.sent[0]!)).toMatchObject({
      op: "subscribe",
      profile: market.profile,
      symbol: market.symbol,
      channel: "orderbook",
    });

    apiMocks.decodeFrame.mockReturnValueOnce({
      ...snapshot,
      sequence: 9n,
      timestampMs: 1_786_512_000_000n,
      levels: [{ ...initialLevel, exchange: "old" }],
    });
    act(() => socket.message(new ArrayBuffer(1)));
    await new Promise((resolve) => window.setTimeout(resolve, 5));
    expect(screen.getByTestId("exchange").textContent).toBe("binance");

    apiMocks.decodeFrame
      .mockReturnValueOnce({
        ...snapshot,
        sequence: 11n,
        timestampMs: 1_786_512_000_001n,
        levels: [{ ...initialLevel, exchange: "okx" }],
      })
      .mockReturnValueOnce({
        ...snapshot,
        sequence: 12n,
        timestampMs: 1_786_512_000_002n,
        levels: [{ ...initialLevel, exchange: "bybit" }],
      });
    act(() => {
      socket.message(new ArrayBuffer(1));
      socket.message(new ArrayBuffer(1));
    });
    await waitFor(() =>
      expect(screen.getByTestId("exchange").textContent).toBe("bybit"),
    );
  });

  it("closes the active socket on unmount", async () => {
    apiMocks.fetchHistory.mockResolvedValue({ points: [], distribution: [] });
    const view = render(
      <OrderbookProvider>
        <Probe />
      </OrderbookProvider>,
    );
    await waitFor(() => expect(MockWebSocket.instances).toHaveLength(1));
    const socket = MockWebSocket.instances[0]!;

    view.unmount();

    expect(socket.closed).toBe(true);
    expect(MockWebSocket.instances).toHaveLength(1);
  });

  it("reloads the snapshot once and reconnects after a server reset", async () => {
    apiMocks.fetchHistory.mockResolvedValue({ points: [], distribution: [] });
    apiMocks.fetchSnapshot
      .mockResolvedValueOnce(snapshot)
      .mockResolvedValueOnce({ ...snapshot, sequence: 20n });
    render(
      <OrderbookProvider>
        <Probe />
      </OrderbookProvider>,
    );
    await waitFor(() => expect(MockWebSocket.instances).toHaveLength(1));
    const firstSocket = MockWebSocket.instances[0]!;
    act(() => {
      firstSocket.open();
      firstSocket.message(JSON.stringify({ op: "reset" }));
    });

    await waitFor(() => {
      expect(apiMocks.fetchSnapshot).toHaveBeenCalledTimes(2);
      expect(MockWebSocket.instances).toHaveLength(2);
    });
    expect(firstSocket.closed).toBe(true);
  });

  it("keeps spot and perpetual state isolated by profile and symbol", async () => {
    apiMocks.fetchMarkets.mockResolvedValue([market, perpetualMarket]);
    apiMocks.fetchSnapshot.mockImplementation(async (selected) => ({
      ...snapshot,
      sequence: selected.profile === market.profile ? 10n : 20n,
    }));
    apiMocks.fetchHistory.mockResolvedValue(historyResult);
    render(
      <OrderbookProvider>
        <Probe />
      </OrderbookProvider>,
    );

    await waitFor(() => {
      expect(screen.getByTestId("profile").textContent).toBe(market.profile);
      expect(MockWebSocket.instances).toHaveLength(1);
    });
    act(() => screen.getByRole("button", { name: "perpetual" }).click());
    await waitFor(() => {
      expect(screen.getByTestId("profile").textContent).toBe(
        perpetualMarket.profile,
      );
      expect(screen.getByTestId("product").textContent).toBe("PERPETUAL");
      expect(MockWebSocket.instances).toHaveLength(2);
    });

    expect(apiMocks.fetchSnapshot).toHaveBeenLastCalledWith(
      perpetualMarket,
      expect.any(AbortSignal),
    );
    expect(apiMocks.fetchHistory).toHaveBeenLastCalledWith(
      perpetualMarket,
      expect.any(AbortSignal),
    );
    const secondSocket = MockWebSocket.instances[1]!;
    act(() => secondSocket.open());
    expect(JSON.parse(secondSocket.sent[0]!)).toMatchObject({
      profile: perpetualMarket.profile,
      symbol: perpetualMarket.symbol,
    });
  });
});

describe("isNewerBookVersion", () => {
  it("accepts a new generation or a higher sequence in the same generation", () => {
    expect(isNewerBookVersion(2n, 11n, 2n, 10n)).toBe(true);
    expect(isNewerBookVersion(3n, 1n, 2n, 10n)).toBe(true);
    expect(isNewerBookVersion(2n, 10n, 2n, 10n)).toBe(false);
    expect(isNewerBookVersion(2n, 9n, 2n, 10n)).toBe(false);
  });
});
