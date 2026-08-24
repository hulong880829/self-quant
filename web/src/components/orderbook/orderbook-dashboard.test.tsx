// @vitest-environment jsdom

import type * as React from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const orderbookContext = vi.hoisted(() => ({
  value: {} as Record<string, unknown>,
}));

vi.mock("./orderbook-provider", () => ({
  OrderbookProvider: ({ children }: { children: React.ReactNode }) => children,
  useOrderbook: () => orderbookContext.value,
}));

import { DashboardContent, SpreadChart } from "./orderbook-dashboard";

afterEach(cleanup);

describe("SpreadChart", () => {
  it("positions points by real time, labels Beijing time, and breaks gaps", () => {
    const startMs = Date.UTC(2026, 7, 15, 0);
    const endMs = Date.UTC(2026, 7, 15, 4);
    const { container } = render(
      <SpreadChart
        startMs={startMs}
        endMs={endMs}
        points={[
          { timestamp: "2026-08-15T00:00:00.000Z", spreadBps: 1 },
          { timestamp: "2026-08-15T01:00:00.000Z", spreadBps: 2 },
          { timestamp: "2026-08-15T02:00:00.000Z", spreadBps: null },
          { timestamp: "2026-08-15T03:00:00.000Z", spreadBps: 1.5 },
        ]}
      />,
    );

    expect(container.textContent).toContain("08:00");
    expect(container.textContent).toContain("12:00");
    expect(
      container.querySelectorAll('path[stroke="var(--primary)"]'),
    ).toHaveLength(2);
  });
});

describe("DashboardContent market filters", () => {
  it("shows the product selector before symbols and filters market options", () => {
    const spotBTC = {
      profile: "agg_spot_usdt_binance-bitget-bybit-gate-okx",
      symbol: "BTCUSDT",
      baseAsset: "BTC",
      quoteAsset: "USDT",
      priceScale: 2,
      quantityScale: 3,
      hasOrderBook: true,
    };
    const selectProduct = vi.fn();
    orderbookContext.value = {
      markets: [
        spotBTC,
        { ...spotBTC, symbol: "ETHUSDT", baseAsset: "ETH" },
        {
          ...spotBTC,
          profile: "agg_perp_usdt_binance-bitget-bybit-gate-okx",
          symbol: "SOLUSDT",
          baseAsset: "SOL",
        },
      ],
      selectedMarket: spotBTC,
      selectedProduct: "SPOT",
      selectProduct,
      selectMarket: vi.fn(),
      levels: [],
      automaticIncrement: null,
      history: [],
      distribution: [],
      historyCoverage: 0,
      historyGapCount: 0,
      historyStartMs: null,
      historyEndMs: null,
      loading: true,
      error: null,
      historyLoading: false,
      historyError: null,
      connection: "live",
      stale: false,
      lastUpdateAt: null,
      retry: vi.fn(),
    };

    const { container } = render(<DashboardContent />);
    const selects = container.querySelectorAll("select");
    expect(selects[0]?.value).toBe("SPOT");
    expect(selects[1]?.textContent).toContain("BTCUSDT");
    expect(selects[1]?.textContent).toContain("ETHUSDT");
    expect(selects[1]?.textContent).not.toContain("SOLUSDT");

    fireEvent.change(screen.getByLabelText("合约类型"), {
      target: { value: "PERPETUAL" },
    });
    expect(selectProduct).toHaveBeenCalledWith("PERPETUAL");
  });
});
