// @vitest-environment jsdom

import * as React from "react";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { FundingSpread } from "@/types/market";

vi.mock("@/hooks/use-element-width", () => ({
  useElementWidth: () => [{ current: null }, 1400],
}));

import { FundingSpreadView } from "./funding-spread-view";

const spread: FundingSpread = {
  id: "BTCUSDT-binance-okx",
  symbol: "BTCUSDT",
  baseAsset: "BTC",
  quoteAsset: "USDT",
  longLeg: {
    exchange: "Binance",
    exchangeSymbol: "BTCUSDT",
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
};

describe("FundingSpreadView", () => {
  afterEach(cleanup);

  it("shows direction, periods, liquidity and both settlement intervals", () => {
    render(<FundingSpreadView data={[spread]} loading={false} />);

    expect(screen.getByText("跨所资金费套利")).not.toBeNull();
    expect(screen.getByText("即时年化")).not.toBeNull();
    expect(screen.getByText("24H 年化")).not.toBeNull();
    expect(screen.getByText("7D 年化")).not.toBeNull();
    expect(screen.getByText("可用容量（较小腿）")).not.toBeNull();
    expect(screen.getAllByText("8h").length).toBeGreaterThan(0);
    expect(screen.getAllByText("4h").length).toBeGreaterThan(0);
  });
});
