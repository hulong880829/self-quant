// @vitest-environment jsdom

import * as React from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { FundingFilters, RankedFundingOpportunity } from "@/types/market";

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
        start: index * 64,
        end: (index + 1) * 64,
        size: 64,
      })),
    getTotalSize: () => count * 64,
    measure: vi.fn(),
  }),
}));

vi.mock("@/components/funding/basis-spread-chart", () => ({
  BasisSpreadPanel: ({
    venue,
    compareVenue,
    baseAsset,
    quoteAsset,
    venueSymbol,
    compareVenueSymbol,
  }: {
    venue: string;
    compareVenue?: string;
    baseAsset: string;
    quoteAsset: string;
    venueSymbol?: string;
    compareVenueSymbol?: string;
  }) => (
    <section aria-label="跨所 Best Ask 价差">
      <span>{`${venue}/${compareVenue}-${baseAsset}-${quoteAsset}-${venueSymbol}/${compareVenueSymbol}`}</span>
    </section>
  ),
}));

const { fetchFundingOpportunities } = vi.hoisted(() => ({
  fetchFundingOpportunities: vi.fn(),
}));
vi.mock("@/lib/api/funding-opportunities", async (importOriginal) => {
  const original = await importOriginal<typeof import("@/lib/api/funding-opportunities")>();
  return { ...original, fetchFundingOpportunities };
});

import { FundingOpportunityRanking } from "./funding-opportunity-ranking";

const item: RankedFundingOpportunity = {
  id: "BTCUSDT-binance-okx-1h",
  rank: 1,
  symbol: "BTCUSDT",
  baseAsset: "BTC",
  quoteAsset: "USDT",
  period: "8h",
  longLeg: {
    exchange: "Binance",
    exchangeSymbol: "BTCUSDT",
    globalSymbol: "BTCUSDT",
    baseAsset: "BTC",
    quoteAsset: "USDT",
    fundingRate: 0.01,
    settlementIntervalHours: 8,
    nextSettlementAt: "2026-08-23T00:00:00Z",
    positionNotional: 5_000_000,
    dailyVolume: 20_000_000,
    latestPrice: 60_000,
    updatedAt: "2026-08-22T14:00:00Z",
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
    nextSettlementAt: "2026-08-23T00:00:00Z",
    positionNotional: 4_000_000,
    dailyVolume: 30_000_000,
    latestPrice: 60_010,
    updatedAt: "2026-08-22T14:00:00Z",
    stale: false,
  },
  currentMidSpreadBps: 12,
  currentExecutableSpreadBps: 10,
  targetSpreadBps: 2,
  periodExpectedReturn: 0.1,
  fundingExpectedAnnualized: 20,
  spreadExpectedAnnualized: 30,
  combinedExpectedAnnualized: 50,
  firstPassageProbability: 80,
  profitProbability: 75,
  expectedExitMinutes: 32,
  p5Return: -0.02,
  minPositionNotional: 4_000_000,
  minDailyVolume: 20_000_000,
  coverage: 98,
  confidence: 85,
  modelState: "replay_7d",
  sampleCount: 160,
  expectedPaybackMinutes: 75,
  paybackStatus: "ready",
  updatedAt: "2026-08-22T14:00:00Z",
  stale: false,
};

const filters: FundingFilters = {
  search: "",
  minPositionNotional: 1_000_000,
  minDailyVolume: 1_000_000,
  intervalHours: "all",
  exchanges: [],
  contractKinds: [],
  direction: "all",
};

const snapshot = {
  status: "updated" as const,
  etag: '"rank-1"',
  snapshot: {
    data: [item],
    meta: {
      total: 1,
      snapshotVersion: "rank-1",
      serverTime: "2026-08-22T14:00:01Z",
      calculatedAt: "2026-08-22T14:00:00Z",
      stale: false,
      status: "ready" as const,
      lastSuccessfulAt: "2026-08-22T14:00:00Z",
      dataThrough: "2026-08-22T13:59:00Z",
      generation: 1,
    },
  },
};

describe("FundingOpportunityRanking", () => {
  afterEach(() => {
    cleanup();
    fetchFundingOpportunities.mockReset();
  });

  it("shows periods, explicit legs, and combined return details", async () => {
    fetchFundingOpportunities.mockResolvedValue(snapshot);
    render(<FundingOpportunityRanking filters={filters} />);

    await waitFor(() => expect(fetchFundingOpportunities).toHaveBeenCalled());
    expect(fetchFundingOpportunities).toHaveBeenNthCalledWith(
      1,
      expect.objectContaining({ period: "8h" }),
      null,
      expect.any(AbortSignal),
    );
    expect(screen.getByRole("button", { name: "8h" })).not.toBeNull();
    expect(screen.getByRole("button", { name: "24h" })).not.toBeNull();
    expect(screen.getByText("明确做多")).not.toBeNull();
    expect(screen.getByText("明确做空")).not.toBeNull();
    expect(screen.getByRole("button", { name: "开启交易" })).not.toBeNull();
    expect(screen.getByText("收益拆解（年化）")).not.toBeNull();
    expect(screen.getAllByText("盈利概率").length).toBeGreaterThan(0);
    expect(screen.getByText("有效样本")).not.toBeNull();
    expect(screen.getAllByText("预计回本周期").length).toBeGreaterThan(0);
    expect(screen.getAllByText("1 小时 15 分钟").length).toBeGreaterThan(0);
    expect(screen.getByText("价差（已扣 12bps）")).not.toBeNull();
    expect(screen.getByText("机会排名 #1 · 8h")).not.toBeNull();
    expect(screen.queryByText("replay_7d")).toBeNull();
    expect(screen.queryByText("数据延迟")).toBeNull();
    expect(screen.queryByText(/排名快照已过期/)).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "24h" }));
    await waitFor(() => {
      expect(fetchFundingOpportunities).toHaveBeenLastCalledWith(
        expect.objectContaining({ period: "24h" }),
        null,
        expect.any(AbortSignal),
      );
    });
  });

  it("hides the rank column and expands a cross-exchange chart under the selected row", async () => {
    fetchFundingOpportunities.mockResolvedValue(snapshot);
    render(<FundingOpportunityRanking filters={filters} />);

    await waitFor(() => expect(screen.getByText("BTC/USDT")).not.toBeNull());
    expect(screen.queryByRole("columnheader", { name: "排名" })).toBeNull();
    expect(screen.queryByText(/^#1$/)).toBeNull();
    expect(screen.queryByLabelText("跨所 Best Ask 价差")).toBeNull();

    const row = screen.getByText("BTC/USDT").closest("tr");
    expect(row).not.toBeNull();
    fireEvent.click(row!);
    expect(screen.getByLabelText("跨所 Best Ask 价差")).not.toBeNull();
    expect(screen.getByText("OKX/Binance-BTC-USDT-BTCUSDT/BTCUSDT")).not.toBeNull();
    expect(screen.getByText("明确做多")).not.toBeNull();
    expect(screen.getByText("明确做空")).not.toBeNull();

    fireEvent.click(row!);
    expect(screen.queryByLabelText("跨所 Best Ask 价差")).toBeNull();
  });

  it("queries Hyperliquid HIP-3 basis spreads with the xyz ClickHouse symbol", async () => {
    const hip3Item: RankedFundingOpportunity = {
      ...item,
      id: "ZHIPU-binance-hyperliquid-8h",
      symbol: "ZHIPUUSDC",
      baseAsset: "ZHIPU",
      quoteAsset: "USDC",
      longLeg: {
        ...item.longLeg,
        exchange: "Binance",
        exchangeSymbol: "ZHIPUUSDT",
        globalSymbol: "ZHIPUUSDT",
        baseAsset: "ZHIPU",
        quoteAsset: "USDT",
        venueContractType: "TRADIFI_PERPETUAL",
      },
      shortLeg: {
        ...item.shortLeg,
        exchange: "Hyperliquid",
        exchangeSymbol: "xyz:ZHIPU",
        globalSymbol: "ZHIPUUSDC",
        baseAsset: "ZHIPU",
        quoteAsset: "USDC",
        venueContractType: "HIP3",
      },
    };
    fetchFundingOpportunities.mockResolvedValue({
      ...snapshot,
      snapshot: { ...snapshot.snapshot, data: [hip3Item] },
    });
    render(<FundingOpportunityRanking filters={filters} />);

    await waitFor(() => expect(screen.getByText("ZHIPU/USDC")).not.toBeNull());
    fireEvent.click(screen.getByText("ZHIPU/USDC").closest("tr")!);
    expect(
      screen.getByText("Hyperliquid/Binance-ZHIPU-USDC-XYZZHIPUUSDC/ZHIPUUSDT"),
    ).not.toBeNull();
  });
});
