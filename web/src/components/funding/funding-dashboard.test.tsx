// @vitest-environment jsdom

import * as React from "react";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { FundingOpportunity } from "@/types/market";

const virtualizerMocks = vi.hoisted(() => ({
  measure: vi.fn(),
}));

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
  {
    id: "binance-zhipuusdt",
    exchange: "Binance",
    exchangeSymbol: "ZHIPUUSDT",
    symbol: "ZHIPUUSDT",
    baseAsset: "ZHIPU",
    quoteAsset: "USDT",
    positionQuantity: 100,
    positionNotional: 2_000_000,
    dailyVolume: 5_000_000,
    annualizedRate: 20,
    currentFundingRate: 0.02,
    nextFundingRate: null,
    settlementIntervalHours: 8,
    nextSettlementAt: "2026-08-20T00:00:00Z",
    cumulative24h: 0.04,
    cumulative7d: 0.3,
    latestPrice: 10,
    priceChange24h: 0.1,
    updatedAt: "2026-08-19T15:00:00Z",
    venueContractType: "TRADIFI_PERPETUAL",
    fundingHistory: [],
    index: { name: "BINANCE_INDEX", value: 10, weight: 100 },
  },
  {
    id: "hyperliquid-xyz:zhipu",
    exchange: "Hyperliquid",
    exchangeSymbol: "xyz:ZHIPU",
    symbol: "ZHIPUUSDC",
    baseAsset: "ZHIPU",
    quoteAsset: "USDC",
    positionQuantity: 50,
    positionNotional: 2_000_000,
    dailyVolume: 4_000_000,
    annualizedRate: 18,
    currentFundingRate: 0.001,
    nextFundingRate: null,
    settlementIntervalHours: 1,
    nextSettlementAt: "2026-08-20T00:00:00Z",
    cumulative24h: 0.02,
    cumulative7d: 0.1,
    latestPrice: 10,
    priceChange24h: 0.1,
    updatedAt: "2026-08-19T15:00:00Z",
    venueContractType: "HIP3",
    fundingHistory: [],
    index: { name: "HYPERLIQUID_INDEX", value: 10, weight: 100 },
  },
];

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn() }),
}));

vi.mock("@/components/auth/auth-provider", () => ({
  useAuth: () => ({
    status: "authenticated",
    requireAuth: () => true,
  }),
}));

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
    measure: virtualizerMocks.measure,
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
    spreadSnapshot: {
      data: [
        {
          id: "BTCUSDT-binance-okx",
          symbol: "BTCUSDT",
          baseAsset: "BTC",
          quoteAsset: "USDT",
          longLeg: {
            exchange: "Binance",
            exchangeSymbol: "BTCUSDT",
            globalSymbol: "BTCUSDT",
            baseAsset: "BTC",
            quoteAsset: "USDT",
            fundingRate: 0.01,
            settlementIntervalHours: 8,
            nextSettlementAt: "2026-08-20T00:00:00Z",
            positionNotional: 5_000_000,
            dailyVolume: 20_000_000,
            latestPrice: 60_000,
            updatedAt: "2026-08-19T15:00:00Z",
            stale: false,
          },
          shortLeg: {
            exchange: "OKX",
            exchangeSymbol: "BTC-USDT-SWAP",
            globalSymbol: "BTCUSDT",
            baseAsset: "BTC",
            quoteAsset: "USDT",
            fundingRate: 0.02,
            settlementIntervalHours: 4,
            nextSettlementAt: "2026-08-20T00:00:00Z",
            positionNotional: 4_000_000,
            dailyVolume: 30_000_000,
            latestPrice: 60_010,
            updatedAt: "2026-08-19T15:00:00Z",
            stale: false,
          },
          spreadAnnualized: 10.95,
          spread24hAnnualized: 7.3,
          spread7dAnnualized: 5.2,
          minPositionNotional: 4_000_000,
          minDailyVolume: 20_000_000,
          updatedAt: "2026-08-19T15:00:00Z",
          stale: false,
        },
        {
          id: "ZHIPUUSDT-binance-hyperliquid",
          symbol: "ZHIPUUSDT",
          baseAsset: "ZHIPU",
          quoteAsset: "USDT",
          longLeg: {
            exchange: "Binance",
            exchangeSymbol: "ZHIPUUSDT",
            globalSymbol: "ZHIPUUSDT",
            baseAsset: "ZHIPU",
            quoteAsset: "USDT",
            fundingRate: 0.02,
            settlementIntervalHours: 8,
            nextSettlementAt: "2026-08-20T00:00:00Z",
            positionNotional: 2_000_000,
            dailyVolume: 5_000_000,
            latestPrice: 10,
            updatedAt: "2026-08-19T15:00:00Z",
            stale: false,
            venueContractType: "TRADIFI_PERPETUAL",
          },
          shortLeg: {
            exchange: "Hyperliquid",
            exchangeSymbol: "xyz:ZHIPU",
            globalSymbol: "ZHIPUUSDC",
            baseAsset: "ZHIPU",
            quoteAsset: "USDC",
            fundingRate: 0.001,
            settlementIntervalHours: 1,
            nextSettlementAt: "2026-08-20T00:00:00Z",
            positionNotional: 2_000_000,
            dailyVolume: 4_000_000,
            latestPrice: 10,
            updatedAt: "2026-08-19T15:00:00Z",
            stale: false,
            venueContractType: "HIP3",
          },
          spreadAnnualized: 15,
          spread24hAnnualized: 12,
          spread7dAnnualized: 8,
          minPositionNotional: 2_000_000,
          minDailyVolume: 4_000_000,
          updatedAt: "2026-08-19T15:00:00Z",
          stale: false,
        },
      ],
      meta: {
        total: 1,
        snapshotVersion: "test-spread",
        serverTime: "2026-08-19T15:00:00Z",
      },
      hasStaleSources: false,
    },
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
    compareVenue,
  }: {
    venue: string;
    baseAsset: string;
    quoteAsset: string;
    compareVenue?: string;
  }) => (
    <section aria-label={compareVenue ? "跨所 Best Ask 价差" : "期现 Best Ask 价差"}>
      <h3>{compareVenue ? "跨所 Best Ask 价差" : "期现 Best Ask 价差"}</h3>
      <button type="button">1h</button>
      <span>
        {compareVenue
          ? `${venue}/${compareVenue}-${baseAsset}-${quoteAsset}`
          : `${venue}-${baseAsset}-${quoteAsset}`}
      </span>
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
  afterEach(() => {
    cleanup();
    virtualizerMocks.measure.mockClear();
  });

  it("expands a basis chart under the selected row without opening a sheet overlay", () => {
    render(<FundingDashboard />);

    expect(screen.getByRole("button", { name: "跨所" })).not.toBeNull();
    expect(screen.queryByRole("button", { name: "跨所套利" })).toBeNull();
    expect(screen.queryByLabelText("期现 Best Ask 价差")).toBeNull();
    expect(screen.getByText("选择合约后查看详情")).not.toBeNull();

    clickAssetRow("ETH");

    expect(screen.getByRole("heading", { name: "ETHUSDT" })).not.toBeNull();
    expect(screen.getByRole("button", { name: "开启交易" })).not.toBeNull();
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

  it("expands a cross-exchange basis chart after clicking a spread row", () => {
    render(<FundingDashboard />);
    fireEvent.click(screen.getByRole("button", { name: "跨所" }));
    expect(screen.queryByLabelText("跨所 Best Ask 价差")).toBeNull();
    const row = screen.getByText("BTC/USDT").closest("tr");
    expect(row).not.toBeNull();
    fireEvent.click(row!);
    expect(screen.getByLabelText("跨所 Best Ask 价差")).not.toBeNull();
    expect(screen.getByText("OKX/Binance-BTC-USDT")).not.toBeNull();
    expect(screen.getByText("跨所资金费套利")).not.toBeNull();
  });

  it("requires both spread legs to be in the selected exchange set", () => {
    render(<FundingDashboard />);
    fireEvent.click(screen.getByRole("button", { name: "跨所" }));
    expect(screen.getByText("BTC/USDT")).not.toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Binance" }));
    expect(screen.queryByText("BTC/USDT")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "OKX" }));
    expect(screen.getByText("BTC/USDT")).not.toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "OKX" }));
    fireEvent.click(screen.getByRole("button", { name: "Hyperliquid" }));
    expect(screen.queryByText("BTC/USDT")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "OKX" }));
    expect(screen.getByText("BTC/USDT")).not.toBeNull();
  });

  it("filters single and spread rows by contract kind", () => {
    render(<FundingDashboard />);
    expect(screen.getByText("BTC")).not.toBeNull();
    expect(screen.getAllByText("ZHIPU").length).toBeGreaterThan(0);

    fireEvent.click(screen.getByRole("button", { name: "TradFi" }));
    expect(screen.queryByText("BTC")).toBeNull();
    expect(screen.queryByText("ETH")).toBeNull();
    expect(screen.getAllByText("ZHIPU").length).toBeGreaterThan(0);

    fireEvent.click(screen.getByRole("button", { name: "跨所" }));
    expect(screen.queryByText("BTC/USDT")).toBeNull();
    expect(screen.queryByText("ZHIPU/USDT")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "HIP-3" }));
    expect(screen.getByText("ZHIPU/USDT")).not.toBeNull();
    expect(screen.queryByText("BTC/USDT")).toBeNull();
    fireEvent.click(screen.getByText("ZHIPU/USDT").closest("tr")!);
    expect(screen.getByRole("button", { name: "开启交易" })).not.toBeNull();
  });

  it("measures only the visible list once after a mode change", async () => {
    render(<FundingDashboard />);
    await act(async () => {
      await new Promise((resolve) => requestAnimationFrame(() => resolve(null)));
    });
    expect(virtualizerMocks.measure).toHaveBeenCalledTimes(1);

    virtualizerMocks.measure.mockClear();
    fireEvent.click(screen.getByRole("button", { name: "跨所" }));
    await act(async () => {
      await new Promise((resolve) => requestAnimationFrame(() => resolve(null)));
    });
    expect(virtualizerMocks.measure).toHaveBeenCalledTimes(1);

    virtualizerMocks.measure.mockClear();
    fireEvent.click(screen.getByRole("button", { name: "单所" }));
    fireEvent.click(screen.getByRole("button", { name: "跨所" }));
    await act(async () => {
      await new Promise((resolve) => requestAnimationFrame(() => resolve(null)));
    });
    expect(virtualizerMocks.measure).toHaveBeenCalledTimes(1);
  });
});
