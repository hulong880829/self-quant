import { describe, expect, it } from "vitest";

import {
  chooseFirstLivePeriod,
  chooseLiveMarket,
  countLiveMarkets,
  resolveMarketInfo,
} from "./polymarket-market";
import type { PolymarketMarket } from "@/types/polymarket";

function market(
  overrides: Partial<PolymarketMarket> & Pick<PolymarketMarket, "id" | "period">,
): PolymarketMarket {
  return {
    conditionId: "c1",
    slug: "btc-updown-5m-1",
    asset: "BTC",
    title: "Bitcoin Up or Down",
    windowStart: "2026-08-08T12:00:00Z",
    windowEnd: "2026-08-08T12:05:00Z",
    upTokenId: "1",
    downTokenId: "2",
    tickSize: "0.01",
    negativeRisk: false,
    active: true,
    ...overrides,
  };
}

describe("polymarket market helpers", () => {
  const now = Date.parse("2026-08-08T12:02:00Z");

  it("counts only live active markets", () => {
    const markets = [
      market({ id: "live", period: "5m", windowEnd: "2026-08-08T12:05:00Z" }),
      market({
        id: "expired",
        period: "5m",
        windowEnd: "2026-08-08T12:01:00Z",
      }),
      market({ id: "inactive", period: "5m", active: false }),
    ];
    expect(countLiveMarkets(markets, "BTC", "5m", now)).toBe(1);
  });

  it("chooses the nearest live market window end", () => {
    const markets = [
      market({ id: "later", period: "15m", windowEnd: "2026-08-08T12:30:00Z" }),
      market({ id: "soon", period: "15m", windowEnd: "2026-08-08T12:15:00Z" }),
      market({
        id: "expired",
        period: "15m",
        windowEnd: "2026-08-08T12:01:00Z",
      }),
    ];
    expect(chooseLiveMarket(markets, "BTC", "15m", now)?.id).toBe("soon");
  });

  it("returns null when all markets are expired", () => {
    const markets = [
      market({ id: "expired", period: "5m", windowEnd: "2026-08-08T12:01:00Z" }),
    ];
    expect(chooseLiveMarket(markets, "BTC", "5m", now)).toBeNull();
  });

  it("picks the first period with live markets", () => {
    const markets = [
      market({ id: "m15", period: "15m", windowEnd: "2026-08-08T12:15:00Z" }),
      market({ id: "m1h", period: "1h", windowEnd: "2026-08-08T13:00:00Z" }),
    ];
    expect(
      chooseFirstLivePeriod(markets, "BTC", ["5m", "15m", "1h"], now),
    ).toBe("15m");
  });

  it("resolves a readable title from condition or token ids", () => {
    const item = market({ id: "live", period: "5m", conditionId: "condition-1" });
    expect(
      resolveMarketInfo([item], {
        conditionId: "condition-1",
        tokenId: "unknown",
        fallbackTitle: "123456789012345678901",
      }),
    ).toEqual({ title: "Bitcoin Up or Down", market: item });
  });

  it("keeps a readable fallback title for an expired market", () => {
    expect(
      resolveMarketInfo([], {
        conditionId: "condition-old",
        tokenId: "token-old",
        fallbackTitle: "Bitcoin Up or Down - August 11",
      }),
    ).toEqual({
      title: "Bitcoin Up or Down - August 11",
      market: null,
    });
  });

  it("does not expose a raw token as the primary market label", () => {
    expect(
      resolveMarketInfo([], {
        conditionId: "",
        tokenId: "123456789012345678901234",
        fallbackTitle: "123456789012345678901234",
      }).title,
    ).toBe("未知市场");
  });
});
