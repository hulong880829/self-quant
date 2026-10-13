// @vitest-environment jsdom

import * as React from "react";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { FundingOpportunity } from "@/types/market";

const virtualizerMocks = vi.hoisted(() => ({
  measure: vi.fn(),
}));

const fundingControls = vi.hoisted(() => ({
  tradefiError: null as string | null,
  useOverride: false,
  tradefiSnapshot: null as unknown,
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
    history24hComplete: false,
    fundingHistory: [],
    index: { name: "BINANCE_INDEX", value: 59_990, weight: 100 },
  },
  {
    id: "okx-solusdt",
    exchange: "OKX",
    exchangeSymbol: "SOL-USDT-SWAP",
    symbol: "SOLUSDT",
    baseAsset: "SOL",
    quoteAsset: "USDT",
    positionQuantity: 500,
    positionNotional: 3_000_000,
    dailyVolume: 40_000_000,
    annualizedRate: -6,
    currentFundingRate: -0.01,
    nextFundingRate: null,
    settlementIntervalHours: 8,
    nextSettlementAt: "2026-08-20T00:00:00Z",
    cumulative24h: -0.04,
    cumulative7d: -0.1,
    latestPrice: 150,
    priceChange24h: -0.2,
    updatedAt: "2026-08-19T15:00:00Z",
    fundingHistory: [],
    index: { name: "OKX_INDEX", value: 149, weight: 100 },
  },
  {
    id: "binance-adausdt",
    exchange: "Binance",
    exchangeSymbol: "ADAUSDT",
    symbol: "ADAUSDT",
    baseAsset: "ADA",
    quoteAsset: "USDT",
    positionQuantity: 10_000,
    positionNotional: 2_500_000,
    dailyVolume: 15_000_000,
    annualizedRate: 9,
    currentFundingRate: 0.01,
    nextFundingRate: null,
    settlementIntervalHours: 8,
    nextSettlementAt: "2026-08-20T00:00:00Z",
    cumulative24h: 0.3,
    cumulative7d: 0.8,
    latestPrice: 0.5,
    priceChange24h: 0.01,
    updatedAt: "2026-08-19T15:00:00Z",
    stale: true,
    fundingHistory: [],
    index: { name: "BINANCE_INDEX", value: 0.5, weight: 100 },
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
    tradefiError: fundingControls.tradefiError,
    tradefiSnapshot: fundingControls.useOverride
      ? fundingControls.tradefiSnapshot
      : {
      data: [
        {
          id: "AAPL-binance-AAPLUSDT",
          stockSymbol: "AAPL",
          spotExchange: "IBKR",
          conId: "265598",
          primaryExchange: "NASDAQ",
          localSymbol: "AAPL",
          tradingClass: "AAPL",
          exchange: "Binance",
          exchangeSymbol: "AAPLUSDT",
          baseAsset: "AAPL",
          quoteAsset: "USDT",
          fundingRate: 1,
          perpetualFundingAnnualized: 11,
          annualizedRate: 11,
          currentFundingRate: 1,
          nextFundingRate: null,
          settlementIntervalHours: 8,
          nextFundingAt: "2026-10-08T08:00:00Z",
          nextSettlementAt: "2026-10-08T08:00:00Z",
          volume24hBase: 10,
          turnover24hUsd: 5_000_000,
          dailyVolume: 5_000_000,
          positionQuantity: 100,
          positionNotional: 2_000_000,
          cumulative24h: 0.2,
          cumulative7d: 1,
          history24hComplete: false,
          history7dComplete: false,
          stale: true,
          updatedAt: "2026-10-08T01:00:00Z",
          verifiedAt: "2026-10-08T01:00:00Z",
        },
      ],
      meta: {
        total: 1,
        snapshotVersion: "test-tradefi",
        serverTime: "2026-10-08T01:00:00Z",
      },
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
    vi.clearAllTimers();
    vi.useRealTimers();
    virtualizerMocks.measure.mockClear();
    fundingControls.tradefiError = null;
    fundingControls.useOverride = false;
    fundingControls.tradefiSnapshot = null;
  });

  it("expands a basis chart under the selected row without opening a sheet overlay", async () => {
    render(<FundingDashboard />);

    expect(screen.getByRole("button", { name: "跨所" })).not.toBeNull();
    expect(screen.queryByRole("button", { name: "跨所套利" })).toBeNull();
    expect(screen.queryByLabelText("期现 Best Ask 价差")).toBeNull();
    expect(screen.getByText("选择合约后查看详情")).not.toBeNull();

    clickAssetRow("ETH");

    expect(await screen.findByRole("heading", { name: "ETHUSDT" })).not.toBeNull();
    expect(screen.getByRole("button", { name: "开启交易" })).not.toBeNull();
    expect(document.querySelector('[data-slot="sheet-overlay"]')).toBeNull();
    const workspace = document.querySelector("[data-workspace-panel]");
    expect(workspace?.className).toContain("grid-cols-[minmax(0,1fr)_380px]");
    expect(within(workspace as HTMLElement).getByText("历史资金费")).not.toBeNull();
    expect(await screen.findByLabelText("期现 Best Ask 价差")).not.toBeNull();
    expect(await screen.findByText("OKX-ETH-USDT")).not.toBeNull();
    expect(screen.getAllByText("预计回本时间").length).toBeGreaterThan(0);
    expect(screen.getAllByText("10 天").length).toBeGreaterThan(0);
    expect(screen.getAllByText("15 天").length).toBeGreaterThan(0);
    expect(screen.queryByText("30 bps")).toBeNull();
    expect(screen.queryByText(/按过去24h已结算资金费估算/)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "1h" }));
    expect(screen.getByRole("button", { name: "1h" })).not.toBeNull();
  }, 15000);

  it("shows non-coverable and stale 24h payback copy", () => {
    render(<FundingDashboard />);
    expect(screen.getAllByText("当前24h累计收益无法覆盖成本").length).toBeGreaterThan(0);
    expect(screen.getAllByText("行情过期，暂不估算").length).toBeGreaterThan(0);
    expect(screen.queryByText("30 bps")).toBeNull();
  }, 15000);

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
  }, 15000);

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
  }, 15000);

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
  }, 15000);

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
  }, 15000);

  it("opens TradeFi-Perps with the same funding columns and without price or start trade", () => {
    render(<FundingDashboard />);
    fireEvent.click(screen.getByRole("button", { name: "TradeFi-Perps" }));
    const headers = [
      "标的",
      "永续",
      "持仓市值 (USDT)",
      "日成交额",
      "历史年化费率",
      "距离结算",
      "资金费率",
      "24H 窗口年化",
      "7D 窗口年化",
      "预计回本时间",
    ];
    const tradefiTable = screen.getByText("IBKR · AAPL 现货").closest("table");
    expect(tradefiTable).not.toBeNull();
    const table = within(tradefiTable as HTMLElement);
    for (const header of headers) {
      expect(table.getByText(header)).not.toBeNull();
    }
    expect(table.getAllByRole("columnheader")).toHaveLength(10);
    expect(table.getAllByText("历史不足").length).toBeGreaterThan(0);
    expect(table.getByText("行情过期，暂不估算")).not.toBeNull();
    expect(table.queryByText("开启交易")).toBeNull();
    expect(table.queryByText("最新价")).toBeNull();
    expect(table.queryByText("基差")).toBeNull();
    expect(screen.queryByText("套利年化")).toBeNull();
    expect(screen.queryByText("可执行套利")).toBeNull();
    expect(screen.queryByText("verified_listing")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "OKX" }));
    expect(screen.queryByText("IBKR · AAPL 现货")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "OKX" }));
    expect(screen.getByText("IBKR · AAPL 现货")).not.toBeNull();

    fireEvent.change(screen.getByRole("combobox"), { target: { value: "4" } });
    expect(screen.queryByText("IBKR · AAPL 现货")).toBeNull();
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "all" } });
    expect(screen.getByText("IBKR · AAPL 现货")).not.toBeNull();

    fireEvent.change(screen.getByPlaceholderText("BTC、Binance、OKX..."), {
      target: { value: "不存在" },
    });
    expect(screen.getByText("没有符合条件的配对")).not.toBeNull();
  }, 15000);

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
  }, 15000);

  it("shows a TradFi load error instead of an empty result", () => {
    fundingControls.tradefiError = "TradeFi-Perps 数据加载失败，请重试";
    fundingControls.useOverride = true;
    fundingControls.tradefiSnapshot = null;
    render(<FundingDashboard />);
    fireEvent.click(screen.getByRole("button", { name: "TradeFi-Perps" }));
    expect(screen.getAllByText("TradeFi-Perps 数据加载失败，请重试").length).toBeGreaterThan(0);
    expect(screen.queryByText("没有符合条件的配对")).toBeNull();
  }, 15000);

  it("keeps the previous TradFi rows visible when a later load fails", () => {
    fundingControls.tradefiError = "TradeFi-Perps 数据加载失败，请重试";
    render(<FundingDashboard />);
    fireEvent.click(screen.getByRole("button", { name: "TradeFi-Perps" }));
    expect(screen.getByRole("alert").textContent).toBe("TradeFi-Perps 数据加载失败，请重试");
    expect(screen.getByText("IBKR · AAPL 现货")).not.toBeNull();
    expect(screen.getByText("NASDAQ · conId 265598")).not.toBeNull();
  }, 15000);

  it("shows the empty filter message when a saved snapshot has no matching rows", () => {
    fundingControls.tradefiError = "TradeFi-Perps 数据加载失败，请重试";
    render(<FundingDashboard />);
    fireEvent.click(screen.getByRole("button", { name: "TradeFi-Perps" }));
    fireEvent.change(screen.getByPlaceholderText("BTC、Binance、OKX..."), {
      target: { value: "不存在" },
    });
    expect(screen.getByRole("alert").textContent).toBe("TradeFi-Perps 数据加载失败，请重试");
    expect(screen.getByText("没有符合条件的配对")).not.toBeNull();
  }, 15000);

  it("clears the TradFi error after a later success", () => {
    fundingControls.tradefiError = "TradeFi-Perps 数据加载失败，请重试";
    const view = render(<FundingDashboard />);
    fireEvent.click(screen.getByRole("button", { name: "TradeFi-Perps" }));
    expect(screen.getByRole("alert")).not.toBeNull();
    fundingControls.tradefiError = null;
    view.rerender(<FundingDashboard />);
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByText("IBKR · AAPL 现货")).not.toBeNull();
  }, 15000);

  it("finds AAPL and shows every returned AAPL row when thresholds are zero", () => {
    const exchanges = ["Binance", "OKX", "Bybit", "Bitget", "Gate", "Hyperliquid", "Aster", "Lighter"];
    fundingControls.useOverride = true;
    fundingControls.tradefiSnapshot = {
      data: exchanges.map((exchange, index) => ({
        id: `AAPL-${exchange}`,
        stockSymbol: "AAPL",
        spotExchange: "IBKR",
        conId: "265598",
        primaryExchange: index === 0 ? "NASDAQ" : "",
        localSymbol: "AAPL",
        tradingClass: "AAPL",
        exchange,
        exchangeSymbol: `${exchange}-AAPLUSDT`,
        baseAsset: "AAPL",
        quoteAsset: "USDT",
        fundingRate: 1,
        perpetualFundingAnnualized: 11,
        annualizedRate: 11,
        currentFundingRate: 1,
        nextFundingRate: null,
        settlementIntervalHours: 8,
        nextFundingAt: "2026-10-08T08:00:00Z",
        nextSettlementAt: "2026-10-08T08:00:00Z",
        volume24hBase: 10,
        turnover24hUsd: index < 4 ? 5_000_000 : 0,
        dailyVolume: index < 4 ? 5_000_000 : 0,
        positionQuantity: 100,
        positionNotional: index < 4 ? 2_000_000 : 0,
        cumulative24h: 0.2,
        cumulative7d: 1,
        history24hComplete: false,
        history7dComplete: false,
        stale: false,
        updatedAt: "2026-10-08T01:00:00Z",
        verifiedAt: "2026-10-08T01:00:00Z",
      })),
      meta: { total: exchanges.length, snapshotVersion: "aapl-8", serverTime: "2026-10-08T01:00:00Z" },
    };
    render(<FundingDashboard />);
    fireEvent.click(screen.getByRole("button", { name: "TradeFi-Perps" }));
    fireEvent.change(screen.getByPlaceholderText("BTC、Binance、OKX..."), {
      target: { value: "AAPL" },
    });
    expect(screen.getAllByText("IBKR · AAPL 现货")).toHaveLength(4);
    expect(screen.getAllByText("— · conId 265598").length).toBeGreaterThan(0);
    fireEvent.change(screen.getByRole("slider", { name: "较小腿持仓" }), {
      target: { value: "0" },
    });
    fireEvent.change(screen.getByRole("slider", { name: "较小腿日成交额" }), {
      target: { value: "0" },
    });
    expect(screen.getAllByText("IBKR · AAPL 现货")).toHaveLength(8);
  }, 15000);
});
