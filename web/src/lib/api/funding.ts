import type {
  Exchange,
  FundingHistoryPoint,
  FundingOpportunity,
  FundingSpread,
  FundingSpreadLeg,
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
  currentFundingRate: DecimalWire | null;
  nextFundingRate: DecimalWire | null;
  settlementIntervalHours: DecimalWire;
  nextFundingAt: string;
  cumulative24h: DecimalWire;
  cumulative7d: DecimalWire;
  latestPrice: DecimalWire;
  priceChange24h: DecimalWire;
  sourceUpdatedAt: string;
  stale?: boolean;
  venueContractType?: string;
  fundingHistory?: FundingHistoryWireDTO[];
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

export interface FundingHistoryWireResponse {
  data: FundingHistoryWireDTO[];
  meta: {
    exchange: string;
    exchangeSymbol: string;
    total: number;
  };
}

export interface FundingSpreadLegWireDTO {
  exchange: string;
  exchangeSymbol: string;
  globalSymbol: string;
  baseAsset: string;
  quoteAsset: string;
  fundingRate: DecimalWire;
  settlementIntervalHours: DecimalWire;
  nextFundingAt: string;
  positionNotional: DecimalWire;
  dailyVolume: DecimalWire;
  latestPrice: DecimalWire;
  sourceUpdatedAt: string;
  stale: boolean;
  venueContractType?: string;
}

export interface FundingSpreadWireDTO {
  id: string;
  symbol: string;
  baseAsset: string;
  quoteAsset: string;
  longLeg: FundingSpreadLegWireDTO;
  shortLeg: FundingSpreadLegWireDTO;
  spreadAnnualized: DecimalWire;
  spread24hAnnualized: DecimalWire;
  spread7dAnnualized: DecimalWire;
  minPositionNotional: DecimalWire;
  minDailyVolume: DecimalWire;
  sourceUpdatedAt: string;
  stale: boolean;
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

export type FundingRatesFetchResult =
  | { status: "updated"; snapshot: FundingSnapshot; etag: string | null }
  | { status: "unchanged"; etag: string | null };

export interface FundingSpreadSnapshot {
  data: FundingSpread[];
  meta: FundingSnapshot["meta"];
  hasStaleSources: boolean;
}

export type FundingSpreadsFetchResult =
  | { status: "updated"; snapshot: FundingSpreadSnapshot; etag: string | null }
  | { status: "unchanged"; etag: string | null };

const exchanges = new Set<Exchange>([
  "Binance",
  "OKX",
  "Bybit",
  "Bitget",
  "Gate",
  "Hyperliquid",
  "Aster",
  "Lighter",
  "Entropy",
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

function optionalBoolean(value: unknown, path: string): boolean | undefined {
  if (value === undefined) {
    return undefined;
  }
  if (typeof value !== "boolean") {
    throw new Error(`${path} 必须是 boolean`);
  }
  return value;
}

function venueContractType(value: unknown, path: string): string {
  if (value === undefined || value === null || value === "") {
    return "PERPETUAL";
  }
  return text(value, path);
}

function boolean(value: unknown, path: string): boolean {
  if (typeof value !== "boolean") {
    throw new Error(`${path} 必须是 boolean`);
  }
  return value;
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
  const currentFundingRate =
    item.currentFundingRate === null
      ? null
      : percentageRatio(item.currentFundingRate, `${path}.currentFundingRate`);
  const rawHistory = item.fundingHistory ?? [];
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
    exchangeSymbol,
    symbol: text(item.symbol, `${path}.symbol`),
    baseAsset: text(item.baseAsset, `${path}.baseAsset`),
    quoteAsset: text(item.quoteAsset, `${path}.quoteAsset`),
    positionQuantity: decimal(item.positionQuantity, `${path}.positionQuantity`),
    positionNotional: decimal(item.positionNotional, `${path}.positionNotional`),
    dailyVolume: decimal(item.dailyVolume, `${path}.dailyVolume`),
    annualizedRate: percentageRatio(item.annualizedRate, `${path}.annualizedRate`),
    currentFundingRate,
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
    history24hComplete: optionalBoolean(
      item.history24hComplete,
      `${path}.history24hComplete`,
    ),
    history7dComplete: optionalBoolean(
      item.history7dComplete,
      `${path}.history7dComplete`,
    ),
    venueContractType: venueContractType(
      item.venueContractType,
      `${path}.venueContractType`,
    ),
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

function mapFundingSpreadLeg(value: unknown, path: string): FundingSpreadLeg {
  const item = record(value, path);
  const exchangeSymbol = text(item.exchangeSymbol, `${path}.exchangeSymbol`);
  return {
    exchange: exchange(item.exchange, `${path}.exchange`),
    exchangeSymbol,
    globalSymbol: text(item.globalSymbol, `${path}.globalSymbol`),
    baseAsset: text(item.baseAsset, `${path}.baseAsset`),
    quoteAsset: text(item.quoteAsset, `${path}.quoteAsset`),
    fundingRate: percentageRatio(item.fundingRate, `${path}.fundingRate`),
    settlementIntervalHours: integer(
      item.settlementIntervalHours,
      `${path}.settlementIntervalHours`,
    ),
    nextSettlementAt: isoTime(item.nextFundingAt, `${path}.nextFundingAt`),
    positionNotional: decimal(item.positionNotional, `${path}.positionNotional`),
    dailyVolume: decimal(item.dailyVolume, `${path}.dailyVolume`),
    latestPrice: decimal(item.latestPrice, `${path}.latestPrice`),
    updatedAt: isoTime(item.sourceUpdatedAt, `${path}.sourceUpdatedAt`),
    stale: boolean(item.stale, `${path}.stale`),
    history24hComplete: optionalBoolean(
      item.history24hComplete,
      `${path}.history24hComplete`,
    ),
    history7dComplete: optionalBoolean(
      item.history7dComplete,
      `${path}.history7dComplete`,
    ),
    venueContractType: venueContractType(
      item.venueContractType,
      `${path}.venueContractType`,
    ),
  };
}

export function mapFundingSpreadDto(value: unknown, index: number): FundingSpread {
  const path = `data[${index}]`;
  const item = record(value, path);
  return {
    id: text(item.id, `${path}.id`),
    symbol: text(item.symbol, `${path}.symbol`),
    baseAsset: text(item.baseAsset, `${path}.baseAsset`),
    quoteAsset: text(item.quoteAsset, `${path}.quoteAsset`),
    longLeg: mapFundingSpreadLeg(item.longLeg, `${path}.longLeg`),
    shortLeg: mapFundingSpreadLeg(item.shortLeg, `${path}.shortLeg`),
    spreadAnnualized: percentageRatio(
      item.spreadAnnualized,
      `${path}.spreadAnnualized`,
    ),
    spread24hAnnualized: percentageRatio(
      item.spread24hAnnualized,
      `${path}.spread24hAnnualized`,
    ),
    spread7dAnnualized: percentageRatio(
      item.spread7dAnnualized,
      `${path}.spread7dAnnualized`,
    ),
    minPositionNotional: decimal(
      item.minPositionNotional,
      `${path}.minPositionNotional`,
    ),
    minDailyVolume: decimal(item.minDailyVolume, `${path}.minDailyVolume`),
    updatedAt: isoTime(item.sourceUpdatedAt, `${path}.sourceUpdatedAt`),
    stale: boolean(item.stale, `${path}.stale`),
    history24hComplete: optionalBoolean(
      item.history24hComplete,
      `${path}.history24hComplete`,
    ),
    history7dComplete: optionalBoolean(
      item.history7dComplete,
      `${path}.history7dComplete`,
    ),
  };
}

export function mapFundingSpreadsResponse(value: unknown): FundingSpreadSnapshot {
  const response = record(value, "response");
  if (!Array.isArray(response.data)) {
    throw new Error("response.data 必须是数组");
  }
  const meta = record(response.meta, "response.meta");
  const total = nonNegativeInteger(meta.total, "response.meta.total");
  const data = response.data.map(mapFundingSpreadDto);
  if (total !== data.length) {
    throw new Error(`response.meta.total (${total}) 与 data 长度不一致`);
  }
  return {
    data,
    meta: {
      total,
      snapshotVersion: version(
        meta.snapshotVersion,
        "response.meta.snapshotVersion",
      ),
      serverTime: isoTime(meta.serverTime, "response.meta.serverTime"),
    },
    hasStaleSources: data.some((item) => item.stale),
  };
}

export function mapFundingHistoryResponse(value: unknown): FundingHistoryPoint[] {
  const response = record(value, "response");
  if (!Array.isArray(response.data)) {
    throw new Error("response.data 必须是数组");
  }
  const meta = record(response.meta, "response.meta");
  const total = nonNegativeInteger(meta.total, "response.meta.total");
  const data = response.data.map((point, index) =>
    historyPoint(point, `data[${index}]`),
  );
  if (total !== data.length) {
    throw new Error(`response.meta.total (${total}) 与 data 长度不一致`);
  }
  return data;
}

function fundingRatesUrl() {
  const baseUrl = (process.env.NEXT_PUBLIC_API_BASE_URL ?? "").replace(/\/+$/, "");
  return `${baseUrl}/api/v1/funding-rates`;
}

function fundingSpreadsUrl() {
  const baseUrl = (process.env.NEXT_PUBLIC_API_BASE_URL ?? "").replace(/\/+$/, "");
  return `${baseUrl}/api/v1/funding-spreads`;
}

export const FUNDING_RATES_LOOKUP_MAX_KEYS = 256;

export type FundingRateLookupKey = {
  exchange: string;
  exchangeSymbol: string;
  baseAsset: string;
  quoteAsset: string;
};

export type FundingRateLookupResultRow = {
  key: string;
  status: "hit" | "missing";
  item?: FundingOpportunity;
};

export type FundingRatesLookupResult = {
  results: FundingRateLookupResultRow[];
  snapshotVersion: string;
  serverTime: string;
};

export function fundingRequestKey(
  instrument: Pick<{ exchange: string; exchangeSymbol: string }, "exchange" | "exchangeSymbol">,
): string {
  return `${instrument.exchange.trim().toLowerCase()}|${instrument.exchangeSymbol.trim().toLowerCase()}`;
}

export function mapFundingRatesLookupResponse(
  value: unknown,
  requested: FundingRateLookupKey[],
): FundingRatesLookupResult {
  const response = record(value, "response");
  if (!Array.isArray(response.results)) {
    throw new Error("response.results 必须是数组");
  }
  if (response.results.length !== requested.length) {
    throw new Error("response.results 与请求 keys 数量不一致");
  }
  const meta = record(response.meta, "response.meta");
  const results = response.results.map((row, index) => {
    const item = record(row, `results[${index}]`);
    const rawStatus = text(item.status, `results[${index}].status`);
    if (rawStatus !== "hit" && rawStatus !== "missing") {
      throw new Error(`results[${index}].status 必须是 hit 或 missing`);
    }
    const status: "hit" | "missing" = rawStatus;
    const requestKey = fundingRequestKey(requested[index]!);
    if (status === "hit") {
      if (item.item === undefined || item.item === null) {
        throw new Error(`results[${index}].item 缺失`);
      }
      return {
        key: requestKey,
        status,
        item: mapFundingDto(item.item, index),
      };
    }
    return { key: requestKey, status };
  });
  return {
    results,
    snapshotVersion: version(
      meta.snapshotVersion,
      "response.meta.snapshotVersion",
    ),
    serverTime: isoTime(meta.serverTime, "response.meta.serverTime"),
  };
}

export async function fetchFundingRates(
  etag?: string | null,
  signal?: AbortSignal,
): Promise<FundingRatesFetchResult> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (etag) {
    headers["If-None-Match"] = etag;
  }
  const response = await fetch(fundingRatesUrl(), {
    method: "GET",
    headers,
    cache: "no-store",
    signal,
  });
  if (response.status === 304) {
    return {
      status: "unchanged",
      etag: response.headers.get("ETag") ?? etag ?? null,
    };
  }
  if (!response.ok) {
    throw new Error(`资金费 API 请求失败 (${response.status})`);
  }
  return {
    status: "updated",
    snapshot: mapFundingRatesResponse(await response.json()),
    etag: response.headers.get("ETag"),
  };
}

export async function fetchFundingRatesLookup(
  keys: FundingRateLookupKey[],
  signal?: AbortSignal,
): Promise<FundingRatesLookupResult> {
  if (keys.length === 0 || keys.length > FUNDING_RATES_LOOKUP_MAX_KEYS) {
    throw new Error("资金费 lookup keys 数量无效");
  }
  const response = await fetch(`${fundingRatesUrl()}/lookup`, {
    method: "POST",
    credentials: "include",
    headers: {
      Accept: "application/json",
      "Content-Type": "application/json",
    },
    cache: "no-store",
    signal,
    body: JSON.stringify({ keys }),
  });
  if (response.status === 401) {
    throw new Error("请先登录");
  }
  if (!response.ok) {
    throw new Error(`资金费 lookup 请求失败 (${response.status})`);
  }
  return mapFundingRatesLookupResponse(await response.json(), keys);
}

export async function fetchFundingSpreads(
  etag?: string | null,
  signal?: AbortSignal,
): Promise<FundingSpreadsFetchResult> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (etag) headers["If-None-Match"] = etag;
  const response = await fetch(fundingSpreadsUrl(), {
    method: "GET",
    headers,
    cache: "no-store",
    signal,
  });
  if (response.status === 304) {
    return {
      status: "unchanged",
      etag: response.headers.get("ETag") ?? etag ?? null,
    };
  }
  if (!response.ok) {
    throw new Error(`跨所资金费 API 请求失败 (${response.status})`);
  }
  return {
    status: "updated",
    snapshot: mapFundingSpreadsResponse(await response.json()),
    etag: response.headers.get("ETag"),
  };
}

export async function fetchFundingHistory(
  exchangeName: string,
  exchangeSymbol: string,
  signal?: AbortSignal,
): Promise<FundingHistoryPoint[]> {
  const url = `${fundingRatesUrl()}/${encodeURIComponent(exchangeName)}/${encodeURIComponent(exchangeSymbol)}/history?limit=10`;
  const response = await fetch(url, {
    method: "GET",
    headers: { Accept: "application/json" },
    cache: "no-store",
    signal,
  });
  if (!response.ok) {
    throw new Error(`历史资金费 API 请求失败 (${response.status})`);
  }
  return mapFundingHistoryResponse(await response.json());
}
