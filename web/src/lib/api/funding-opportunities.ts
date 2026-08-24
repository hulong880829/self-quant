import type {
  Exchange,
  FundingOpportunityPeriod,
  FundingSpreadLeg,
  RankedFundingOpportunity,
} from "@/types/market";

export type DecimalWire = string | number;

export interface FundingOpportunityQuery {
  period: FundingOpportunityPeriod;
  minPositionNotional: number;
  minDailyVolume: number;
}

export interface FundingOpportunitySnapshot {
  data: RankedFundingOpportunity[];
  meta: {
    total: number;
    snapshotVersion: string;
    serverTime: string;
    calculatedAt: string;
    stale: boolean;
    status: "ready" | "warming" | "stale" | "unavailable";
    lastSuccessfulAt: string;
    dataThrough: string;
    generation: number;
  };
}

export type FundingOpportunitiesFetchResult =
  | { status: "updated"; snapshot: FundingOpportunitySnapshot; etag: string | null }
  | { status: "unchanged"; etag: string | null };

const exchanges = new Set<Exchange>([
  "Binance",
  "OKX",
  "Bybit",
  "Bitget",
  "Gate",
  "Hyperliquid",
]);
const periods = new Set<FundingOpportunityPeriod>(["1h", "4h", "8h", "24h"]);

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
  const parsed = Number(value);
  if (!Number.isFinite(parsed)) throw new Error(`${path} 不是有限十进制数`);
  return parsed;
}

function integer(value: unknown, path: string, allowZero = false): number {
  const parsed = decimal(value, path);
  if (!Number.isSafeInteger(parsed) || parsed < (allowZero ? 0 : 1)) {
    throw new Error(`${path} 必须是${allowZero ? "非负" : "正"}安全整数`);
  }
  return parsed;
}

function boolean(value: unknown, path: string): boolean {
  if (typeof value !== "boolean") throw new Error(`${path} 必须是 boolean`);
  return value;
}

function snapshotStatus(
  value: unknown,
  stale: boolean,
): FundingOpportunitySnapshot["meta"]["status"] {
  if (value === undefined || value === "") return stale ? "stale" : "ready";
  if (value === "ready" || value === "warming" || value === "stale" || value === "unavailable") {
    return value;
  }
  throw new Error("response.meta.status 不是支持的快照状态");
}

function isoTime(value: unknown, path: string): string {
  const parsed = text(value, path);
  if (!Number.isFinite(Date.parse(parsed))) throw new Error(`${path} 必须是有效 ISO 时间`);
  return parsed;
}

function ratio(value: unknown, path: string): number {
  return decimal(value, path) * 100;
}

function exchange(value: unknown, path: string): Exchange {
  const parsed = text(value, path) as Exchange;
  if (!exchanges.has(parsed)) throw new Error(`${path} 不是受支持的交易所`);
  return parsed;
}

function period(value: unknown, path: string): FundingOpportunityPeriod {
  const parsed = text(value, path) as FundingOpportunityPeriod;
  if (!periods.has(parsed)) throw new Error(`${path} 不是支持的排名周期`);
  return parsed;
}

function version(value: unknown, path: string): string {
  if ((typeof value !== "string" && typeof value !== "number") || String(value).length === 0) {
    throw new Error(`${path} 必须是非空 string 或 number`);
  }
  return String(value);
}

function mapLeg(value: unknown, path: string): FundingSpreadLeg {
  const item = record(value, path);
  return {
    exchange: exchange(item.exchange, `${path}.exchange`),
    exchangeSymbol: text(item.exchangeSymbol, `${path}.exchangeSymbol`),
    fundingRate: ratio(item.fundingRate, `${path}.fundingRate`),
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
  };
}

export function mapFundingOpportunity(
  value: unknown,
  index: number,
): RankedFundingOpportunity {
  const path = `data[${index}]`;
  const item = record(value, path);
  return {
    id: text(item.id, `${path}.id`),
    rank: integer(item.rank, `${path}.rank`),
    symbol: text(item.symbol, `${path}.symbol`),
    baseAsset: text(item.baseAsset, `${path}.baseAsset`),
    quoteAsset: text(item.quoteAsset, `${path}.quoteAsset`),
    period: period(item.period, `${path}.period`),
    longLeg: mapLeg(item.longLeg, `${path}.longLeg`),
    shortLeg: mapLeg(item.shortLeg, `${path}.shortLeg`),
    currentMidSpreadBps: decimal(item.currentMidSpreadBps, `${path}.currentMidSpreadBps`),
    currentExecutableSpreadBps: decimal(
      item.currentExecutableSpreadBps,
      `${path}.currentExecutableSpreadBps`,
    ),
    targetSpreadBps: decimal(item.targetSpreadBps, `${path}.targetSpreadBps`),
    periodExpectedReturn: ratio(item.periodExpectedReturn, `${path}.periodExpectedReturn`),
    fundingExpectedAnnualized: ratio(
      item.fundingExpectedAnnualized,
      `${path}.fundingExpectedAnnualized`,
    ),
    spreadExpectedAnnualized: ratio(
      item.spreadExpectedAnnualized,
      `${path}.spreadExpectedAnnualized`,
    ),
    combinedExpectedAnnualized: ratio(
      item.combinedExpectedAnnualized,
      `${path}.combinedExpectedAnnualized`,
    ),
    firstPassageProbability: ratio(
      item.firstPassageProbability,
      `${path}.firstPassageProbability`,
    ),
    profitProbability: ratio(item.profitProbability, `${path}.profitProbability`),
    expectedExitMinutes: decimal(item.expectedExitMinutes, `${path}.expectedExitMinutes`),
    p5Return: ratio(item.p5Return, `${path}.p5Return`),
    minPositionNotional: decimal(item.minPositionNotional, `${path}.minPositionNotional`),
    minDailyVolume: decimal(item.minDailyVolume, `${path}.minDailyVolume`),
    coverage: ratio(item.coverage, `${path}.coverage`),
    confidence: ratio(item.confidence, `${path}.confidence`),
    modelState: text(item.modelState, `${path}.modelState`),
    updatedAt: isoTime(item.sourceUpdatedAt, `${path}.sourceUpdatedAt`),
    stale: boolean(item.stale, `${path}.stale`),
  };
}

export function mapFundingOpportunitiesResponse(value: unknown): FundingOpportunitySnapshot {
  const response = record(value, "response");
  if (!Array.isArray(response.data)) throw new Error("response.data 必须是数组");
  const meta = record(response.meta, "response.meta");
  const data = response.data.map(mapFundingOpportunity);
  const total = integer(meta.total, "response.meta.total", true);
  if (total < data.length) {
    throw new Error(`response.meta.total (${total}) 小于 data 长度`);
  }
  const serverTime = isoTime(meta.serverTime, "response.meta.serverTime");
  const calculatedAt =
    typeof meta.calculatedAt === "string" && meta.calculatedAt.length > 0
      ? isoTime(meta.calculatedAt, "response.meta.calculatedAt")
      : serverTime;
  const stale = boolean(meta.stale, "response.meta.stale");
  const lastSuccessfulAt =
    typeof meta.lastSuccessfulAt === "string" && meta.lastSuccessfulAt.length > 0
      ? isoTime(meta.lastSuccessfulAt, "response.meta.lastSuccessfulAt")
      : calculatedAt;
  const dataThrough =
    typeof meta.dataThrough === "string" && meta.dataThrough.length > 0
      ? isoTime(meta.dataThrough, "response.meta.dataThrough")
      : calculatedAt;
  return {
    data,
    meta: {
      total,
      snapshotVersion:
        typeof meta.snapshotVersion === "string" && meta.snapshotVersion.length === 0
          ? "unavailable"
          : version(meta.snapshotVersion, "response.meta.snapshotVersion"),
      serverTime,
      calculatedAt,
      stale,
      status: snapshotStatus(meta.status, stale),
      lastSuccessfulAt,
      dataThrough,
      generation:
        meta.generation === undefined
          ? 0
          : integer(meta.generation, "response.meta.generation", true),
    },
  };
}

function opportunitiesUrl(query: FundingOpportunityQuery) {
  const baseUrl = (process.env.NEXT_PUBLIC_API_BASE_URL ?? "").replace(/\/+$/, "");
  const search = new URLSearchParams({
    period: query.period,
    minLegNotionalUsd: String(query.minPositionNotional),
    minLegVolume24hUsd: String(query.minDailyVolume),
    limit: "200",
  });
  return `${baseUrl}/api/v1/funding-opportunities?${search}`;
}

export async function fetchFundingOpportunities(
  query: FundingOpportunityQuery,
  etag?: string | null,
  signal?: AbortSignal,
): Promise<FundingOpportunitiesFetchResult> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (etag) headers["If-None-Match"] = etag;
  const response = await fetch(opportunitiesUrl(query), {
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
  if (!response.ok) throw new Error(`机会排名 API 请求失败 (${response.status})`);
  return {
    status: "updated",
    snapshot: mapFundingOpportunitiesResponse(await response.json()),
    etag: response.headers.get("ETag"),
  };
}
