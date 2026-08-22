// @vitest-environment jsdom

import * as React from "react";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { FundingOpportunity } from "@/types/market";

const opportunities: FundingOpportunity[] = [
  {
    id: "binance-btcusdt",
    exchange: "Binance",
    exchangeSymbol: "BTCUSDT",
    symbol: "BTCUSDT",
    baseAsset: "BTC",
    quoteAsset: "USDT",
    positionQuantity: 100,
    positionNotional: 6_000_000,
    dailyVolume: 100_000_000,
    annualizedRate: 12,
    currentFundingRate: 0.01,
    nextFundingRate: null,
    settlementIntervalHours: 8,
    nextSettlementAt: "2026-08-20T00:00:00Z",
    cumulative24h: 0.03,
    cumulative7d: 0.2,
    latestPrice: 60_000,
    priceChange24h: 1,
    updatedAt: "2026-08-19T15:00:00Z",
    fundingHistory: [],
    index: { name: "BINANCE_INDEX", value: 59_990, weight: 100 },
  },
  {
    id: "okx-ethusdt",
    exchange: "OKX",
    exchangeSymbol: "ETH-USDT-SWAP",
    symbol: "ETHUSDT",
    baseAsset: "ETH",
    quoteAsset: "USDT",
    positionQuantity: 1_000,
    positionNotional: 4_000_000,
    dailyVolume: 80_000_000,
    annualizedRate: 8,
    currentFundingRate: 0.008,
    nextFundingRate: null,
    settlementIntervalHours: 4,
    nextSettlementAt: "2026-08-20T00:00:00Z",
    cumulative24h: 0.02,
    cumulative7d: 0.1,
    latestPrice: 4_000,
    priceChange24h: 0.5,
    updatedAt: "2026-08-19T15:00:00Z",
    fundingHistory: [],
    index: { name: "OKX_INDEX", value: 3_999, weight: 100 },
  },
];

vi.mock("@/hooks/use-element-width", () => ({
  useElementWidth: () => [{ current: null }, 1400],
}));

vi.mock("@tanstack/react-virtual", () => ({
  useVirtualizer: ({ count }: { count: number }) => ({
    getVirtualItems: () =>
      Array.from({ length: count }, (_, index) => ({
        index,
        key: index,
        start: index * 56,
        end: (index + 1) * 56,
        size: 56,
      })),
    getTotalSize: () => count * 56,
  }),
}));

vi.mock("@/components/funding/funding-provider", () => ({
  useFundingSnapshot: () => ({
    snapshot: {
      data: opportunities,
      meta: {
        total: opportunities.length,
        snapshotVersion: "test",
        serverTime: "2026-08-19T15:00:00Z",
      },
      hasStaleSources: false,
    },
    spreadSnapshot: null,
    loading: false,
    refreshing: false,
    error: null,
    lastSuccessAt: Date.now(),
    retry: vi.fn(),
  }),
}));

vi.mock("@/lib/api/funding", async (importOriginal) => {
  const original = await importOriginal<typeof import("@/lib/api/funding")>();
  return { ...original, fetchFundingHistory: vi.fn(async () => []) };
});

vi.mock("@/components/funding/basis-spread-chart", () => ({
  BasisSpreadPanel: ({
    venue,
    baseAsset,
    quoteAsset,
  }: {
    venue: string;
    baseAsset: string;
    quoteAsset: string;
  }) => (
    <section aria-label="期现 Best Ask 价差">
      <h3>期现 Best Ask 价差</h3>
      <button type="button">1h</button>
      <span>{`${venue}-${baseAsset}-${quoteAsset}`}</span>
    </section>
  ),
}));

import { FundingDashboard } from "./funding-dashboard";

function clickAssetRow(asset: string) {
  const row = screen.getByText(asset).closest("tr");
  expect(row).not.toBeNull();
  fireEvent.click(row!);
  return row!;
}

describe("FundingDashboard single-exchange detail", () => {
  afterEach(cleanup);

  it("expands a basis chart under the selected row without opening a sheet overlay", () => {
    render(<FundingDashboard />);

    expect(screen.getByRole("button", { name: "跨所" })).not.toBeNull();
    expect(screen.queryByRole("button", { name: "跨所套利" })).toBeNull();
    expect(screen.queryByLabelText("期现 Best Ask 价差")).toBeNull();
    expect(screen.getByText("选择合约后查看详情")).not.toBeNull();

    clickAssetRow("ETH");

    expect(screen.getByRole("heading", { name: "ETHUSDT" })).not.toBeNull();
    expect(document.querySelector('[data-slot="sheet-overlay"]')).toBeNull();
    const workspace = document.querySelector("[data-workspace-panel]");
    expect(workspace?.className).toContain("grid-cols-[minmax(0,1fr)_380px]");
    expect(within(workspace as HTMLElement).getByText("历史资金费")).not.toBeNull();
    expect(screen.getByLabelText("期现 Best Ask 价差")).not.toBeNull();
    expect(screen.getByText("OKX-ETH-USDT")).not.toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "1h" }));
    expect(screen.getByRole("button", { name: "1h" })).not.toBeNull();
  });

  it("collapses the selected row on a second click and only expands one row", () => {
    render(<FundingDashboard />);

    clickAssetRow("ETH");
    expect(screen.getByText("OKX-ETH-USDT")).not.toBeNull();

    clickAssetRow("BTC");
    expect(screen.getByText("Binance-BTC-USDT")).not.toBeNull();
    expect(screen.queryByText("OKX-ETH-USDT")).toBeNull();

    clickAssetRow("BTC");
    expect(screen.queryByLabelText("期现 Best Ask 价差")).toBeNull();
    expect(screen.getByText("选择合约后查看详情")).not.toBeNull();
  });

  it("does not render the basis chart in cross-exchange mode", () => {
    render(<FundingDashboard />);
    fireEvent.click(screen.getByRole("button", { name: "跨所" }));
    expect(screen.queryByLabelText("期现 Best Ask 价差")).toBeNull();
    expect(screen.queryByText("期现 Best Ask 价差")).toBeNull();
  });
});
