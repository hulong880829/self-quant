import { createIdempotencyKey } from "../idempotency-key";

export type ArbitrageView = "running" | "closed";
export type ArbitrageStatus = "running" | "closing" | "closed" | "failed";
export type ArbitragePreferredLeg = "a" | "b";
export type ArbitrageExecutionMode = "maker_then_hedge" | "simultaneous_market";
export type ArbitrageDirection = "ask" | "bid";

export interface ArbitrageLeg {
  tradingAccountId: number;
  accountName: string;
  exchange: string;
  instrumentId: number;
  exchangeSymbol: string;
  baseAsset: string;
  quoteAsset: string;
}

export interface ArbitrageCombination {
  id: string;
  productName: string;
  status: ArbitrageStatus;
  legA: ArbitrageLeg;
  legB: ArbitrageLeg;
  askThresholdBps: string;
  bidThresholdBps: string;
  targetNotional: string;
  positionNotional: string;
  cumulativeTurnoverNotional: string;
  consecutiveFailures: number;
  nextRetryAt: string;
  positionUncertain: boolean;
  orderNotional: string;
  maxDeltaNotional: string;
  preferredLeg: ArbitragePreferredLeg;
  executionMode: ArbitrageExecutionMode;
  askSpreadBps: string | null;
  bidSpreadBps: string | null;
  marketDataStale: boolean;
  errorMessage: string;
  createdAt: string;
  updatedAt: string;
  closedAt: string;
}

export interface ArbitrageExecution {
  id: string;
  direction: ArbitrageDirection;
  status: string;
  triggerSpreadBps: string;
  targetBaseQuantity: string;
  filledBaseQuantity: string;
  deltaNotional: string;
  errorMessage: string;
  createdAt: string;
  updatedAt: string;
}

export interface ArbitrageEvent {
  id: string;
  type: string;
  message: string;
  createdAt: string;
}

export interface ArbitrageCombinationDetail extends ArbitrageCombination {
  recentExecutions: ArbitrageExecution[];
  recentEvents: ArbitrageEvent[];
}

export interface ArbitrageCombinationPage {
  items: ArbitrageCombination[];
  nextCursor: string;
  total: number;
}

export interface CreateArbitrageCombinationInput {
  productName: string;
  legAAccountId: number;
  legAInstrumentId: number;
  legBAccountId: number;
  legBInstrumentId: number;
  askThresholdBps: string;
  bidThresholdBps: string;
  targetNotional: string;
  orderNotional: string;
  maxDeltaNotional: string;
  preferredLeg: ArbitragePreferredLeg;
  executionMode: ArbitrageExecutionMode;
}

function record(value: unknown, path: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`${path} 必须是对象`);
  }
  return value as Record<string, unknown>;
}

function text(value: unknown, path: string, allowEmpty = false): string {
  if (typeof value !== "string" || (!allowEmpty && value.length === 0)) {
    throw new Error(`${path} 必须是${allowEmpty ? "" : "非空"}字符串`);
  }
  return value;
}

function integer(value: unknown, path: string): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value <= 0) {
    throw new Error(`${path} 必须是正安全整数`);
  }
  return value;
}

function nonNegativeInteger(value: unknown, path: string): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0) {
    throw new Error(`${path} 必须是非负整数`);
  }
  return value;
}

function decimalString(value: unknown, path: string): string {
  if (typeof value !== "string" || value.trim() === "" || !Number.isFinite(Number(value))) {
    throw new Error(`${path} 必须是 decimal string`);
  }
  return value;
}

function nullableDecimalString(value: unknown, path: string): string | null {
  return value === null ? null : decimalString(value, path);
}

function boolean(value: unknown, path: string): boolean {
  if (typeof value !== "boolean") throw new Error(`${path} 必须是 boolean`);
  return value;
}

function oneOf<T extends string>(
  value: unknown,
  allowed: readonly T[],
  path: string,
): T {
  if (typeof value !== "string" || !allowed.includes(value as T)) {
    throw new Error(`${path} 不是支持的值`);
  }
  return value as T;
}

function array(value: unknown, path: string): unknown[] {
  if (!Array.isArray(value)) throw new Error(`${path} 必须是数组`);
  return value;
}

function mapLeg(value: unknown, path: string): ArbitrageLeg {
  const item = record(value, path);
  return {
    tradingAccountId: integer(item.tradingAccountId, `${path}.tradingAccountId`),
    accountName: text(item.accountName, `${path}.accountName`),
    exchange: text(item.exchange, `${path}.exchange`),
    instrumentId: integer(item.instrumentId, `${path}.instrumentId`),
    exchangeSymbol: text(item.exchangeSymbol, `${path}.exchangeSymbol`),
    baseAsset: text(item.baseAsset, `${path}.baseAsset`),
    quoteAsset: text(item.quoteAsset, `${path}.quoteAsset`),
  };
}

export function mapArbitrageCombination(
  value: unknown,
  path = "response.data",
): ArbitrageCombination {
  const item = record(value, path);
  return {
    id: text(item.id, `${path}.id`),
    productName: text(item.productName, `${path}.productName`),
    status: oneOf(item.status, ["running", "closing", "closed", "failed"], `${path}.status`),
    legA: mapLeg(item.legA, `${path}.legA`),
    legB: mapLeg(item.legB, `${path}.legB`),
    askThresholdBps: decimalString(item.askThresholdBps, `${path}.askThresholdBps`),
    bidThresholdBps: decimalString(item.bidThresholdBps, `${path}.bidThresholdBps`),
    targetNotional: decimalString(item.targetNotional, `${path}.targetNotional`),
    positionNotional: decimalString(item.positionNotional, `${path}.positionNotional`),
    cumulativeTurnoverNotional: decimalString(
      item.cumulativeTurnoverNotional,
      `${path}.cumulativeTurnoverNotional`,
    ),
    consecutiveFailures: nonNegativeInteger(
      item.consecutiveFailures,
      `${path}.consecutiveFailures`,
    ),
    nextRetryAt: text(item.nextRetryAt, `${path}.nextRetryAt`, true),
    positionUncertain: boolean(item.positionUncertain, `${path}.positionUncertain`),
    orderNotional: decimalString(item.orderNotional, `${path}.orderNotional`),
    maxDeltaNotional: decimalString(item.maxDeltaNotional, `${path}.maxDeltaNotional`),
    preferredLeg: oneOf(item.preferredLeg, ["a", "b"], `${path}.preferredLeg`),
    executionMode: oneOf(
      item.executionMode,
      ["maker_then_hedge", "simultaneous_market"],
      `${path}.executionMode`,
    ),
    askSpreadBps: nullableDecimalString(item.askSpreadBps, `${path}.askSpreadBps`),
    bidSpreadBps: nullableDecimalString(item.bidSpreadBps, `${path}.bidSpreadBps`),
    marketDataStale: boolean(item.marketDataStale, `${path}.marketDataStale`),
    errorMessage: text(item.errorMessage, `${path}.errorMessage`, true),
    createdAt: text(item.createdAt, `${path}.createdAt`),
    updatedAt: text(item.updatedAt, `${path}.updatedAt`),
    closedAt: text(item.closedAt, `${path}.closedAt`, true),
  };
}

function mapExecution(value: unknown, path: string): ArbitrageExecution {
  const item = record(value, path);
  return {
    id: text(item.id, `${path}.id`),
    direction: oneOf(item.direction, ["ask", "bid"], `${path}.direction`),
    status: text(item.status, `${path}.status`),
    triggerSpreadBps: decimalString(item.triggerSpreadBps, `${path}.triggerSpreadBps`),
    targetBaseQuantity: decimalString(item.targetBaseQuantity, `${path}.targetBaseQuantity`),
    filledBaseQuantity: decimalString(item.filledBaseQuantity, `${path}.filledBaseQuantity`),
    deltaNotional: decimalString(item.deltaNotional, `${path}.deltaNotional`),
    errorMessage: text(item.errorMessage, `${path}.errorMessage`, true),
    createdAt: text(item.createdAt, `${path}.createdAt`),
    updatedAt: text(item.updatedAt, `${path}.updatedAt`),
  };
}

function mapEvent(value: unknown, path: string): ArbitrageEvent {
  const item = record(value, path);
  return {
    id: text(item.id, `${path}.id`),
    type: text(item.type, `${path}.type`),
    message: text(item.message, `${path}.message`, true),
    createdAt: text(item.createdAt, `${path}.createdAt`),
  };
}

export function mapArbitrageCombinationDetail(value: unknown): ArbitrageCombinationDetail {
  const item = record(value, "response.data");
  return {
    ...mapArbitrageCombination(item),
    recentExecutions: array(item.recentExecutions, "response.data.recentExecutions").map(
      (entry, index) => mapExecution(entry, `response.data.recentExecutions[${index}]`),
    ),
    recentEvents: array(item.recentEvents, "response.data.recentEvents").map(
      (entry, index) => mapEvent(entry, `response.data.recentEvents[${index}]`),
    ),
  };
}

function apiUrl(path = ""): string {
  const base = (process.env.NEXT_PUBLIC_API_BASE_URL ?? "").replace(/\/+$/, "");
  return `${base}/api/v1/trader/arbitrage-combinations${path}`;
}

async function responseError(response: Response, fallback: string): Promise<Error> {
  const body = (await response.json().catch(() => ({}))) as { error?: unknown };
  return new Error(typeof body.error === "string" && body.error ? body.error : fallback);
}

async function data(response: Response, fallback: string): Promise<unknown> {
  if (response.status === 401) throw new Error("请先登录");
  if (!response.ok) throw await responseError(response, fallback);
  const payload = record(await response.json(), "response");
  if (!("data" in payload)) throw new Error(`${fallback}：响应缺少 data`);
  return payload.data;
}

export async function createArbitrageCombination(
  input: CreateArbitrageCombinationInput,
  idempotencyKey = createIdempotencyKey(),
): Promise<ArbitrageCombination> {
  const response = await fetch(apiUrl(), {
    method: "POST",
    credentials: "include",
    headers: {
      Accept: "application/json",
      "Content-Type": "application/json",
      "Idempotency-Key": idempotencyKey,
    },
    body: JSON.stringify(input),
  });
  return mapArbitrageCombination(await data(response, "套利组合创建失败"));
}

export async function fetchArbitrageCombinations(
  view: ArbitrageView,
  options: { cursor?: string; limit?: number; signal?: AbortSignal } = {},
): Promise<ArbitrageCombinationPage> {
  const params = new URLSearchParams({
    view,
    limit: String(options.limit ?? 50),
  });
  if (options.cursor) params.set("cursor", options.cursor);
  const response = await fetch(`${apiUrl()}?${params.toString()}`, {
    credentials: "include",
    headers: { Accept: "application/json" },
    cache: "no-store",
    signal: options.signal,
  });
  if (response.status === 401) throw new Error("请先登录");
  if (!response.ok) throw await responseError(response, "无法加载套利组合");
  const payload = record(await response.json(), "response");
  const items = array(payload.data, "response.data").map((item, index) =>
    mapArbitrageCombination(item, `response.data[${index}]`),
  );
  const meta = record(payload.meta, "response.meta");
  const total = meta.total;
  if (typeof total !== "number" || !Number.isSafeInteger(total) || total < 0) {
    throw new Error("response.meta.total 必须是非负整数");
  }
  return {
    items,
    nextCursor: text(meta.nextCursor, "response.meta.nextCursor", true),
    total,
  };
}

export async function fetchArbitrageCombination(
  id: string,
  signal?: AbortSignal,
): Promise<ArbitrageCombinationDetail> {
  const response = await fetch(apiUrl(`/${encodeURIComponent(id)}`), {
    credentials: "include",
    headers: { Accept: "application/json" },
    cache: "no-store",
    signal,
  });
  return mapArbitrageCombinationDetail(await data(response, "无法加载套利组合详情"));
}

export async function closeArbitrageCombination(id: string): Promise<ArbitrageCombination> {
  const response = await fetch(apiUrl(`/${encodeURIComponent(id)}`), {
    method: "DELETE",
    credentials: "include",
    headers: { Accept: "application/json" },
  });
  return mapArbitrageCombination(await data(response, "关闭套利组合失败"));
}
