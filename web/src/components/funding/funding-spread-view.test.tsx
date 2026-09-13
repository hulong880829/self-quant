// @vitest-environment jsdom

import * as React from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { FundingSpread } from "@/types/market";

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

import { FundingSpreadView } from "./funding-spread-view";

const spread: FundingSpread = {
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
};

describe("FundingSpreadView", () => {
  afterEach(cleanup);

  it("shows direction, periods, liquidity and both settlement intervals", () => {
    render(<FundingSpreadView data={[spread]} loading={false} />);

    expect(screen.getByText("跨所资金费套利")).not.toBeNull();
    expect(screen.getByRole("button", { name: "开启交易" })).not.toBeNull();
    expect(screen.getByText("即时年化")).not.toBeNull();
    expect(screen.getByText("24H 年化")).not.toBeNull();
    expect(screen.getByText("7D 年化")).not.toBeNull();
    expect(screen.getByText("可用容量（较小腿）")).not.toBeNull();
    expect(screen.getAllByText("8h").length).toBeGreaterThan(0);
    expect(screen.getAllByText("4h").length).toBeGreaterThan(0);
  });

  it("sorts by 24H annualized spread descending by default", () => {
    const ethSpread: FundingSpread = {
      ...spread,
      id: "ETHUSDT-binance-okx",
      symbol: "ETHUSDT",
      baseAsset: "ETH",
      spreadAnnualized: 99,
      spread24hAnnualized: 1,
    };
    const { container } = render(
      <FundingSpreadView data={[ethSpread, spread]} loading={false} />,
    );
    expect(container.querySelector("tbody tr")?.textContent).toContain("BTC/USDT");
  });

  it("expands a cross-exchange chart under the selected row", () => {
    render(<FundingSpreadView data={[spread]} loading={false} />);
    expect(screen.queryByLabelText("跨所 Best Ask 价差")).toBeNull();
    const row = screen.getByText("永续对冲").closest("tr");
    expect(row).not.toBeNull();
    fireEvent.click(row!);
    expect(screen.getByLabelText("跨所 Best Ask 价差")).not.toBeNull();
    expect(screen.getByText("OKX/Binance-BTC-USDT-BTCUSDT/BTCUSDT")).not.toBeNull();
    fireEvent.click(row!);
    expect(screen.queryByLabelText("跨所 Best Ask 价差")).toBeNull();
  });

  it("keeps DEX spread detail, chart, and trading prefill available", () => {
    const dexSpread: FundingSpread = {
      ...spread,
      id: "BTC-lighter-aster",
      longLeg: {
        ...spread.longLeg,
        exchange: "Lighter",
        exchangeSymbol: "BTC",
        globalSymbol: "BTCUSDC",
        quoteAsset: "USDC",
      },
      shortLeg: {
        ...spread.shortLeg,
        exchange: "Aster",
        exchangeSymbol: "BTCUSDT",
        globalSymbol: "BTCUSDT",
      },
    };
    render(<FundingSpreadView data={[dexSpread]} loading={false} />);
    expect(screen.getByRole("button", { name: "开启交易" })).not.toBeNull();
    fireEvent.click(screen.getByText("永续对冲").closest("tr")!);
    expect(
      screen.getByText("Aster/Lighter-BTC-USDT-BTCUSDT/BTCUSDC"),
    ).not.toBeNull();
  });

  it("queries Hyperliquid HIP-3 basis spreads with the xyz ClickHouse symbol", () => {
    const hip3Spread: FundingSpread = {
      ...spread,
      id: "ZHIPU-binance-hyperliquid",
      symbol: "ZHIPUUSDC",
      baseAsset: "ZHIPU",
      quoteAsset: "USDC",
      longLeg: {
        ...spread.longLeg,
        exchange: "Binance",
        exchangeSymbol: "ZHIPUUSDT",
        globalSymbol: "ZHIPUUSDT",
        baseAsset: "ZHIPU",
        quoteAsset: "USDT",
        venueContractType: "TRADIFI_PERPETUAL",
      },
      shortLeg: {
        ...spread.shortLeg,
        exchange: "Hyperliquid",
        exchangeSymbol: "xyz:ZHIPU",
        globalSymbol: "ZHIPUUSDC",
        baseAsset: "ZHIPU",
        quoteAsset: "USDC",
        venueContractType: "HIP3",
      },
    };
    render(<FundingSpreadView data={[hip3Spread]} loading={false} />);
    fireEvent.click(screen.getByText("永续对冲").closest("tr")!);
    expect(
      screen.getByText("Hyperliquid/Binance-ZHIPU-USDC-XYZZHIPUUSDC/ZHIPUUSDT"),
    ).not.toBeNull();
  });

  it("hides start trade when either spread leg is Entropy", () => {
    const entropySpread: FundingSpread = {
      ...spread,
      id: "ANTH-entropy-binance",
      longLeg: {
        ...spread.longLeg,
        exchange: "Entropy",
        exchangeSymbol: "io:ANTH",
        globalSymbol: "ANTHUSDC",
        baseAsset: "ANTH",
        quoteAsset: "USDC",
        venueContractType: "HIP3",
      },
      shortLeg: {
        ...spread.shortLeg,
        exchange: "Binance",
        exchangeSymbol: "ANTHUSDT",
        globalSymbol: "ANTHUSDT",
        baseAsset: "ANTH",
      },
    };
    const { rerender } = render(<FundingSpreadView data={[entropySpread]} loading={false} />);
    expect(screen.queryByRole("button", { name: "开启交易" })).toBeNull();
    rerender(
      <FundingSpreadView
        data={[{
          ...entropySpread,
          longLeg: entropySpread.shortLeg,
          shortLeg: entropySpread.longLeg,
        }]}
        loading={false}
      />,
    );
    expect(screen.queryByRole("button", { name: "开启交易" })).toBeNull();
  });
});
