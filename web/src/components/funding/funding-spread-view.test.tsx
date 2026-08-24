// @vitest-environment jsdom

import * as React from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { FundingSpread } from "@/types/market";

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
  }),
}));

vi.mock("@/components/funding/basis-spread-chart", () => ({
  BasisSpreadPanel: ({
    venue,
    compareVenue,
    baseAsset,
    quoteAsset,
  }: {
    venue: string;
    compareVenue?: string;
    baseAsset: string;
    quoteAsset: string;
  }) => (
    <section aria-label="跨所 Best Ask 价差">
      <span>{`${venue}/${compareVenue}-${baseAsset}-${quoteAsset}`}</span>
    </section>
  ),
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

  it("expands a cross-exchange chart under the selected row", () => {
    render(<FundingSpreadView data={[spread]} loading={false} />);
    expect(screen.queryByLabelText("跨所 Best Ask 价差")).toBeNull();
    const row = screen.getByText("永续对冲").closest("tr");
    expect(row).not.toBeNull();
    fireEvent.click(row!);
    expect(screen.getByLabelText("跨所 Best Ask 价差")).not.toBeNull();
    expect(screen.getByText("OKX/Binance-BTC-USDT")).not.toBeNull();
    fireEvent.click(row!);
    expect(screen.queryByLabelText("跨所 Best Ask 价差")).toBeNull();
  });
});
