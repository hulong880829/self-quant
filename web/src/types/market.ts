export type Exchange =
  | "Binance"
  | "OKX"
  | "Bybit"
  | "Bitget"
  | "Gate"
  | "Hyperliquid";

export interface FundingHistoryPoint {
  rate: number;
  settledAt: string;
}

export interface IndexInfo {
  name: string;
  value: number;
  weight: number;
}

export interface FundingOpportunity {
  id: string;
  exchange: Exchange;
  symbol: string;
  baseAsset: string;
  quoteAsset: string;
  positionQuantity: number;
  positionNotional: number;
  dailyVolume: number;
  annualizedRate: number;
  currentFundingRate: number;
  nextFundingRate: number | null;
  settlementIntervalHours: number;
  nextSettlementAt: string;
  cumulative24h: number;
  cumulative7d: number;
  latestPrice: number;
  priceChange24h: number;
  updatedAt: string;
  stale?: boolean;
  fundingHistory: FundingHistoryPoint[];
  index: IndexInfo;
}

export type RateDirection = "all" | "positive" | "negative";

export interface FundingFilters {
  search: string;
  minPositionNotional: number;
  minDailyVolume: number;
  intervalHours: number | "all";
  exchanges: Exchange[];
  direction: RateDirection;
}
