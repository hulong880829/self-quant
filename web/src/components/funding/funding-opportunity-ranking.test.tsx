// @vitest-environment jsdom

import * as React from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { FundingFilters, RankedFundingOpportunity } from "@/types/market";

vi.mock("@/hooks/use-element-width", () => ({
  useElementWidth: () => [{ current: null }, 1400],
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
  period: "1h",
  longLeg: {
    exchange: "Binance",
    exchangeSymbol: "BTCUSDT",
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
  modelState: "ready",
  updatedAt: "2026-08-22T14:00:00Z",
  stale: false,
};

const filters: FundingFilters = {
  search: "",
  minPositionNotional: 1_000_000,
  minDailyVolume: 1_000_000,
  intervalHours: "all",
  exchanges: [],
  direction: "all",
};

describe("FundingOpportunityRanking", () => {
  afterEach(() => {
    cleanup();
    fetchFundingOpportunities.mockReset();
  });

  it("shows periods, explicit legs, and combined return details", async () => {
    fetchFundingOpportunities.mockResolvedValue({
      status: "updated",
      etag: '"rank-1"',
      snapshot: {
        data: [item],
        meta: {
          total: 1,
          snapshotVersion: "rank-1",
          serverTime: "2026-08-22T14:00:01Z",
          calculatedAt: "2026-08-22T14:00:00Z",
          stale: false,
        },
      },
    });
    render(<FundingOpportunityRanking filters={filters} />);

    await waitFor(() => expect(fetchFundingOpportunities).toHaveBeenCalled());
    expect(screen.getByRole("button", { name: "1h" })).not.toBeNull();
    expect(screen.getByRole("button", { name: "24h" })).not.toBeNull();
    expect(screen.getByText("明确做多")).not.toBeNull();
    expect(screen.getByText("明确做空")).not.toBeNull();
    expect(screen.getByText("收益拆解（年化）")).not.toBeNull();
    expect(screen.getAllByText("盈利概率").length).toBeGreaterThan(0);

    fireEvent.click(screen.getByRole("button", { name: "4h" }));
    await waitFor(() => {
      expect(fetchFundingOpportunities).toHaveBeenLastCalledWith(
        expect.objectContaining({ period: "4h" }),
        null,
        expect.any(AbortSignal),
      );
    });
  });
});
