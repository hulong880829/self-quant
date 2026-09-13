export type PolymarketPeriodId =
  | "5m"
  | "15m"
  | "1h"
  | "4h"
  | "1d"
  | "1w"
  | "1mo"
  | "1y";

export type PolymarketAssetId =
  | "BTC"
  | "ETH"
  | "SOL"
  | "XRP"
  | "DOGE"
  | "HYPE"
  | "BNB";

export interface PolymarketAssetMeta {
  id: PolymarketAssetId;
  label: string;
  symbol: string;
}

export interface PolymarketPeriodMeta {
  id: PolymarketPeriodId;
  label: string;
  count: number;
}

export interface PolymarketPricePoint {
  timestamp: string;
  openPrice: number | null;
  chainlinkPrice: number | null;
}

export type PolymarketFairPriceStatus =
  | "connecting"
  | "live"
  | "stale"
  | "unavailable";

export interface PolymarketFairPricePoint {
  timestamp: string;
  sourceWallNS: string;
  ringEpoch: string;
  sequence: string;
  modelId: string;
  price: number;
  degraded: boolean;
  degradedReasons: string[];
}

export interface PolymarketFairPriceSource {
  profile: string;
  symbol: string;
}

export interface PolymarketMarket {
  id: string;
  conditionId: string;
  slug: string;
  asset: PolymarketAssetId;
  period: PolymarketPeriodId;
  title: string;
  windowStart: string;
  windowEnd: string;
  upTokenId: string;
  downTokenId: string;
  tickSize: string;
  negativeRisk: boolean;
  active: boolean;
}

export interface PolymarketMarketSnapshot {
  market: PolymarketMarket;
  openPrice: number | null;
  chainlinkPrice: number | null;
  upBid: number | null;
  upAsk: number | null;
  downBid: number | null;
  downAsk: number | null;
  priceSeries: PolymarketPricePoint[];
  sourceUpdatedAt: string;
  stale: boolean;
  version: string;
}

export interface PolymarketPosition {
  id: string;
  tradingAccountId: number;
  conditionId: string;
  tokenId: string;
  market: string;
  outcome: string;
  size: number;
  averagePrice: number;
  currentPrice: number;
  initialValue: number;
  currentValue: number;
  cashPnl: number;
  percentPnl: number;
  redeemable: boolean;
  sourceUpdatedAt: string;
}

export interface PolymarketAccountSummary {
  tradingAccountId: number;
  accountName: string;
  walletAddress: string;
  availableBalance: number;
  positionValue: number;
  totalAssets: number;
  sourceUpdatedAt: string;
  stale: boolean;
  bindingStatus: string;
}

export interface PolymarketOrder {
  id: string;
  clobOrderId: string;
  tradingAccountId: number;
  marketId: string;
  tokenId: string;
  outcome: "up" | "down";
  side: "buy" | "sell";
  requestedAmount: number;
  amountUnit: "usd" | "shares";
  executionType: "book" | "limit";
  limitPrice: number | null;
  clobOrderType: "FAK" | "GTC";
  filledSize: number;
  averagePrice: number;
  status: string;
  errorCode: string;
  errorMessage: string;
  createdAt: string;
  updatedAt: string;
}

export interface PolymarketOpenOrder {
  id: string;
  tokenId: string;
  conditionId: string;
  marketTitle: string;
  outcome: string;
  side: "buy" | "sell";
  price: number;
  originalSize: number;
  matchedSize: number;
  remainingSize: number;
  status: string;
  orderType: string;
  createdAt: string;
}

export const DEFAULT_POLYMARKET_PERIOD_ID: PolymarketPeriodId = "5m";
export const DEFAULT_POLYMARKET_ASSET_ID: PolymarketAssetId = "BTC";
