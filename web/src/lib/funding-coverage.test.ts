import { describe, expect, it } from "vitest";

import {
  bboCanonicalSymbol,
  canStartFundingTrade,
  formatHistoryWindow,
  historyWindowReady,
} from "./funding-coverage";

describe("funding coverage tri-state", () => {
  it("treats missing fields as the legacy six-venue display", () => {
    expect(historyWindowReady(undefined)).toBe(true);
    expect(formatHistoryWindow(undefined, 0, () => "0.0%")).toBe("0.0%");
  });

  it("hides incomplete new-venue windows instead of 0%", () => {
    expect(historyWindowReady(false)).toBe(false);
    expect(formatHistoryWindow(false, 0, () => "0.0%")).toBe("历史不足");
  });

  it("shows complete new-venue windows", () => {
    expect(historyWindowReady(true)).toBe(true);
    expect(formatHistoryWindow(true, 1.2, () => "1.2%")).toBe("1.2%");
  });

  it("keeps perpetual-only venues out of basis trades but allows cross-venue prefill", () => {
    expect(canStartFundingTrade("Hyperliquid")).toBe(false);
    expect(canStartFundingTrade("Aster")).toBe(false);
    expect(canStartFundingTrade("Lighter")).toBe(false);
    expect(canStartFundingTrade("Binance")).toBe(true);
    for (const exchange of ["Hyperliquid", "Aster", "Lighter"]) {
      expect(canStartFundingTrade(exchange, "cross")).toBe(true);
    }
  });

  it("allows HIP-3 legs on cross-venue trades but not Hyperliquid basis", () => {
    expect(
      canStartFundingTrade("Hyperliquid", "cross", {
        venueContractType: "HIP3",
        exchangeSymbol: "xyz:ZHIPU",
      }),
    ).toBe(true);
    expect(
      canStartFundingTrade("Hyperliquid", "cross", {
        exchangeSymbol: "xyz:ZHIPU",
      }),
    ).toBe(true);
    expect(
      canStartFundingTrade("Hyperliquid", "basis", {
        venueContractType: "HIP3",
        exchangeSymbol: "xyz:ZHIPU",
      }),
    ).toBe(false);
    expect(canStartFundingTrade("Binance", "basis", {
      venueContractType: "TRADIFI_PERPETUAL",
      exchangeSymbol: "ZHIPUUSDT",
    })).toBe(true);
  });

  it("keeps Entropy out of trading prefill for single and cross rows", () => {
    expect(canStartFundingTrade("Entropy")).toBe(false);
    expect(canStartFundingTrade("Entropy", "cross")).toBe(false);
    expect(
      canStartFundingTrade("Entropy", "cross", {
        venueContractType: "HIP3",
        exchangeSymbol: "io:ANTH",
      }),
    ).toBe(false);
    for (const exchange of ["Hyperliquid", "Aster", "Lighter"]) {
      expect(canStartFundingTrade(exchange, "cross")).toBe(true);
    }
  });
});

describe("bboCanonicalSymbol", () => {
  it("prefixes Hyperliquid HIP-3 ClickHouse symbols from exchangeSymbol", () => {
    expect(
      bboCanonicalSymbol({
        exchange: "Hyperliquid",
        venueContractType: "HIP3",
        exchangeSymbol: "xyz:ZHIPU",
        quoteAsset: "USDC",
        globalSymbol: "ZHIPUUSDC",
      }),
    ).toBe("XYZZHIPUUSDC");
  });

  it("keeps globalSymbol for ordinary Hyperliquid perps and other venues", () => {
    expect(
      bboCanonicalSymbol({
        exchange: "Hyperliquid",
        venueContractType: "PERPETUAL",
        exchangeSymbol: "BTC",
        quoteAsset: "USDC",
        globalSymbol: "BTCUSDC",
      }),
    ).toBe("BTCUSDC");
    expect(
      bboCanonicalSymbol({
        exchange: "Binance",
        venueContractType: "HIP3",
        exchangeSymbol: "xyz:ZHIPU",
        quoteAsset: "USDT",
        globalSymbol: "ZHIPUUSDT",
      }),
    ).toBe("ZHIPUUSDT");
  });

  it("treats Hyperliquid exchangeSymbol with a dex prefix as HIP-3", () => {
    expect(
      bboCanonicalSymbol({
        exchange: "hyperliquid",
        exchangeSymbol: "xyz:HYUNDAI",
        quoteAsset: "USDC",
        globalSymbol: "HYUNDAIUSDC",
      }),
    ).toBe("XYZHYUNDAIUSDC");
  });
});
