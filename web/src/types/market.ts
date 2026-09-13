export type Exchange =
  | "Binance"
  | "OKX"
  | "Bybit"
  | "Bitget"
  | "Gate"
  | "Hyperliquid"
  | "Aster"
  | "Lighter"
  | "Entropy"
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
  history24hComplete?: boolean;
  history7dComplete?: boolean;
  venueContractType?: string;
  fundingHistory: FundingHistoryPoint[];
  index: IndexInfo;
}

export interface FundingSpreadLeg {
  exchange: Exchange;
  exchangeSymbol: string;
  globalSymbol: string;
  baseAsset: string;
  quoteAsset: string;
  fundingRate: number;
  settlementIntervalHours: number;
  nextSettlementAt: string;
  positionNotional: number;
  dailyVolume: number;
  latestPrice: number;
  updatedAt: string;
  stale: boolean;
  history24hComplete?: boolean;
  history7dComplete?: boolean;
  venueContractType?: string;
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
  history24hComplete?: boolean;
  history7dComplete?: boolean;
}

export type FundingOpportunityPeriod = "8h" | "24h";
export type PaybackStatus = "ready" | "never" | "insufficient_sample";

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
  sampleCount: number;
  expectedPaybackMinutes: number | null;
  paybackStatus: PaybackStatus;
  updatedAt: string;
  stale: boolean;
}

export type RateDirection = "all" | "positive" | "negative";

export type ContractKind = "crypto" | "tradifi" | "hip3";

export interface FundingFilters {
  search: string;
  minPositionNotional: number;
  minDailyVolume: number;
  intervalHours: number | "all";
  exchanges: Exchange[];
  contractKinds: ContractKind[];
  direction: RateDirection;
}
