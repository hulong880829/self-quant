import type {
  PolymarketAccountSummary,
  PolymarketMarket,
  PolymarketMarketSnapshot,
  PolymarketOpenOrder,
  PolymarketOrder,
  PolymarketPosition,
  PolymarketPricePoint,
} from "@/types/polymarket";
import { createIdempotencyKey } from "../idempotency-key";

function apiBaseUrl(): string {
  return (process.env.NEXT_PUBLIC_API_BASE_URL ?? "").replace(/\/+$/, "");
}

function url(path: string): string {
  return `${apiBaseUrl()}/api/v1/polymarket${path}`;
}

function decimal(value: unknown): number | null {
  if (value === "" || value == null) return null;
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : null;
}

async function errorMessage(response: Response, fallback: string): Promise<string> {
  const body = (await response.json().catch(() => ({}))) as { error?: string };
  return body.error || fallback;
}

function market(value: unknown): PolymarketMarket {
  const item = value as Record<string, unknown>;
  return {
    id: String(item.id ?? ""),
    conditionId: String(item.conditionId ?? ""),
    slug: String(item.slug ?? ""),
    asset: String(item.asset ?? "") as PolymarketMarket["asset"],
    period: String(item.period ?? "") as PolymarketMarket["period"],
    title: String(item.title ?? ""),
    windowStart: String(item.windowStart ?? ""),
    windowEnd: String(item.windowEnd ?? ""),
    upTokenId: String(item.upTokenId ?? ""),
    downTokenId: String(item.downTokenId ?? ""),
    tickSize: String(item.tickSize ?? ""),
    negativeRisk: Boolean(item.negativeRisk),
    active: Boolean(item.active),
  };
}

const MAX_CHART_POINTS = 300;

function downsamplePoints<T>(points: T[], maxPoints = MAX_CHART_POINTS): T[] {
  if (points.length <= maxPoints) return points;
  const step = (points.length - 1) / (maxPoints - 1);
  return Array.from({ length: maxPoints }, (_, index) => {
    const sourceIndex = Math.round(index * step);
    return points[Math.min(sourceIndex, points.length - 1)];
  });
}

export function mergeSnapshot(
  current: PolymarketMarketSnapshot | null,
  incoming: PolymarketMarketSnapshot,
): PolymarketMarketSnapshot {
  if (current && current.market.id !== incoming.market.id) {
    return {
      ...incoming,
      priceSeries: downsamplePoints(incoming.priceSeries),
    };
  }
  if (!current || incoming.priceSeries.length > 1) {
    return {
      ...incoming,
      priceSeries: downsamplePoints(incoming.priceSeries),
    };
  }
  if (incoming.priceSeries.length === 0) {
    return {
      ...current,
      ...incoming,
      priceSeries: current.priceSeries,
    };
  }
  const merged = [...current.priceSeries];
  for (const point of incoming.priceSeries) {
    const index = merged.findIndex((item) => item.timestamp === point.timestamp);
    if (index >= 0) merged[index] = point;
    else merged.push(point);
  }
  merged.sort(
    (left, right) =>
      new Date(left.timestamp).getTime() - new Date(right.timestamp).getTime(),
  );
  return {
    ...current,
    ...incoming,
    priceSeries: downsamplePoints(merged),
  };
}

export type PolymarketLiveQuote = Omit<PolymarketMarketSnapshot, "priceSeries">;

export function liveQuoteFromSnapshot(
  snapshot: PolymarketMarketSnapshot,
): PolymarketLiveQuote {
  const { priceSeries, ...quote } = snapshot;
  void priceSeries;
  return quote;
}

export function applySnapshotParts(
  currentSnapshot: PolymarketMarketSnapshot | null,
  currentChartPoints: PolymarketPricePoint[],
  incoming: PolymarketMarketSnapshot,
): { liveSnapshot: PolymarketMarketSnapshot; chartPoints: PolymarketPricePoint[] } {
  const merged = mergeSnapshot(currentSnapshot, incoming);
  const marketChanged = currentSnapshot?.market.id !== incoming.market.id;
  let chartPoints = currentChartPoints;
  if (marketChanged || incoming.priceSeries.length > 0) {
    chartPoints = merged.priceSeries;
  }
  return {
    liveSnapshot: { ...merged, priceSeries: chartPoints },
    chartPoints,
  };
}

export function mapSnapshot(value: unknown): PolymarketMarketSnapshot {
  const item = value as Record<string, unknown>;
  const points = Array.isArray(item.priceSeries) ? item.priceSeries : [];
  return {
    market: market(item.market),
    openPrice: decimal(item.openPrice),
    chainlinkPrice: decimal(item.chainlinkPrice),
    upBid: decimal(item.upBid),
    upAsk: decimal(item.upAsk),
    downBid: decimal(item.downBid),
    downAsk: decimal(item.downAsk),
    priceSeries: points.map((point) => {
      const source = point as Record<string, unknown>;
      return {
        timestamp: String(source.timestamp ?? ""),
        openPrice: decimal(source.openPrice),
        chainlinkPrice: decimal(source.chainlinkPrice),
      };
    }),
    sourceUpdatedAt: String(item.sourceUpdatedAt ?? ""),
    stale: Boolean(item.stale),
    version: String(item.version ?? ""),
  };
}

export async function fetchPolymarketMarkets(): Promise<PolymarketMarket[]> {
  const response = await fetch(url("/markets?active=true"), {
    headers: { Accept: "application/json" },
    cache: "no-store",
  });
  if (!response.ok) {
    throw new Error(await errorMessage(response, "无法加载 Polymarket 市场"));
  }
  const payload = (await response.json()) as { data?: unknown[] };
  return (payload.data ?? []).map(market);
}

export async function fetchPolymarketSnapshot(
  marketId: string,
  etag?: string,
): Promise<{ snapshot: PolymarketMarketSnapshot | null; etag: string }> {
  const response = await fetch(url(`/markets/${encodeURIComponent(marketId)}`), {
    headers: {
      Accept: "application/json",
      ...(etag ? { "If-None-Match": etag } : {}),
    },
    cache: "no-store",
  });
  if (response.status === 304) return { snapshot: null, etag: etag ?? "" };
  if (!response.ok) {
    throw new Error(await errorMessage(response, "无法加载市场快照"));
  }
  const payload = (await response.json()) as { data: unknown };
  return {
    snapshot: mapSnapshot(payload.data),
    etag: response.headers.get("ETag") ?? "",
  };
}

export function polymarketStreamUrl(marketId: string): string {
  return url(`/stream?marketId=${encodeURIComponent(marketId)}`);
}

export async function fetchPolymarketAccountSummary(
  accountId: number,
): Promise<PolymarketAccountSummary> {
  const response = await fetch(url(`/accounts/${accountId}/summary`), {
    credentials: "include",
    headers: { Accept: "application/json" },
    cache: "no-store",
  });
  if (!response.ok) {
    throw new Error(await errorMessage(response, "无法加载账户资产"));
  }
  const { data } = (await response.json()) as {
    data: Record<string, unknown>;
  };
  return {
    tradingAccountId: Number(data.tradingAccountId),
    accountName: String(data.accountName ?? ""),
    walletAddress: String(data.walletAddress ?? ""),
    availableBalance: decimal(data.availableBalance) ?? 0,
    positionValue: decimal(data.positionValue) ?? 0,
    totalAssets: decimal(data.totalAssets) ?? 0,
    sourceUpdatedAt: String(data.sourceUpdatedAt ?? ""),
    stale: Boolean(data.stale),
    bindingStatus: String(data.bindingStatus ?? ""),
  };
}

export async function fetchPolymarketPositions(
  accountId: number,
): Promise<{ positions: PolymarketPosition[]; stale: boolean }> {
  const response = await fetch(url(`/accounts/${accountId}/positions`), {
    credentials: "include",
    headers: { Accept: "application/json" },
    cache: "no-store",
  });
  if (!response.ok) {
    throw new Error(await errorMessage(response, "无法加载持仓"));
  }
  const payload = (await response.json()) as {
    data?: Record<string, unknown>[];
    stale?: boolean;
  };
  return {
    positions: (payload.data ?? []).map((item) => ({
      id: String(item.id ?? item.tokenId ?? ""),
      tradingAccountId: Number(item.tradingAccountId),
      conditionId: String(item.conditionId ?? ""),
      tokenId: String(item.tokenId ?? ""),
      market: String(item.market ?? ""),
      outcome: String(item.outcome ?? ""),
      size: decimal(item.size) ?? 0,
      averagePrice: decimal(item.averagePrice) ?? 0,
      currentPrice: decimal(item.currentPrice) ?? 0,
      initialValue: decimal(item.initialValue) ?? 0,
      currentValue: decimal(item.currentValue) ?? 0,
      cashPnl: decimal(item.cashPnl) ?? 0,
      percentPnl: decimal(item.percentPnl) ?? 0,
      redeemable: Boolean(item.redeemable),
      sourceUpdatedAt: String(item.sourceUpdatedAt ?? ""),
    })),
    stale: Boolean(payload.stale),
  };
}

export function mapPolymarketOpenOrder(value: unknown): PolymarketOpenOrder {
  const item = value as Record<string, unknown>;
  return {
    id: String(item.id ?? ""),
    tokenId: String(item.tokenId ?? ""),
    conditionId: String(item.conditionId ?? ""),
    marketTitle: String(item.marketTitle ?? ""),
    outcome: String(item.outcome ?? ""),
    side: String(item.side ?? "") as PolymarketOpenOrder["side"],
    price: decimal(item.price) ?? 0,
    originalSize: decimal(item.originalSize) ?? 0,
    matchedSize: decimal(item.matchedSize) ?? 0,
    remainingSize: decimal(item.remainingSize) ?? 0,
    status: String(item.status ?? ""),
    orderType: String(item.orderType ?? ""),
    createdAt: String(item.createdAt ?? ""),
  };
}

export interface PolymarketAccountEvent {
  type: "snapshot" | "order" | "portfolio" | "heartbeat";
  order: PolymarketOpenOrder | null;
  openOrders: PolymarketOpenOrder[];
  portfolioChanged: boolean;
}

export function mapPolymarketAccountEvent(value: unknown): PolymarketAccountEvent {
  const item = value as Record<string, unknown>;
  return {
    type: String(item.type ?? "heartbeat") as PolymarketAccountEvent["type"],
    order: item.order ? mapPolymarketOpenOrder(item.order) : null,
    openOrders: Array.isArray(item.openOrders)
      ? item.openOrders.map(mapPolymarketOpenOrder)
      : [],
    portfolioChanged: Boolean(item.portfolioChanged),
  };
}

export function polymarketAccountStreamUrl(accountId: number): string {
  return url(`/accounts/${accountId}/events`);
}

export async function fetchPolymarketOpenOrders(
  accountId: number,
): Promise<{ openOrders: PolymarketOpenOrder[]; stale: boolean }> {
  const response = await fetch(url(`/accounts/${accountId}/open-orders`), {
    credentials: "include",
    headers: { Accept: "application/json" },
    cache: "no-store",
  });
  if (!response.ok) {
    throw new Error(await errorMessage(response, "无法加载挂单"));
  }
  const payload = (await response.json()) as {
    data?: unknown[];
    stale?: boolean;
  };
  return {
    openOrders: (payload.data ?? []).map(mapPolymarketOpenOrder),
    stale: Boolean(payload.stale),
  };
}

export async function cancelPolymarketOrder(
  accountId: number,
  orderId: string,
): Promise<{ orderId: string; status: string; message: string }> {
  const response = await fetch(
    url(`/accounts/${accountId}/orders/${encodeURIComponent(orderId)}`),
    {
      method: "DELETE",
      credentials: "include",
      headers: { Accept: "application/json" },
      cache: "no-store",
    },
  );
  if (!response.ok) {
    throw new Error(await errorMessage(response, "撤单失败"));
  }
  const payload = (await response.json()) as {
    data?: { orderId?: unknown; status?: unknown; message?: unknown };
  };
  return {
    orderId: String(payload.data?.orderId ?? orderId),
    status: String(payload.data?.status ?? "canceled"),
    message: String(payload.data?.message ?? ""),
  };
}

export async function placePolymarketOrder(input: {
  tradingAccountId: number;
  marketId: string;
  outcome: "up" | "down";
  side: "buy" | "sell";
  amount: string;
  amountUnit: "usd" | "shares";
  executionType?: "book" | "limit";
  limitPrice?: string;
}): Promise<PolymarketOrder> {
  const idempotencyKey = createIdempotencyKey();
  const response = await fetch(url("/orders"), {
    method: "POST",
    credentials: "include",
    headers: {
      Accept: "application/json",
      "Content-Type": "application/json",
      "Idempotency-Key": idempotencyKey,
    },
    body: JSON.stringify({ ...input, idempotencyKey }),
  });
  if (!response.ok) {
    throw new Error(await errorMessage(response, "订单提交失败"));
  }
  const { data } = (await response.json()) as {
    data: Record<string, unknown>;
  };
  return {
    id: String(data.id ?? ""),
    clobOrderId: String(data.clobOrderId ?? ""),
    tradingAccountId: Number(data.tradingAccountId),
    marketId: String(data.marketId ?? ""),
    tokenId: String(data.tokenId ?? ""),
    outcome: String(data.outcome) as "up" | "down",
    side: String(data.side) as "buy" | "sell",
    requestedAmount: decimal(data.requestedAmount) ?? 0,
    amountUnit: String(data.amountUnit) as "usd" | "shares",
    executionType: (String(data.executionType || "book") as "book" | "limit"),
    limitPrice: decimal(data.limitPrice),
    clobOrderType: (String(data.clobOrderType || "FAK") as "FAK" | "GTC"),
    filledSize: decimal(data.filledSize) ?? 0,
    averagePrice: decimal(data.averagePrice) ?? 0,
    status: String(data.status ?? ""),
    errorCode: String(data.errorCode ?? ""),
    errorMessage: String(data.errorMessage ?? ""),
    createdAt: String(data.createdAt ?? ""),
    updatedAt: String(data.updatedAt ?? ""),
  };
}
