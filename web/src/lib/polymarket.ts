import type {
  PolymarketAssetId,
  PolymarketAssetMeta,
  PolymarketPeriodId,
  PolymarketPeriodMeta,
} from "../types/polymarket";
import {
  DEFAULT_POLYMARKET_ASSET_ID,
  DEFAULT_POLYMARKET_PERIOD_ID,
} from "../types/polymarket";

interface LegacyPricePoint {
  timestamp: string;
  open: number;
  chainlink: number;
  fairPrice: number;
}

interface LegacySnapshot {
  periodId: PolymarketPeriodId;
  assetId: PolymarketAssetId;
  asset: string;
  assetLabel: string;
  title: string;
  windowLabel: string;
  countdownSeconds: number;
  openPrice: number;
  chainlinkPrice: number;
  fairPrice: number;
  priceDelta: number;
  upProbability: number;
  upQuoteCents: number;
  downQuoteCents: number | null;
  priceSeries: LegacyPricePoint[];
}

const ASSET_CONFIG: Record<
  PolymarketAssetId,
  { label: string; symbol: string; basePrice: number; phase: number }
> = {
  BTC: { label: "比特币", symbol: "₿", basePrice: 64962.57, phase: 0 },
  ETH: { label: "以太坊", symbol: "Ξ", basePrice: 3491.36, phase: 1 },
  SOL: { label: "Solana", symbol: "◎", basePrice: 163.924, phase: 2 },
  XRP: { label: "瑞波币", symbol: "✕", basePrice: 0.6234, phase: 3 },
  DOGE: { label: "狗狗币", symbol: "Ð", basePrice: 0.1842, phase: 4 },
  HYPE: { label: "Hyperliquid", symbol: "H", basePrice: 42.15, phase: 5 },
  BNB: { label: "币安币", symbol: "B", basePrice: 582.4, phase: 6 },
};

const PERIOD_CONFIG: Record<
  PolymarketPeriodId,
  { label: string; count: number; windowMinutes: number; phase: number }
> = {
  "5m": { label: "5 分钟", count: 7, windowMinutes: 5, phase: 0 },
  "15m": { label: "15 分钟", count: 7, windowMinutes: 15, phase: 1 },
  "1h": { label: "1 小时", count: 9, windowMinutes: 60, phase: 2 },
  "4h": { label: "4 小时", count: 7, windowMinutes: 240, phase: 3 },
  "1d": { label: "每天", count: 11, windowMinutes: 1440, phase: 4 },
  "1w": { label: "每周", count: 63, windowMinutes: 10080, phase: 5 },
  "1mo": { label: "每月", count: 23, windowMinutes: 43200, phase: 6 },
  "1y": { label: "每年", count: 27, windowMinutes: 525600, phase: 7 },
};

function priceDigits(value: number) {
  if (value >= 1000) return 2;
  if (value >= 1) return 4;
  return 6;
}

function buildPriceSeries(
  openPrice: number,
  phase: number,
  points = 25,
): LegacyPricePoint[] {
  const digits = priceDigits(openPrice);
  const driftScale = Math.max(openPrice * 0.00045, 0.0002);
  const base = Date.UTC(2026, 7, 8, 7, 35, 0);
  return Array.from({ length: points }, (_, index) => {
    const ratio = index / Math.max(points - 1, 1);
    const chainlinkDrift =
      Math.sin((index + phase) * 0.55) * driftScale * 40 +
      Math.cos((index + phase) * 0.31) * driftScale * 20 -
      ratio * driftScale * 75;
    const fairDrift =
      Math.sin((index + phase) * 0.42) * driftScale * 28 +
      Math.cos((index + phase) * 0.18) * driftScale * 14 -
      ratio * driftScale * 62;
    const chainlink = Number((openPrice + chainlinkDrift).toFixed(digits));
    const fairPrice = Number((openPrice + fairDrift).toFixed(digits));
    return {
      timestamp: new Date(base + index * 12_000).toISOString(),
      open: openPrice,
      chainlink,
      fairPrice,
    };
  });
}

function buildSnapshot(
  periodId: PolymarketPeriodId,
  assetId: PolymarketAssetId,
): LegacySnapshot {
  const period = PERIOD_CONFIG[periodId];
  const asset = ASSET_CONFIG[assetId];
  const phase = period.phase + asset.phase;
  const openPrice = Number(
    (asset.basePrice + period.phase * asset.basePrice * 0.0018).toFixed(
      priceDigits(asset.basePrice),
    ),
  );
  const priceSeries = buildPriceSeries(openPrice, phase);
  const last = priceSeries[priceSeries.length - 1];
  const chainlinkPrice = last.chainlink;
  const fairPrice = last.fairPrice;
  const priceDelta = Number(
    (chainlinkPrice - openPrice).toFixed(priceDigits(asset.basePrice)),
  );
  const upProbability = Math.max(
    8,
    Math.min(92, Math.round(50 + priceDelta * (asset.basePrice >= 100 ? 0.85 : 120))),
  );

  return {
    periodId,
    assetId,
    asset: assetId,
    assetLabel: asset.label,
    title: `${assetId} ${period.label}上涨或下跌`,
    windowLabel: `八月 8, 上午 3:35 - 上午 3:40 ET · ${period.label}`,
    countdownSeconds: Math.max(35, 300 - period.phase * 18),
    openPrice,
    chainlinkPrice,
    fairPrice,
    priceDelta,
    upProbability,
    upQuoteCents: Math.max(1, Math.min(99, upProbability)),
    downQuoteCents: upProbability >= 99 ? null : Math.max(1, 100 - upProbability),
    priceSeries,
  };
}

export const polymarketAssets: PolymarketAssetMeta[] = (
  Object.keys(ASSET_CONFIG) as PolymarketAssetId[]
).map((id) => ({
  id,
  label: ASSET_CONFIG[id].label,
  symbol: ASSET_CONFIG[id].symbol,
}));

export const polymarketPeriods: PolymarketPeriodMeta[] = (
  Object.keys(PERIOD_CONFIG) as PolymarketPeriodId[]
).map((id) => ({
  id,
  label: PERIOD_CONFIG[id].label,
  count: PERIOD_CONFIG[id].count,
}));

export function getPolymarketSnapshot(
  periodId: PolymarketPeriodId = DEFAULT_POLYMARKET_PERIOD_ID,
  assetId: PolymarketAssetId = DEFAULT_POLYMARKET_ASSET_ID,
): LegacySnapshot {
  if (!(periodId in PERIOD_CONFIG)) {
    throw new Error(`unknown polymarket period: ${periodId}`);
  }
  if (!(assetId in ASSET_CONFIG)) {
    throw new Error(`unknown polymarket asset: ${assetId}`);
  }
  return buildSnapshot(periodId, assetId);
}

export function getDefaultPolymarketPeriodId(): PolymarketPeriodId {
  return DEFAULT_POLYMARKET_PERIOD_ID;
}

export function getDefaultPolymarketAssetId(): PolymarketAssetId {
  return DEFAULT_POLYMARKET_ASSET_ID;
}

export function getAssetSymbol(assetId: PolymarketAssetId): string {
  return ASSET_CONFIG[assetId].symbol;
}

export const polymarketAccount = {
  accountName: "poly_demo",
  assetsUsd: 1250.48,
};

export const polymarketPositions = [
  {
    id: "btc-5m-up",
    market: "BTC 5分钟 · Up",
    avgPriceCents: 49,
    currentPriceCents: 52,
    tradeAmountUsd: 120,
    profitUsd: 7.35,
    valueUsd: 127.35,
  },
  {
    id: "btc-5m-down",
    market: "BTC 5分钟 · Down",
    avgPriceCents: 51,
    currentPriceCents: 48,
    tradeAmountUsd: 80,
    profitUsd: -4.71,
    valueUsd: 75.29,
  },
  {
    id: "eth-15m-up",
    market: "ETH 15分钟 · Up",
    avgPriceCents: 44,
    currentPriceCents: 47,
    tradeAmountUsd: 65,
    profitUsd: 4.43,
    valueUsd: 69.43,
  },
  {
    id: "sol-5m-up",
    market: "SOL 5分钟 · Up",
    avgPriceCents: 38,
    currentPriceCents: 41,
    tradeAmountUsd: 50,
    profitUsd: 3.95,
    valueUsd: 53.95,
  },
  {
    id: "xrp-1h-down",
    market: "XRP 1小时 · Down",
    avgPriceCents: 56,
    currentPriceCents: 53,
    tradeAmountUsd: 40,
    profitUsd: 2.14,
    valueUsd: 42.14,
  },
  {
    id: "doge-5m-up",
    market: "DOGE 5分钟 · Up",
    avgPriceCents: 33,
    currentPriceCents: 31,
    tradeAmountUsd: 25,
    profitUsd: -1.52,
    valueUsd: 23.48,
  },
  {
    id: "hype-15m-up",
    market: "HYPE 15分钟 · Up",
    avgPriceCents: 42,
    currentPriceCents: 45,
    tradeAmountUsd: 90,
    profitUsd: 6.43,
    valueUsd: 96.43,
  },
  {
    id: "bnb-4h-down",
    market: "BNB 4小时 · Down",
    avgPriceCents: 58,
    currentPriceCents: 55,
    tradeAmountUsd: 110,
    profitUsd: 5.69,
    valueUsd: 115.69,
  },
  {
    id: "btc-1d-up",
    market: "BTC 每天 · Up",
    avgPriceCents: 47,
    currentPriceCents: 50,
    tradeAmountUsd: 200,
    profitUsd: 12.77,
    valueUsd: 212.77,
  },
  {
    id: "eth-5m-down",
    market: "ETH 5分钟 · Down",
    avgPriceCents: 54,
    currentPriceCents: 57,
    tradeAmountUsd: 35,
    profitUsd: -1.94,
    valueUsd: 33.06,
  },
];
