import type {
  Exchange,
  FundingHistoryPoint,
  FundingOpportunity,
} from "@/types/market";

export type DecimalWire = string | number;

export interface FundingHistoryWireDTO {
  rate: DecimalWire;
  settledAt: string;
}

export interface FundingWireDTO {
  id?: string;
  exchange: string;
  exchangeSymbol: string;
  symbol: string;
  baseAsset: string;
  quoteAsset: string;
  positionQuantity: DecimalWire;
  positionNotional: DecimalWire;
  dailyVolume: DecimalWire;
  annualizedRate: DecimalWire;
  currentFundingRate: DecimalWire;
  nextFundingRate: DecimalWire | null;
  settlementIntervalHours: DecimalWire;
  nextFundingAt: string;
  cumulative24h: DecimalWire;
  cumulative7d: DecimalWire;
  latestPrice: DecimalWire;
  priceChange24h: DecimalWire;
  sourceUpdatedAt: string;
  stale?: boolean;
  fundingHistory: FundingHistoryWireDTO[];
  index?: {
    name: string;
    value: DecimalWire;
    weight: DecimalWire;
  };
}

export interface FundingRatesWireResponse {
  data: FundingWireDTO[];
  meta: {
    total: number;
    snapshotVersion: string | number;
    serverTime: string;
  };
}

export interface FundingSnapshot {
  data: FundingOpportunity[];
  meta: {
    total: number;
    snapshotVersion: string;
    serverTime: string;
  };
  hasStaleSources: boolean;
}

const exchanges = new Set<Exchange>([
  "Binance",
  "OKX",
  "Bybit",
  "Bitget",
  "Gate",
  "Hyperliquid",
]);

function record(value: unknown, path: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`${path} 必须是对象`);
  }
  return value as Record<string, unknown>;
}

function text(value: unknown, path: string): string {
  if (typeof value !== "string" || value.length === 0) {
    throw new Error(`${path} 必须是非空字符串`);
  }
  return value;
}

function decimal(value: unknown, path: string): number {
  if (
    (typeof value !== "string" && typeof value !== "number") ||
    (typeof value === "string" && value.trim() === "")
  ) {
    throw new Error(`${path} 必须是 decimal string 或 number`);
  }
  const parsed = typeof value === "number" ? value : Number(value);
  if (!Number.isFinite(parsed)) {
    throw new Error(`${path} 不是有限十进制数`);
  }
  return parsed;
}

function integer(value: unknown, path: string): number {
  const parsed = decimal(value, path);
  if (!Number.isSafeInteger(parsed) || parsed <= 0) {
    throw new Error(`${path} 必须是正安全整数`);
  }
  return parsed;
}

function nonNegativeInteger(value: unknown, path: string): number {
  const parsed = decimal(value, path);
  if (!Number.isSafeInteger(parsed) || parsed < 0) {
    throw new Error(`${path} 必须是非负安全整数`);
  }
  return parsed;
}

function isoTime(value: unknown, path: string): string {
  const parsed = text(value, path);
  if (!Number.isFinite(Date.parse(parsed))) {
    throw new Error(`${path} 必须是有效 ISO 时间`);
  }
  return parsed;
}

function exchange(value: unknown, path: string): Exchange {
  const parsed = text(value, path) as Exchange;
  if (!exchanges.has(parsed)) {
    throw new Error(`${path} 不是受支持的交易所`);
  }
  return parsed;
}

function percentageRatio(value: unknown, path: string): number {
  return decimal(value, path) * 100;
}

function version(value: unknown, path: string): string {
  if (
    (typeof value !== "string" && typeof value !== "number") ||
    String(value).length === 0
  ) {
    throw new Error(`${path} 必须是非空 string 或 number`);
  }
  return String(value);
}

function historyPoint(value: unknown, path: string): FundingHistoryPoint {
  const item = record(value, path);
  return {
    rate: percentageRatio(item.rate, `${path}.rate`),
    settledAt: isoTime(item.settledAt, `${path}.settledAt`),
  };
}

export function mapFundingDto(
  value: unknown,
  index: number,
): FundingOpportunity {
  const path = `data[${index}]`;
  const item = record(value, path);
  const venue = exchange(item.exchange, `${path}.exchange`);
  const exchangeSymbol = text(item.exchangeSymbol, `${path}.exchangeSymbol`);
  const nextFundingRate =
    item.nextFundingRate === null
      ? null
      : percentageRatio(item.nextFundingRate, `${path}.nextFundingRate`);
  const rawHistory = item.fundingHistory;
  if (!Array.isArray(rawHistory)) {
    throw new Error(`${path}.fundingHistory 必须是数组`);
  }

  const indexDto =
    item.index === undefined ? undefined : record(item.index, `${path}.index`);

  return {
    id:
      item.id === undefined
        ? `${venue.toLowerCase()}-${exchangeSymbol.toLowerCase()}`
        : text(item.id, `${path}.id`),
    exchange: venue,
    symbol: text(item.symbol, `${path}.symbol`),
    baseAsset: text(item.baseAsset, `${path}.baseAsset`),
    quoteAsset: text(item.quoteAsset, `${path}.quoteAsset`),
    positionQuantity: decimal(item.positionQuantity, `${path}.positionQuantity`),
    positionNotional: decimal(item.positionNotional, `${path}.positionNotional`),
    dailyVolume: decimal(item.dailyVolume, `${path}.dailyVolume`),
    annualizedRate: percentageRatio(item.annualizedRate, `${path}.annualizedRate`),
    currentFundingRate: percentageRatio(
      item.currentFundingRate,
      `${path}.currentFundingRate`,
    ),
    nextFundingRate,
    settlementIntervalHours: integer(
      item.settlementIntervalHours,
      `${path}.settlementIntervalHours`,
    ),
    nextSettlementAt: isoTime(item.nextFundingAt, `${path}.nextFundingAt`),
    cumulative24h: percentageRatio(item.cumulative24h, `${path}.cumulative24h`),
    cumulative7d: percentageRatio(item.cumulative7d, `${path}.cumulative7d`),
    latestPrice: decimal(item.latestPrice, `${path}.latestPrice`),
    priceChange24h: percentageRatio(
      item.priceChange24h,
      `${path}.priceChange24h`,
    ),
    updatedAt: isoTime(item.sourceUpdatedAt, `${path}.sourceUpdatedAt`),
    stale:
      item.stale === undefined
        ? false
        : typeof item.stale === "boolean"
          ? item.stale
          : (() => {
              throw new Error(`${path}.stale 必须是 boolean`);
            })(),
    fundingHistory: rawHistory.map((point, historyIndex) =>
      historyPoint(point, `${path}.fundingHistory[${historyIndex}]`),
    ),
    index: indexDto
      ? {
          name: text(indexDto.name, `${path}.index.name`),
          value: decimal(indexDto.value, `${path}.index.value`),
          weight: decimal(indexDto.weight, `${path}.index.weight`),
        }
      : {
          name: `${venue.toUpperCase()}_INDEX`,
          value: 0,
          weight: 0,
        },
  };
}

export function mapFundingRatesResponse(value: unknown): FundingSnapshot {
  const response = record(value, "response");
  if (!Array.isArray(response.data)) {
    throw new Error("response.data 必须是数组");
  }
  const meta = record(response.meta, "response.meta");
  const total = nonNegativeInteger(meta.total, "response.meta.total");
  const snapshotVersion = version(
    meta.snapshotVersion,
    "response.meta.snapshotVersion",
  );
  const serverTime = isoTime(meta.serverTime, "response.meta.serverTime");
  const data = response.data.map(mapFundingDto);
  if (total !== data.length) {
    throw new Error(`response.meta.total (${total}) 与 data 长度不一致`);
  }

  return {
    data,
    meta: { total, snapshotVersion, serverTime },
    hasStaleSources: data.some((item) => item.stale),
  };
}

function fundingRatesUrl() {
  const baseUrl = (process.env.NEXT_PUBLIC_API_BASE_URL ?? "").replace(/\/+$/, "");
  return `${baseUrl}/api/v1/funding-rates`;
}

export async function fetchFundingRates(
  signal?: AbortSignal,
): Promise<FundingSnapshot> {
  const response = await fetch(fundingRatesUrl(), {
    method: "GET",
    headers: { Accept: "application/json" },
    cache: "no-store",
    signal,
  });
  if (!response.ok) {
    throw new Error(`资金费 API 请求失败 (${response.status})`);
  }
  return mapFundingRatesResponse(await response.json());
}
