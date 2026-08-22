export type Exchange =
  | "Binance"
  | "OKX"
  | "Bybit"
  | "Bitget"
  | "Gate"
  | "Hyperliquid"
  | "Polymarket";

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
  exchangeSymbol: string;
  symbol: string;
  baseAsset: string;
  quoteAsset: string;
  positionQuantity: number;
  positionNotional: number;
  dailyVolume: number;
  annualizedRate: number;
  currentFundingRate: number | null;
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

export interface FundingSpreadLeg {
  exchange: Exchange;
  exchangeSymbol: string;
  fundingRate: number;
  settlementIntervalHours: number;
  nextSettlementAt: string;
  positionNotional: number;
  dailyVolume: number;
  latestPrice: number;
  updatedAt: string;
  stale: boolean;
}

export interface FundingSpread {
  id: string;
  symbol: string;
  baseAsset: string;
  quoteAsset: string;
  longLeg: FundingSpreadLeg;
  shortLeg: FundingSpreadLeg;
  spreadAnnualized: number;
  spread24hAnnualized: number;
  spread7dAnnualized: number;
  minPositionNotional: number;
  minDailyVolume: number;
  updatedAt: string;
  stale: boolean;
}

export type FundingOpportunityPeriod = "1h" | "4h" | "8h" | "24h";

export interface RankedFundingOpportunity {
  id: string;
  rank: number;
  symbol: string;
  baseAsset: string;
  quoteAsset: string;
  period: FundingOpportunityPeriod;
  longLeg: FundingSpreadLeg;
  shortLeg: FundingSpreadLeg;
  currentMidSpreadBps: number;
  currentExecutableSpreadBps: number;
  targetSpreadBps: number;
  periodExpectedReturn: number;
  fundingExpectedAnnualized: number;
  spreadExpectedAnnualized: number;
  combinedExpectedAnnualized: number;
  firstPassageProbability: number;
  profitProbability: number;
  expectedExitMinutes: number;
  p5Return: number;
  minPositionNotional: number;
  minDailyVolume: number;
  coverage: number;
  confidence: number;
  modelState: string;
  updatedAt: string;
  stale: boolean;
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
