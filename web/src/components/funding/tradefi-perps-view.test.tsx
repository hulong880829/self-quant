// @vitest-environment jsdom

import * as React from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { Exchange, TradefiPerpRow } from "@/types/market";

vi.mock("@/components/funding/basis-spread-chart", () => ({
  BasisSpreadPanel: ({
    venue,
    compareVenue,
    baseAsset,
    quoteAsset,
    venueSymbol,
    compareVenueSymbol,
    identity,
  }: {
    venue: string;
    compareVenue?: string;
    baseAsset: string;
    quoteAsset: string;
    venueSymbol?: string;
    compareVenueSymbol?: string;
    identity?: { venueExchangeSymbol: string };
  }) => (
    <section aria-label="跨所 Best Ask 价差">
      <span>
        {`${venue}/${compareVenue}-${baseAsset}-${quoteAsset}-${venueSymbol}/${compareVenueSymbol}-${identity?.venueExchangeSymbol}`}
      </span>
    </section>
  ),
}));

import { TradefiPerpsView } from "./tradefi-perps-view";

function row(overrides: Partial<TradefiPerpRow> & Pick<TradefiPerpRow, "exchange" | "exchangeSymbol" | "baseAsset" | "quoteAsset" | "conId">): TradefiPerpRow {
  return {
    id: overrides.conId,
    stockSymbol: "GME",
    spotExchange: "IBKR",
    primaryExchange: "NYSE",
    localSymbol: "GME",
    tradingClass: "GME",
    fundingRate: 0.0001,
    perpetualFundingAnnualized: 0.1,
    annualizedRate: 0.1,
    currentFundingRate: 0.0001,
    nextFundingRate: 0.0001,
    settlementIntervalHours: 1,
    nextFundingAt: "2026-10-13T18:00:00Z",
    nextSettlementAt: "2026-10-13T18:00:00Z",
    volume24hBase: 1,
    turnover24hUsd: 1_000_000,
    dailyVolume: 1_000_000,
    positionQuantity: 1,
    positionNotional: 1_000_000,
    cumulative24h: 0,
    cumulative7d: 0,
    updatedAt: "2026-10-13T17:00:00Z",
    verifiedAt: "2026-10-13T17:00:00Z",
    ...overrides,
  };
}

const hyperliquid = row({
  exchange: "Hyperliquid" as Exchange,
  exchangeSymbol: "xyz:GME",
  baseAsset: "GME",
  quoteAsset: "USDC",
  conId: "36285627",
});

const binance = row({
  exchange: "Binance" as Exchange,
  exchangeSymbol: "GMEUSDT",
  baseAsset: "GME",
  quoteAsset: "USDT",
  conId: "36285627",
  id: "binance-gme",
});

afterEach(cleanup);

describe("TradefiPerpsView", () => {
  it("queries Hyperliquid HIP-3 with the ClickHouse symbol and keeps ordinary symbols", () => {
    render(<TradefiPerpsView data={[hyperliquid, binance]} loading={false} hasSnapshot />);

    fireEvent.click(screen.getByText("Hyperliquid · xyz:GME").closest("tr")!);
    expect(
      screen.getByText("Hyperliquid/ibkr-GME-USDC-XYZGMEUSDC/GME-xyz:GME"),
    ).not.toBeNull();

    fireEvent.click(screen.getByText("Binance · GMEUSDT").closest("tr")!);
    expect(
      screen.getByText("Binance/ibkr-GME-USDT-GMEUSDT/GME-GMEUSDT"),
    ).not.toBeNull();
  });
});
