import { createIdempotencyKey } from "../idempotency-key";
import { mapTraderOrder, type TraderOrder } from "./trader";

export type ArbitrageView = "running" | "closed";
export type ArbitrageStatus = "running" | "closing" | "closed" | "failed";
export type ArbitragePreferredLeg = "a" | "b";
export type ArbitrageExecutionMode = "maker_then_hedge" | "simultaneous_market";
export type ArbitrageDirection = "ask" | "bid";
export type ArbitrageRuntimeState =
  | "monitoring"
  | "maker_open"
  | "maker_canceling"
  | "repricing"
  | "opportunity_gone"
  | "hedging"
  | "hedge_deferred_dust"
  | "reconciling"
  | "backoff"
  | "position_uncertain"
  | "manual_intervention"
  | "closing";

export interface ArbitrageLeg {
  tradingAccountId: number;
  accountName: string;
  exchange: string;
  contractType: "spot" | "perpetual";
  instrumentId: number;
  exchangeSymbol: string;
  baseAsset: string;
  quoteAsset: string;
}

export type ArbitrageRunMode = "spread" | "one_shot";
export type ArbitrageExitPolicy = "annualized" | "time" | "";
export type ArbitrageOneShotPhase =
  | "building_target"
  | "waiting_exit"
  | "exiting"
  | "exited"
  | "";

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
  grossTurnoverNotional: string;
  consecutiveFailures: number;
  nextRetryAt: string;
  positionUncertain: boolean;
  runtimeState: ArbitrageRuntimeState;
  runMode: ArbitrageRunMode;
  entryDirection: "" | ArbitrageDirection;
  legALeverage: string;
  legBLeverage: string;
  exitPolicy: ArbitrageExitPolicy;
  exitAnnualizedRate: string;
  exitAfterSeconds: number;
  targetReachedAt: string;
  scheduledExitAt: string;
  oneShotPhase: ArbitrageOneShotPhase;
  earlyExitFunding8hAnnualizedFloor: string;
  legABasePosition: string;
  legBBasePosition: string;
  carryBaseQuantity: string;
  legAAverageEntryPrice: string | null;
  legBAverageEntryPrice: string | null;
  averageEntrySpreadBps: string | null;
  legAUnrealizedPnl: string | null;
  legBUnrealizedPnl: string | null;
  realizedSpreadPnl: string;
  estimatedFundingPnl: string;
  combinedPositionAnnualized: string | null;
  fundingHistoryComplete: boolean;
  legAVenueBaselineBasePosition: string | null;
  legBVenueBaselineBasePosition: string | null;
  venueBaselineCapturedAt: string;
  legAExpectedBasePosition: string | null;
  legBExpectedBasePosition: string | null;
  legAVenueBasePosition: string;
  legBVenueBasePosition: string;
  legAVenueNotional: string | null;
  legBVenueNotional: string | null;
  legAVenueValuationPrice: string | null;
  legBVenueValuationPrice: string | null;
  legAVenueValuationAt: string;
  legBVenueValuationAt: string;
  legAPositionDifference: string;
  legBPositionDifference: string;
  lastPositionReconciledAt: string;
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
  orders: TraderOrder[];
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
  preferredLeg: ArbitragePreferredLeg;
  executionMode: ArbitrageExecutionMode;
  runMode: ArbitrageRunMode;
  entryDirection: "" | ArbitrageDirection;
  legALeverage: string;
  legBLeverage: string;
  exitPolicy: ArbitrageExitPolicy;
  exitAnnualizedRate: string;
  exitAfterSeconds: number;
  earlyExitFunding8hAnnualizedFloor: string;
}

export type UpdateArbitrageCombinationInput =
  | { targetNotional: string }
  | { askThresholdBps: string }
  | { bidThresholdBps: string };

export class ArbitrageCreateFailure extends Error {
  code: string;
  leg: string;
  details: Record<string, string>;

  constructor(
    message: string,
    code: string,
    leg: string,
    details: Record<string, string> = {},
  ) {
    super(message);
    this.name = "ArbitrageCreateFailure";
    this.code = code;
    this.leg = leg;
    this.details = details;
  }
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
  if (
    typeof value !== "string" ||
    value.trim() === "" ||
    !Number.isFinite(Number(value))
  ) {
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
    tradingAccountId: integer(
      item.tradingAccountId,
      `${path}.tradingAccountId`,
    ),
    accountName: text(item.accountName, `${path}.accountName`),
    exchange: text(item.exchange, `${path}.exchange`),
    contractType: oneOf(
      item.contractType,
      ["spot", "perpetual"],
      `${path}.contractType`,
    ),
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
    status: oneOf(
      item.status,
      ["running", "closing", "closed", "failed"],
      `${path}.status`,
    ),
    legA: mapLeg(item.legA, `${path}.legA`),
    legB: mapLeg(item.legB, `${path}.legB`),
    askThresholdBps: decimalString(
      item.askThresholdBps,
      `${path}.askThresholdBps`,
    ),
    bidThresholdBps: decimalString(
      item.bidThresholdBps,
      `${path}.bidThresholdBps`,
    ),
    targetNotional: decimalString(
      item.targetNotional,
      `${path}.targetNotional`,
    ),
    positionNotional: decimalString(
      item.positionNotional,
      `${path}.positionNotional`,
    ),
    cumulativeTurnoverNotional: decimalString(
      item.cumulativeTurnoverNotional,
      `${path}.cumulativeTurnoverNotional`,
    ),
    grossTurnoverNotional: decimalString(
      item.grossTurnoverNotional ?? "0",
      `${path}.grossTurnoverNotional`,
    ),
    consecutiveFailures: nonNegativeInteger(
      item.consecutiveFailures,
      `${path}.consecutiveFailures`,
    ),
    nextRetryAt: text(item.nextRetryAt, `${path}.nextRetryAt`, true),
    positionUncertain: boolean(
      item.positionUncertain,
      `${path}.positionUncertain`,
    ),
    runtimeState: oneOf(
      item.runtimeState,
      [
        "monitoring",
        "maker_open",
        "maker_canceling",
        "repricing",
        "opportunity_gone",
        "hedging",
        "hedge_deferred_dust",
        "reconciling",
        "backoff",
        "position_uncertain",
        "manual_intervention",
        "closing",
      ],
      `${path}.runtimeState`,
    ),
    legABasePosition: decimalString(
      item.legABasePosition,
      `${path}.legABasePosition`,
    ),
    legBBasePosition: decimalString(
      item.legBBasePosition,
      `${path}.legBBasePosition`,
    ),
    carryBaseQuantity: decimalString(
      item.carryBaseQuantity,
      `${path}.carryBaseQuantity`,
    ),
    legAAverageEntryPrice: nullableDecimalString(
      item.legAAverageEntryPrice ?? null,
      `${path}.legAAverageEntryPrice`,
    ),
    legBAverageEntryPrice: nullableDecimalString(
      item.legBAverageEntryPrice ?? null,
      `${path}.legBAverageEntryPrice`,
    ),
    averageEntrySpreadBps: nullableDecimalString(
      item.averageEntrySpreadBps ?? null,
      `${path}.averageEntrySpreadBps`,
    ),
    legAUnrealizedPnl: nullableDecimalString(
      item.legAUnrealizedPnl ?? null,
      `${path}.legAUnrealizedPnl`,
    ),
    legBUnrealizedPnl: nullableDecimalString(
      item.legBUnrealizedPnl ?? null,
      `${path}.legBUnrealizedPnl`,
    ),
    realizedSpreadPnl: decimalString(
      item.realizedSpreadPnl ?? "0",
      `${path}.realizedSpreadPnl`,
    ),
    estimatedFundingPnl: decimalString(
      item.estimatedFundingPnl ?? "0",
      `${path}.estimatedFundingPnl`,
    ),
    combinedPositionAnnualized: nullableDecimalString(
      item.combinedPositionAnnualized ?? null,
      `${path}.combinedPositionAnnualized`,
    ),
    fundingHistoryComplete: boolean(
      item.fundingHistoryComplete ?? true,
      `${path}.fundingHistoryComplete`,
    ),
    legAVenueBaselineBasePosition: nullableDecimalString(
      item.legAVenueBaselineBasePosition ?? null,
      `${path}.legAVenueBaselineBasePosition`,
    ),
    legBVenueBaselineBasePosition: nullableDecimalString(
      item.legBVenueBaselineBasePosition ?? null,
      `${path}.legBVenueBaselineBasePosition`,
    ),
    venueBaselineCapturedAt: text(
      item.venueBaselineCapturedAt ?? "",
      `${path}.venueBaselineCapturedAt`,
      true,
    ),
    legAExpectedBasePosition: nullableDecimalString(
      item.legAExpectedBasePosition ?? null,
      `${path}.legAExpectedBasePosition`,
    ),
    legBExpectedBasePosition: nullableDecimalString(
      item.legBExpectedBasePosition ?? null,
      `${path}.legBExpectedBasePosition`,
    ),
    legAVenueBasePosition: decimalString(
      item.legAVenueBasePosition ?? "0",
      `${path}.legAVenueBasePosition`,
    ),
    legBVenueBasePosition: decimalString(
      item.legBVenueBasePosition ?? "0",
      `${path}.legBVenueBasePosition`,
    ),
    legAVenueNotional: nullableDecimalString(
      item.legAVenueNotional ?? null,
      `${path}.legAVenueNotional`,
    ),
    legBVenueNotional: nullableDecimalString(
      item.legBVenueNotional ?? null,
      `${path}.legBVenueNotional`,
    ),
    legAVenueValuationPrice: nullableDecimalString(
      item.legAVenueValuationPrice ?? null,
      `${path}.legAVenueValuationPrice`,
    ),
    legBVenueValuationPrice: nullableDecimalString(
      item.legBVenueValuationPrice ?? null,
      `${path}.legBVenueValuationPrice`,
    ),
    legAVenueValuationAt: text(
      item.legAVenueValuationAt ?? "",
      `${path}.legAVenueValuationAt`,
      true,
    ),
    legBVenueValuationAt: text(
      item.legBVenueValuationAt ?? "",
      `${path}.legBVenueValuationAt`,
      true,
    ),
    legAPositionDifference: decimalString(
      item.legAPositionDifference ?? "0",
      `${path}.legAPositionDifference`,
    ),
    legBPositionDifference: decimalString(
      item.legBPositionDifference ?? "0",
      `${path}.legBPositionDifference`,
    ),
    lastPositionReconciledAt: text(
      item.lastPositionReconciledAt ?? "",
      `${path}.lastPositionReconciledAt`,
      true,
    ),
    runMode: oneOf(
      item.runMode || "spread",
      ["spread", "one_shot"],
      `${path}.runMode`,
    ),
    entryDirection:
      item.entryDirection === "ask" || item.entryDirection === "bid"
        ? item.entryDirection
        : "",
    legALeverage: text(item.legALeverage ?? "", `${path}.legALeverage`, true),
    legBLeverage: text(item.legBLeverage ?? "", `${path}.legBLeverage`, true),
    exitPolicy:
      item.exitPolicy === "annualized" || item.exitPolicy === "time"
        ? item.exitPolicy
        : "",
    exitAnnualizedRate: text(
      item.exitAnnualizedRate ?? "",
      `${path}.exitAnnualizedRate`,
      true,
    ),
    exitAfterSeconds:
      typeof item.exitAfterSeconds === "number" &&
      Number.isSafeInteger(item.exitAfterSeconds)
        ? item.exitAfterSeconds
        : 0,
    targetReachedAt: text(
      item.targetReachedAt ?? "",
      `${path}.targetReachedAt`,
      true,
    ),
    scheduledExitAt: text(
      item.scheduledExitAt ?? "",
      `${path}.scheduledExitAt`,
      true,
    ),
    oneShotPhase:
      item.oneShotPhase === "building_target" ||
      item.oneShotPhase === "waiting_exit" ||
      item.oneShotPhase === "exiting" ||
      item.oneShotPhase === "exited"
        ? item.oneShotPhase
        : "",
    earlyExitFunding8hAnnualizedFloor: text(
      item.earlyExitFunding8hAnnualizedFloor ?? "",
      `${path}.earlyExitFunding8hAnnualizedFloor`,
      true,
    ),
    preferredLeg: oneOf(item.preferredLeg, ["a", "b"], `${path}.preferredLeg`),
    executionMode: oneOf(
      item.executionMode,
      ["maker_then_hedge", "simultaneous_market"],
      `${path}.executionMode`,
    ),
    askSpreadBps: nullableDecimalString(
      item.askSpreadBps,
      `${path}.askSpreadBps`,
    ),
    bidSpreadBps: nullableDecimalString(
      item.bidSpreadBps,
      `${path}.bidSpreadBps`,
    ),
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
    triggerSpreadBps: decimalString(
      item.triggerSpreadBps,
      `${path}.triggerSpreadBps`,
    ),
    targetBaseQuantity: decimalString(
      item.targetBaseQuantity,
      `${path}.targetBaseQuantity`,
    ),
    filledBaseQuantity: decimalString(
      item.filledBaseQuantity,
      `${path}.filledBaseQuantity`,
    ),
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

export function mapArbitrageCombinationDetail(
  value: unknown,
): ArbitrageCombinationDetail {
  const item = record(value, "response.data");
  return {
    ...mapArbitrageCombination(item),
    orders: array(item.orders, "response.data.orders").map((entry, index) =>
      mapTraderOrder(record(entry, `response.data.orders[${index}]`)),
    ),
    recentExecutions: array(
      item.recentExecutions,
      "response.data.recentExecutions",
    ).map((entry, index) =>
      mapExecution(entry, `response.data.recentExecutions[${index}]`),
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

async function responseError(
  response: Response,
  fallback: string,
): Promise<Error> {
  const body = (await response.json().catch(() => ({}))) as {
    error?: unknown;
    code?: unknown;
    leg?: unknown;
    details?: unknown;
  };
  const message =
    typeof body.error === "string" && body.error ? body.error : fallback;
  if (typeof body.code === "string" && body.code) {
    const details =
      body.details && typeof body.details === "object" && !Array.isArray(body.details)
        ? (body.details as Record<string, string>)
        : {};
    return new ArbitrageCreateFailure(
      message,
      body.code,
      typeof body.leg === "string" ? body.leg : "",
      details,
    );
  }
  return new Error(message);
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
  return mapArbitrageCombinationDetail(
    await data(response, "无法加载套利组合详情"),
  );
}

export async function updateArbitrageCombination(
  id: string,
  input: UpdateArbitrageCombinationInput,
): Promise<ArbitrageCombination> {
  const response = await fetch(apiUrl(`/${encodeURIComponent(id)}`), {
    method: "PATCH",
    credentials: "include",
    headers: {
      Accept: "application/json",
      "Content-Type": "application/json",
    },
    body: JSON.stringify(input),
  });
  return mapArbitrageCombination(await data(response, "套利组合参数更新失败"));
}

export async function closeArbitrageCombination(
  id: string,
): Promise<ArbitrageCombination> {
  const response = await fetch(apiUrl(`/${encodeURIComponent(id)}`), {
    method: "DELETE",
    credentials: "include",
    headers: { Accept: "application/json" },
  });
  return mapArbitrageCombination(await data(response, "关闭套利组合失败"));
}

export function annualizedPercentToRatio(percent: string): string | null {
  const trimmed = percent.trim();
  if (!/^(?:\d+)(?:\.\d+)?$/.test(trimmed)) {
    return null;
  }
  const ratio = percentDigitsToRatio(trimmed);
  if (ratio === "0") {
    return null;
  }
  return ratio;
}

export function signedAnnualizedPercentToRatio(percent: string): string | null {
  const trimmed = percent.trim();
  const match = /^(-)?(\d+(?:\.\d+)?)$/.exec(trimmed);
  if (!match) {
    return null;
  }
  const ratio = percentDigitsToRatio(match[2]!);
  if (ratio === "0") {
    return "0";
  }
  return match[1] ? `-${ratio}` : ratio;
}

function percentDigitsToRatio(trimmed: string): string {
  const [wholeRaw, frac = ""] = trimmed.split(".");
  const whole = wholeRaw.replace(/^0+(?=\d)/, "") || "0";
  const digits = `${whole}${frac}`.replace(/^0+/, "") || "0";
  const scale = frac.length + 2;
  let integerPart: string;
  let fractionPart: string;
  if (digits.length <= scale) {
    integerPart = "0";
    fractionPart = digits.padStart(scale, "0");
  } else {
    integerPart = digits.slice(0, digits.length - scale);
    fractionPart = digits.slice(digits.length - scale);
  }
  fractionPart = fractionPart.replace(/0+$/, "");
  return fractionPart.length > 0 ? `${integerPart}.${fractionPart}` : integerPart;
}

export function annualizedRatioToPercent(ratio: string): string | null {
  const trimmed = ratio.trim();
  if (!/^(?:\d+)(?:\.\d+)?$/.test(trimmed)) {
    return null;
  }
  const [wholeRaw, frac = ""] = trimmed.split(".");
  const whole = wholeRaw.replace(/^0+(?=\d)/, "") || "0";
  const intDigits =
    `${whole}${frac.padEnd(2, "0").slice(0, 2)}`.replace(/^0+/, "") || "0";
  const rest = frac.length > 2 ? frac.slice(2).replace(/0+$/, "") : "";
  return rest ? `${intDigits}.${rest}` : intDigits;
}

export function formatAnnualizedRatioAsPercent(ratio: string): string {
  const percent = annualizedRatioToPercent(ratio);
  if (percent == null) {
    return ratio;
  }
  const [whole, fraction = ""] = percent.split(".");
  return `${whole}.${fraction.padEnd(2, "0").slice(0, 2)}%`;
}

export function formatSignedAnnualizedRatioAsPercent(ratio: string): string {
  const trimmed = ratio.trim();
  const negative = trimmed.startsWith("-");
  const abs = negative ? trimmed.slice(1) : trimmed;
  const formatted = formatAnnualizedRatioAsPercent(abs);
  if (formatted === abs) {
    return negative ? `-${formatted}` : formatted;
  }
  return negative ? `-${formatted}` : formatted;
}
