import type {
  PolymarketAssetId,
  PolymarketMarket,
  PolymarketPeriodId,
} from "@/types/polymarket";

export const MARKET_REFRESH_MS = 45_000;

export interface ResolvedMarketInfo {
  title: string;
  market: PolymarketMarket | null;
}

function readableMarketLabel(
  value: string,
  conditionId: string,
  tokenId: string,
): string {
  const label = value.trim();
  if (
    label === "" ||
    label === conditionId ||
    label === tokenId ||
    /^0x[0-9a-f]+$/i.test(label) ||
    /^\d{20,}$/.test(label)
  ) {
    return "";
  }
  return label;
}

export function resolveMarketInfo(
  markets: PolymarketMarket[],
  reference: {
    conditionId: string;
    tokenId: string;
    fallbackTitle?: string;
  },
): ResolvedMarketInfo {
  const matched =
    markets.find(
      (market) =>
        (reference.conditionId !== "" &&
          market.conditionId === reference.conditionId) ||
        (reference.tokenId !== "" &&
          (market.upTokenId === reference.tokenId ||
            market.downTokenId === reference.tokenId)),
    ) ?? null;
  const matchedTitle = matched?.title.trim() ?? "";
  if (matchedTitle !== "") {
    return { title: matchedTitle, market: matched };
  }
  const fallback = readableMarketLabel(
    reference.fallbackTitle ?? "",
    reference.conditionId,
    reference.tokenId,
  );
  if (fallback !== "") {
    return { title: fallback, market: matched };
  }
  if (matched) {
    return {
      title: `${matched.asset} ${matched.period} Up or Down`,
      market: matched,
    };
  }
  return { title: "未知市场", market: null };
}

export function isLiveMarket(
  market: PolymarketMarket,
  now = Date.now(),
): boolean {
  return (
    market.active && new Date(market.windowEnd).getTime() > now
  );
}

export function countLiveMarkets(
  markets: PolymarketMarket[],
  asset: PolymarketAssetId,
  period: PolymarketPeriodId,
  now = Date.now(),
): number {
  return markets.filter(
    (market) =>
      market.asset === asset &&
      market.period === period &&
      isLiveMarket(market, now),
  ).length;
}

export function chooseLiveMarket(
  markets: PolymarketMarket[],
  asset: PolymarketAssetId,
  period: PolymarketPeriodId,
  now = Date.now(),
): PolymarketMarket | null {
  const live = markets.filter(
    (market) =>
      market.asset === asset &&
      market.period === period &&
      isLiveMarket(market, now),
  );
  if (live.length === 0) {
    return null;
  }
  return live.sort(
    (left, right) =>
      new Date(left.windowEnd).getTime() - new Date(right.windowEnd).getTime(),
  )[0];
}

export function chooseFirstLivePeriod(
  markets: PolymarketMarket[],
  asset: PolymarketAssetId,
  periods: PolymarketPeriodId[],
  now = Date.now(),
): PolymarketPeriodId | null {
  for (const period of periods) {
    if (countLiveMarkets(markets, asset, period, now) > 0) {
      return period;
    }
  }
  return null;
}
