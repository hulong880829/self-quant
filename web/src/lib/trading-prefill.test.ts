import { describe, expect, it } from "vitest";

import type { TraderInstrument } from "@/lib/api/trader";
import type { FundingOpportunity, FundingSpread } from "@/types/market";
import {
  arbitragePrefillSignature,
  buildArbitragePrefillUrlFromOpportunity,
  buildArbitragePrefillUrlFromSpread,
  compactExchangeSymbol,
  exchangeToSlug,
  findPrefillProductGroup,
  findPrefillInstrument,
  parseArbitragePrefill,
  tradingViewFromSearchParams,
} from "./trading-prefill";

const opportunity: FundingOpportunity = {
  id: "binance-btwusdt",
  exchange: "Binance",
  exchangeSymbol: "BTWUSDT",
  symbol: "BTWUSDT",
  baseAsset: "BTW",
  quoteAsset: "USDT",
  positionQuantity: 1,
  positionNotional: 1_000_000,
  dailyVolume: 1_000_000,
  annualizedRate: 10,
  currentFundingRate: 0.01,
  nextFundingRate: null,
  settlementIntervalHours: 8,
  nextSettlementAt: "2026-08-24T00:00:00Z",
  cumulative24h: 0.01,
  cumulative7d: 0.05,
  latestPrice: 0.4,
  priceChange24h: 0.01,
  updatedAt: "2026-08-24T00:00:00Z",
  fundingHistory: [],
  index: { name: "INDEX", value: 0, weight: 0 },
};

const spread: FundingSpread = {
  id: "GRVTUSDT-bybit-okx",
  symbol: "GRVTUSDT",
  baseAsset: "GRVT",
  quoteAsset: "USDT",
  longLeg: {
    exchange: "Bybit",
    exchangeSymbol: "GRVTUSDT",
    globalSymbol: "GRVTUSDT",
    baseAsset: "GRVT",
    quoteAsset: "USDT",
    fundingRate: -0.004,
    settlementIntervalHours: 4,
    nextSettlementAt: "2026-08-24T12:00:00Z",
    positionNotional: 2_000_000,
    dailyVolume: 3_000_000,
    latestPrice: 0.21,
    updatedAt: "2026-08-24T00:00:00Z",
    stale: false,
  },
  shortLeg: {
    exchange: "OKX",
    exchangeSymbol: "GRVT-USDT-SWAP",
    globalSymbol: "GRVTUSDT",
    baseAsset: "GRVT",
    quoteAsset: "USDT",
    fundingRate: -0.001,
    settlementIntervalHours: 4,
    nextSettlementAt: "2026-08-24T12:00:00Z",
    positionNotional: 1_500_000,
    dailyVolume: 2_000_000,
    latestPrice: 0.214,
    updatedAt: "2026-08-24T00:00:00Z",
    stale: false,
  },
  spreadAnnualized: 6,
  spread24hAnnualized: 1,
  spread7dAnnualized: 0.3,
  minPositionNotional: 1_500_000,
  minDailyVolume: 2_000_000,
  updatedAt: "2026-08-24T00:00:00Z",
  stale: false,
};

describe("trading prefill", () => {
  it("opens the arbitrage workspace only for a complete prefill", () => {
    expect(tradingViewFromSearchParams(new URLSearchParams("mode=arbitrage"))).toBe(
      "manual",
    );
    expect(
      tradingViewFromSearchParams(
        new URLSearchParams(
          "mode=arbitrage&legAExchange=binance&legAContract=spot&legABase=BTC&legAQuote=USDT&legBExchange=okx&legBContract=perpetual&legBBase=BTC&legBQuote=USDT",
        ),
      ),
    ).toBe("arbitrage");
    expect(tradingViewFromSearchParams(new URLSearchParams())).toBe("manual");
  });

  it("lowercases exchange display names", () => {
    expect(exchangeToSlug("Binance")).toBe("binance");
    expect(exchangeToSlug(" OKX ")).toBe("okx");
  });

  it("builds a same-venue spot/perpetual URL from a funding opportunity", () => {
    const url = buildArbitragePrefillUrlFromOpportunity(opportunity);
    const params = new URLSearchParams(url.slice(url.indexOf("?") + 1));
    expect(url.startsWith("/trading?")).toBe(true);
    expect(parseArbitragePrefill(params)).toEqual({
      mode: "arbitrage",
      legA: {
        exchange: "binance",
        contract: "spot",
        base: "BTW",
        quote: "USDT",
      },
      legB: {
        exchange: "binance",
        contract: "perpetual",
        base: "BTW",
        quote: "USDT",
        exchangeSymbol: "BTWUSDT",
      },
    });
  });

  it("builds a cross-venue perpetual URL from a funding spread", () => {
    const url = buildArbitragePrefillUrlFromSpread(spread);
    const params = new URLSearchParams(url.slice(url.indexOf("?") + 1));
    expect(parseArbitragePrefill(params)).toEqual({
      mode: "arbitrage",
      legA: {
        exchange: "bybit",
        contract: "perpetual",
        base: "GRVT",
        quote: "USDT",
        exchangeSymbol: "GRVTUSDT",
      },
      legB: {
        exchange: "okx",
        contract: "perpetual",
        base: "GRVT",
        quote: "USDT",
        exchangeSymbol: "GRVT-USDT-SWAP",
      },
    });
  });

  it("preserves Hyperliquid USDC and other-venue USDT contracts", () => {
    const mixedQuoteSpread: FundingSpread = {
      ...spread,
      baseAsset: "ZEC",
      longLeg: {
        ...spread.longLeg,
        exchange: "Hyperliquid",
        exchangeSymbol: "ZEC",
        baseAsset: "ZEC",
        quoteAsset: "USDC",
        globalSymbol: "ZECUSDC",
      },
      shortLeg: {
        ...spread.shortLeg,
        exchange: "Binance",
        exchangeSymbol: "ZECUSDT",
        baseAsset: "ZEC",
        quoteAsset: "USDT",
        globalSymbol: "ZECUSDT",
      },
    };
    const url = buildArbitragePrefillUrlFromSpread(mixedQuoteSpread);
    const params = parseArbitragePrefill(
      new URLSearchParams(url.slice(url.indexOf("?") + 1)),
    );
    expect(params?.legA.quote).toBe("USDC");
    expect(params?.legB.quote).toBe("USDT");
    expect(params?.legA.exchangeSymbol).toBe("ZEC");
    expect(params?.legB.exchangeSymbol).toBe("ZECUSDT");
  });

  it("builds perpetual spread prefills for all DEX venues", () => {
    for (const [exchange, quote] of [
      ["Hyperliquid", "USDC"],
      ["Aster", "USDT"],
      ["Lighter", "USDC"],
    ] as const) {
      const dexSpread: FundingSpread = {
        ...spread,
        longLeg: {
          ...spread.longLeg,
          exchange,
          quoteAsset: quote,
          exchangeSymbol: exchange === "Aster" ? "GRVTUSDT" : "GRVT",
        },
      };
      const url = buildArbitragePrefillUrlFromSpread(dexSpread);
      const params = parseArbitragePrefill(
        new URLSearchParams(url.slice(url.indexOf("?") + 1)),
      );
      expect(params?.legA).toMatchObject({
        exchange: exchange.toLowerCase(),
        contract: "perpetual",
        quote,
      });
    }
  });

  it("returns null when required query fields are missing", () => {
    expect(parseArbitragePrefill(new URLSearchParams("mode=arbitrage"))).toBeNull();
    expect(parseArbitragePrefill(new URLSearchParams("mode=manual"))).toBeNull();
    expect(parseArbitragePrefill(null)).toBeNull();
  });

  it("parses an optional prefillRequestId and keeps URLs without one valid", () => {
    const withoutId = parseArbitragePrefill(
      new URLSearchParams(
        "mode=arbitrage&legAExchange=binance&legAContract=spot&legABase=BTC&legAQuote=USDT&legBExchange=okx&legBContract=perpetual&legBBase=BTC&legBQuote=USDT",
      ),
    );
    const withId = parseArbitragePrefill(
      new URLSearchParams(
        "mode=arbitrage&legAExchange=binance&legAContract=spot&legABase=BTC&legAQuote=USDT&legBExchange=okx&legBContract=perpetual&legBBase=BTC&legBQuote=USDT&prefillRequestId=req-a",
      ),
    );
    expect(withoutId?.prefillRequestId).toBeUndefined();
    expect(withId?.prefillRequestId).toBe("req-a");
    expect(withoutId?.legA.quote).toBe("USDT");
    const sameLegsDifferentId = parseArbitragePrefill(
      new URLSearchParams(
        "mode=arbitrage&legAExchange=binance&legAContract=spot&legABase=BTC&legAQuote=USDT&legBExchange=okx&legBContract=perpetual&legBBase=BTC&legBQuote=USDT&prefillRequestId=req-b",
      ),
    );
    expect(arbitragePrefillSignature(withId)).not.toBe(
      arbitragePrefillSignature(sameLegsDifferentId),
    );
    expect(arbitragePrefillSignature(withoutId)).not.toBe(
      arbitragePrefillSignature(withId),
    );
  });

  it("prefers exchangeSymbol then falls back to base and quote", () => {
    const items: TraderInstrument[] = [
      {
        id: 1,
        exchange: "binance",
        contractType: "perpetual",
        exchangeSymbol: "ETHUSDT",
        baseAsset: "ETH",
        quoteAsset: "USDT",
        settleAsset: "USDT",
        contractSize: "1",
        priceTick: "0.1",
        quantityStep: "0.001",
      },
      {
        id: 2,
        exchange: "binance",
        contractType: "perpetual",
        exchangeSymbol: "BTWUSDT",
        baseAsset: "BTW",
        quoteAsset: "USDT",
        settleAsset: "USDT",
        contractSize: "1",
        priceTick: "0.0001",
        quantityStep: "1",
      },
    ];
    expect(
      findPrefillInstrument(items, {
        exchange: "binance",
        contract: "perpetual",
        base: "BTW",
        quote: "USDT",
        exchangeSymbol: "BTWUSDT",
      })?.id,
    ).toBe(2);
    expect(
      findPrefillInstrument(items, {
        exchange: "binance",
        contract: "perpetual",
        base: "ETH",
        quote: "USDT",
      })?.id,
    ).toBe(1);
  });

  it("normalizes Gate and OKX venue symbol separators", () => {
    expect(compactExchangeSymbol("BEAT_USDT")).toBe("BEATUSDT");
    expect(compactExchangeSymbol("BEAT-USDT-SWAP")).toBe("BEATUSDT");
    const items: TraderInstrument[] = [
      {
        id: 3,
        exchange: "gate",
        contractType: "perpetual",
        exchangeSymbol: "BEATUSDT",
        baseAsset: "BEAT",
        quoteAsset: "USDT",
        settleAsset: "USDT",
        contractSize: "1",
        priceTick: "0.0001",
        quantityStep: "1",
      },
    ];
    expect(
      findPrefillInstrument(items, {
        exchange: "gate",
        contract: "perpetual",
        base: "BEAT",
        quote: "USDT",
        exchangeSymbol: "BEAT_USDT",
      })?.id,
    ).toBe(3);
  });

  it("keeps Chinese symbols and does not select an earlier Chinese instrument", () => {
    expect(compactExchangeSymbol("龙虾USDT")).toBe("龙虾USDT");
    expect(compactExchangeSymbol("龙虾-USDT-SWAP")).toBe("龙虾USDT");
    expect(compactExchangeSymbol("哈基米USDT")).toBe("哈基米USDT");
    expect(compactExchangeSymbol("BEAT_USDT")).toBe("BEATUSDT");
    expect(compactExchangeSymbol("BEAT-USDT-SWAP")).toBe("BEATUSDT");
    const chinese = (id: number, base: string): TraderInstrument => ({
      id,
      exchange: "aster",
      contractType: "perpetual",
      exchangeSymbol: `${base}USDT`,
      baseAsset: base,
      quoteAsset: "USDT",
      settleAsset: "USDT",
      contractSize: "1",
      priceTick: "0.0001",
      quantityStep: "1",
    });
    const items = [chinese(1, "哈基米"), chinese(2, "龙虾")];
    expect(
      findPrefillInstrument(items, {
        exchange: "aster",
        contract: "perpetual",
        base: "龙虾",
        quote: "USDT",
        exchangeSymbol: "龙虾USDT",
      })?.id,
    ).toBe(2);
  });

  it("selects only a product group containing both requested exchanges", () => {
    const account = (id: number, productName: string, exchangeSlug: string) => ({
      id,
      productName,
      exchange: (exchangeSlug === "gate" ? "Gate" : "OKX") as "Gate" | "OKX",
      exchangeSlug,
      accountName: `${exchangeSlug}-${id}`,
      hasPassphrase: false,
      createdAt: "",
      updatedAt: "",
    });
    const prefill = parseArbitragePrefill(
      new URLSearchParams(
        "mode=arbitrage&legAExchange=gate&legAContract=perpetual&legABase=BEAT&legAQuote=USDT&legBExchange=okx&legBContract=perpetual&legBBase=BEAT&legBQuote=USDT",
      ),
    );
    expect(prefill).not.toBeNull();
    const group = findPrefillProductGroup(
      [
        { productName: "Gate only", accounts: [account(1, "Gate only", "gate")] },
        {
          productName: "Arbitrage",
          accounts: [
            account(2, "Arbitrage", "gate"),
            account(3, "Arbitrage", "okx"),
          ],
        },
      ],
      prefill!,
    );
    expect(group?.productName).toBe("Arbitrage");
    expect(arbitragePrefillSignature(prefill)).toContain("BEAT");
  });
});
