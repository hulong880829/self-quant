import { createIdempotencyKey } from "../idempotency-key";

export type TraderContractType = "spot" | "perpetual";
export type TraderSide = "buy" | "sell";
export type TraderOrderType = "limit" | "market";
export type TraderTwapOrderType = "maker" | "market";
export type TraderTwapView = "running" | "closed";

export interface TraderInstrument {
  id: number;
  exchange: string;
  contractType: TraderContractType;
  exchangeSymbol: string;
  baseAsset: string;
  quoteAsset: string;
  settleAsset: string;
  contractSize: string;
  priceTick: string;
  quantityStep: string;
}

export interface TraderOrder {
  id: string;
  idempotencyKey: string;
  tradingAccountId: number;
  productName: string;
  exchange: string;
  instrumentId: number;
  contractType: TraderContractType;
  exchangeSymbol: string;
  baseAsset: string;
  quoteAsset: string;
  clientOrderId: string;
  venueOrderId: string;
  side: TraderSide;
  orderType: TraderOrderType;
  quantity: string;
  price: string;
  filledQuantity: string;
  averagePrice: string;
  status: string;
  errorCode: string;
  errorMessage: string;
  createdAt: string;
  updatedAt: string;
  lastReconciledAt: string;
  syncState: string;
  twapSliceIndex: number;
  twapAttemptIndex: number;
}

export interface TraderOrderPage {
  items: TraderOrder[];
  nextCursor: string;
}

export interface TraderTwap {
  id: string;
  idempotencyKey: string;
  tradingAccountId: number;
  productName: string;
  exchange: string;
  instrumentId: number;
  contractType: TraderContractType;
  exchangeSymbol: string;
  baseAsset: string;
  quoteAsset: string;
  side: TraderSide;
  totalQty: string;
  filledQty: string;
  averagePrice: string;
  startAt: string;
  endAt: string;
  intervalSeconds: number;
  limitPrice: string;
  maxQty: string;
  orderType: TraderTwapOrderType;
  orderTimeoutSeconds: number;
  status: string;
  currentSlice: number;
  currentAttempt: number;
  childOrderCount: number;
  createdAt: string;
  updatedAt: string;
  nextRunAt: string;
  lastRunAt: string;
  canceledAt: string;
  errorMessage: string;
}

export interface TraderTwapPage {
  items: TraderTwap[];
  nextCursor: string;
}

export interface CreateTraderTwapInput {
  tradingAccountId: number;
  instrumentId: number;
  side: TraderSide;
  totalQty: string;
  startAt: string;
  endAt: string;
  intervalSeconds: number;
  limitPrice?: string;
  maxQty?: string;
  orderType: TraderTwapOrderType;
  orderTimeoutSeconds?: number;
}

function apiBaseUrl(): string {
  return (process.env.NEXT_PUBLIC_API_BASE_URL ?? "").replace(/\/+$/, "");
}

function traderUrl(path: string): string {
  return `${apiBaseUrl()}/api/v1/trader${path}`;
}

async function errorMessage(response: Response, fallback: string): Promise<string> {
  const body = (await response.json().catch(() => ({}))) as { error?: string };
  return body.error || fallback;
}

function mapInstrument(value: Record<string, unknown>): TraderInstrument {
  return {
    id: Number(value.id),
    exchange: String(value.exchange ?? ""),
    contractType: String(value.contractType ?? "perpetual") as TraderContractType,
    exchangeSymbol: String(value.exchangeSymbol ?? ""),
    baseAsset: String(value.baseAsset ?? ""),
    quoteAsset: String(value.quoteAsset ?? ""),
    settleAsset: String(value.settleAsset ?? ""),
    contractSize: String(value.contractSize ?? ""),
    priceTick: String(value.priceTick ?? ""),
    quantityStep: String(value.quantityStep ?? ""),
  };
}

function mapOrder(value: Record<string, unknown>): TraderOrder {
  return {
    id: String(value.id ?? ""),
    idempotencyKey: String(value.idempotencyKey ?? ""),
    tradingAccountId: Number(value.tradingAccountId),
    productName: String(value.productName ?? ""),
    exchange: String(value.exchange ?? ""),
    instrumentId: Number(value.instrumentId),
    contractType: String(value.contractType ?? "") as TraderContractType,
    exchangeSymbol: String(value.exchangeSymbol ?? ""),
    baseAsset: String(value.baseAsset ?? ""),
    quoteAsset: String(value.quoteAsset ?? ""),
    clientOrderId: String(value.clientOrderId ?? ""),
    venueOrderId: String(value.venueOrderId ?? ""),
    side: String(value.side ?? "") as TraderSide,
    orderType: String(value.orderType ?? "") as TraderOrderType,
    quantity: String(value.quantity ?? ""),
    price: String(value.price ?? ""),
    filledQuantity: String(value.filledQuantity ?? ""),
    averagePrice: String(value.averagePrice ?? ""),
    status: String(value.status ?? ""),
    errorCode: String(value.errorCode ?? ""),
    errorMessage: String(value.errorMessage ?? ""),
    createdAt: String(value.createdAt ?? ""),
    updatedAt: String(value.updatedAt ?? ""),
    lastReconciledAt: String(value.lastReconciledAt ?? ""),
    syncState: String(value.syncState ?? ""),
    twapSliceIndex: Number(value.twapSliceIndex ?? 0),
    twapAttemptIndex: Number(value.twapAttemptIndex ?? 0),
  };
}

function mapTwap(value: Record<string, unknown>): TraderTwap {
  return {
    id: String(value.id ?? ""),
    idempotencyKey: String(value.idempotencyKey ?? ""),
    tradingAccountId: Number(value.tradingAccountId),
    productName: String(value.productName ?? ""),
    exchange: String(value.exchange ?? ""),
    instrumentId: Number(value.instrumentId),
    contractType: String(value.contractType ?? "") as TraderContractType,
    exchangeSymbol: String(value.exchangeSymbol ?? ""),
    baseAsset: String(value.baseAsset ?? ""),
    quoteAsset: String(value.quoteAsset ?? ""),
    side: String(value.side ?? "") as TraderSide,
    totalQty: String(value.totalQuantity ?? value.totalQty ?? ""),
    filledQty: String(value.filledQuantity ?? value.filledQty ?? value.executedQty ?? ""),
    averagePrice: String(value.averagePrice ?? ""),
    startAt: String(value.startAt ?? ""),
    endAt: String(value.endAt ?? ""),
    intervalSeconds: Number(value.intervalSeconds ?? 0),
    limitPrice: String(value.limitPrice ?? ""),
    maxQty: String(value.maxQuantity ?? value.maxQty ?? ""),
    orderType: String(value.executionType ?? value.orderType ?? "") as TraderTwapOrderType,
    orderTimeoutSeconds: Number(value.orderTimeoutSeconds ?? 0),
    status: String(value.status ?? ""),
    currentSlice: Number(value.currentSlice ?? 0),
    currentAttempt: Number(value.currentAttempt ?? 0),
    childOrderCount: Number(value.childOrderCount ?? 0),
    createdAt: String(value.createdAt ?? ""),
    updatedAt: String(value.updatedAt ?? ""),
    nextRunAt: String(value.nextActionAt ?? value.nextRunAt ?? ""),
    lastRunAt: String(value.lastRunAt ?? ""),
    canceledAt: String(value.closedAt ?? value.canceledAt ?? ""),
    errorMessage: String(value.errorMessage ?? ""),
  };
}

export async function fetchTraderInstruments(
  accountId: number,
  contractType: TraderContractType,
): Promise<TraderInstrument[]> {
  const response = await fetch(
    traderUrl(`/accounts/${accountId}/instruments?type=${encodeURIComponent(contractType)}`),
    { credentials: "include", headers: { Accept: "application/json" }, cache: "no-store" },
  );
  if (response.status === 401) throw new Error("请先登录");
  if (!response.ok) throw new Error(await errorMessage(response, "无法加载交易标的"));
  const payload = (await response.json()) as { data?: Record<string, unknown>[] };
  return (payload.data ?? []).map(mapInstrument);
}

export async function fetchTraderOrder(orderId: string): Promise<TraderOrder> {
  const response = await fetch(traderUrl(`/orders/${encodeURIComponent(orderId)}`), {
    credentials: "include",
    headers: { Accept: "application/json" },
    cache: "no-store",
  });
  if (response.status === 401) throw new Error("请先登录");
  if (!response.ok) throw new Error(await errorMessage(response, "无法加载订单"));
  const payload = (await response.json()) as { data?: Record<string, unknown> };
  if (!payload.data) throw new Error("订单响应无效");
  return mapOrder(payload.data);
}

export async function fetchTraderOrders(accountId: number): Promise<TraderOrder[]> {
  return (await fetchTraderOrderPage(accountId, { view: "open" })).items;
}

export async function fetchTraderOrderPage(
  accountId: number,
  options: {
    view: "open" | "history";
    limit?: number;
    cursor?: string;
    signal?: AbortSignal;
  },
): Promise<TraderOrderPage> {
  const params = new URLSearchParams({
    accountId: String(accountId),
    limit: String(options.limit ?? 50),
    view: options.view,
  });
  if (options.cursor) params.set("cursor", options.cursor);
  const response = await fetch(
    traderUrl(`/orders?${params.toString()}`),
    {
      credentials: "include",
      headers: { Accept: "application/json" },
      cache: "no-store",
      signal: options.signal,
    },
  );
  if (response.status === 401) throw new Error("请先登录");
  if (!response.ok) throw new Error(await errorMessage(response, "无法加载订单"));
  const payload = (await response.json()) as {
    data?: Record<string, unknown>[];
    meta?: { nextCursor?: string };
  };
  return {
    items: (payload.data ?? []).map(mapOrder),
    nextCursor: payload.meta?.nextCursor ?? "",
  };
}

export async function placeTraderOrder(input: {
  tradingAccountId: number;
  instrumentId: number;
  side: TraderSide;
  orderType: TraderOrderType;
  quantity: string;
  price?: string;
}): Promise<TraderOrder> {
  const idempotencyKey = createIdempotencyKey();
  const response = await fetch(traderUrl("/orders"), {
    method: "POST",
    credentials: "include",
    headers: {
      Accept: "application/json",
      "Content-Type": "application/json",
      "Idempotency-Key": idempotencyKey,
    },
    body: JSON.stringify({ ...input, price: input.price ?? "", idempotencyKey }),
  });
  if (!response.ok) throw new Error(await errorMessage(response, "订单提交失败"));
  const payload = (await response.json()) as { data?: Record<string, unknown> };
  if (!payload.data) throw new Error("下单响应无效");
  return mapOrder(payload.data);
}

export async function cancelTraderOrder(orderId: string): Promise<TraderOrder> {
  const response = await fetch(traderUrl(`/orders/${encodeURIComponent(orderId)}`), {
    method: "DELETE",
    credentials: "include",
    headers: { Accept: "application/json" },
  });
  if (!response.ok) throw new Error(await errorMessage(response, "撤单失败"));
  const payload = (await response.json()) as { data?: Record<string, unknown> };
  if (!payload.data) throw new Error("撤单响应无效");
  return mapOrder(payload.data);
}

export async function createTraderTwap(input: CreateTraderTwapInput): Promise<TraderTwap> {
  const idempotencyKey = createIdempotencyKey();
  const response = await fetch(traderUrl("/twaps"), {
    method: "POST",
    credentials: "include",
    headers: {
      Accept: "application/json",
      "Content-Type": "application/json",
      "Idempotency-Key": idempotencyKey,
    },
    body: JSON.stringify({
      tradingAccountId: input.tradingAccountId,
      instrumentId: input.instrumentId,
      side: input.side,
      totalQuantity: input.totalQty,
      startAt: input.startAt,
      endAt: input.endAt,
      intervalSeconds: input.intervalSeconds,
      limitPrice: input.limitPrice ?? "",
      maxQuantity: input.maxQty ?? "",
      executionType: input.orderType,
      orderTimeoutSeconds: input.orderType === "maker" ? input.orderTimeoutSeconds ?? 0 : 0,
    }),
  });
  if (!response.ok) throw new Error(await errorMessage(response, "TWAP 计划创建失败"));
  const payload = (await response.json()) as { data?: Record<string, unknown> };
  if (!payload.data) throw new Error("TWAP 创建响应无效");
  return mapTwap(payload.data);
}

export async function fetchTraderTwapPage(
  accountId: number | undefined,
  options: {
    view: TraderTwapView;
    limit?: number;
    cursor?: string;
    signal?: AbortSignal;
  },
): Promise<TraderTwapPage> {
  const params = new URLSearchParams({
    view: options.view,
    limit: String(options.limit ?? 50),
  });
  if (accountId != null) params.set("accountId", String(accountId));
  if (options.cursor) params.set("cursor", options.cursor);
  const response = await fetch(traderUrl(`/twaps?${params.toString()}`), {
    credentials: "include",
    headers: { Accept: "application/json" },
    cache: "no-store",
    signal: options.signal,
  });
  if (response.status === 401) throw new Error("请先登录");
  if (!response.ok) throw new Error(await errorMessage(response, "无法加载 TWAP 计划"));
  const payload = (await response.json()) as {
    data?: Record<string, unknown>[];
    meta?: { nextCursor?: string };
  };
  return {
    items: (payload.data ?? []).map(mapTwap),
    nextCursor: payload.meta?.nextCursor ?? "",
  };
}

export async function fetchTraderTwap(twapId: string): Promise<TraderTwap> {
  const response = await fetch(traderUrl(`/twaps/${encodeURIComponent(twapId)}`), {
    credentials: "include",
    headers: { Accept: "application/json" },
    cache: "no-store",
  });
  if (response.status === 401) throw new Error("请先登录");
  if (!response.ok) throw new Error(await errorMessage(response, "无法加载 TWAP 详情"));
  const payload = (await response.json()) as { data?: Record<string, unknown> };
  if (!payload.data) throw new Error("TWAP 详情响应无效");
  return mapTwap(payload.data);
}

export async function fetchTraderTwapOrders(twapId: string): Promise<TraderOrder[]> {
  const response = await fetch(traderUrl(`/twaps/${encodeURIComponent(twapId)}/orders`), {
    credentials: "include",
    headers: { Accept: "application/json" },
    cache: "no-store",
  });
  if (response.status === 401) throw new Error("请先登录");
  if (!response.ok) throw new Error(await errorMessage(response, "无法加载 TWAP 子订单"));
  const payload = (await response.json()) as { data?: Record<string, unknown>[] };
  return (payload.data ?? []).map(mapOrder);
}

export async function cancelTraderTwap(twapId: string): Promise<TraderTwap> {
  const response = await fetch(traderUrl(`/twaps/${encodeURIComponent(twapId)}`), {
    method: "DELETE",
    credentials: "include",
    headers: { Accept: "application/json" },
  });
  if (!response.ok) throw new Error(await errorMessage(response, "取消 TWAP 计划失败"));
  const payload = (await response.json()) as { data?: Record<string, unknown> };
  if (!payload.data) throw new Error("取消 TWAP 响应无效");
  return mapTwap(payload.data);
}

export const CEX_EXCHANGES = new Set(["binance", "okx", "bybit", "bitget", "gate"]);
