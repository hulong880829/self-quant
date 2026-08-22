export type ReportStatus = "final" | "provisional" | "failed";
export type CashFlowType = "subscription" | "redemption" | "deposit" | "withdrawal";
export type CashFlowStatus = "pending" | "confirmed" | "canceled";

export interface ReportProduct {
  id: string;
  name: string;
  displayName: string;
  category: string;
  strategy: string;
  currency: string;
  inceptionDate: string | null;
  status: string;
}

export interface ReportRow {
  date: string;
  absoluteReturn: number;
  dailyReturn: number | null;
  annualizedReturn: number | null;
  annualized7d: number | null;
  annualized30d: number | null;
  maxDrawdown: number | null;
  capitalUtilization: number | null;
  sharpe: number | null;
  aum: number;
  volume24h: number | null;
  status: ReportStatus;
  partial: boolean;
}

export interface AumPoint {
  date: string;
  aum: number;
}

export interface ProductReport {
  product: ReportProduct;
  latest: ReportRow | null;
  rows: ReportRow[];
  aumSeries: AumPoint[];
  status: ReportStatus | null;
  partial: boolean;
  errors: string[];
  dataDate: string | null;
  serverTime: string | null;
}

export interface ProductCashFlow {
  id: string;
  productId: string;
  type: CashFlowType;
  amount: string;
  currency: string;
  occurredAt: string;
  note: string;
  status: CashFlowStatus;
  createdAt: string;
  updatedAt: string;
}

export interface CreateCashFlowPayload {
  type: CashFlowType;
  amount: string;
  currency: string;
  occurredAt: string;
  note?: string;
  status: "confirmed";
}

type JsonRecord = Record<string, unknown>;

function apiBaseUrl(): string {
  return (process.env.NEXT_PUBLIC_API_BASE_URL ?? "").replace(/\/+$/, "");
}

function productUrl(productId = ""): string {
  const suffix = productId ? `/${encodeURIComponent(productId)}` : "";
  return `${apiBaseUrl()}/api/v1/reports/products${suffix}`;
}

function record(value: unknown, path: string): JsonRecord {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`${path} 必须是对象`);
  }
  return value as JsonRecord;
}

function text(value: unknown, path: string, fallback?: string): string {
  if (value === undefined && fallback !== undefined) return fallback;
  if (typeof value !== "string") throw new Error(`${path} 必须是字符串`);
  return value;
}

function decimal(value: unknown, path: string, nullable = false): number | null {
  if (nullable && (value === null || value === undefined || value === "")) return null;
  if (typeof value !== "string" || value.trim() === "") {
    throw new Error(`${path} 必须是 decimal string`);
  }
  const parsed = Number(value);
  if (!Number.isFinite(parsed)) throw new Error(`${path} 不是有限十进制数`);
  return parsed;
}

function boolean(value: unknown, path: string, fallback = false): boolean {
  if (value === undefined) return fallback;
  if (typeof value !== "boolean") throw new Error(`${path} 必须是 boolean`);
  return value;
}

function reportStatus(value: unknown, path: string): ReportStatus {
  if (value === "final" || value === "provisional" || value === "failed") return value;
  throw new Error(`${path} 不是有效报表状态`);
}

function cashFlowStatus(value: unknown, path: string): CashFlowStatus {
  if (value === "pending" || value === "confirmed" || value === "canceled") return value;
  throw new Error(`${path} 不是有效流水状态`);
}

function cashFlowType(value: unknown, path: string): CashFlowType {
  if (
    value === "subscription" ||
    value === "redemption" ||
    value === "deposit" ||
    value === "withdrawal"
  ) {
    return value;
  }
  throw new Error(`${path} 不是有效流水类型`);
}

function dataEnvelope(value: unknown): { data: unknown; meta: JsonRecord } {
  const response = record(value, "response");
  return {
    data: response.data,
    meta:
      response.meta === undefined ? {} : record(response.meta, "response.meta"),
  };
}

export function mapReportProduct(value: unknown, path = "product"): ReportProduct {
  const item = record(value, path);
  const name = text(item.name, `${path}.name`);
  return {
    id: text(item.id, `${path}.id`),
    name,
    displayName: text(item.displayName, `${path}.displayName`, name),
    category: text(item.category, `${path}.category`, ""),
    strategy: text(item.strategy, `${path}.strategy`, ""),
    currency: text(item.currency, `${path}.currency`),
    inceptionDate:
      item.inceptionDate == null
        ? null
        : text(item.inceptionDate, `${path}.inceptionDate`),
    status: text(item.status, `${path}.status`, "active"),
  };
}

export function mapReportRow(value: unknown, path = "row"): ReportRow {
  const item = record(value, path);
  return {
    date: text(item.date, `${path}.date`),
    absoluteReturn: decimal(item.absoluteReturn, `${path}.absoluteReturn`) as number,
    dailyReturn: decimal(item.dailyReturn, `${path}.dailyReturn`, true),
    annualizedReturn: decimal(item.annualizedReturn, `${path}.annualizedReturn`, true),
    annualized7d: decimal(item.annualized7d, `${path}.annualized7d`, true),
    annualized30d: decimal(item.annualized30d, `${path}.annualized30d`, true),
    maxDrawdown: decimal(item.maxDrawdown, `${path}.maxDrawdown`, true),
    capitalUtilization: decimal(
      item.capitalUtilization,
      `${path}.capitalUtilization`,
      true,
    ),
    sharpe: decimal(item.sharpe, `${path}.sharpe`, true),
    aum: decimal(item.aum, `${path}.aum`) as number,
    volume24h: decimal(item.volume24h, `${path}.volume24h`, true),
    status: reportStatus(item.status ?? "final", `${path}.status`),
    partial: boolean(item.partial, `${path}.partial`),
  };
}

export function mapProductReportResponse(value: unknown): ProductReport {
  const { data, meta } = dataEnvelope(value);
  const item = record(data, "response.data");
  const rowsValue = item.rows ?? item.daily ?? [];
  const seriesValue = item.aumSeries ?? [];
  if (!Array.isArray(rowsValue)) throw new Error("response.data.rows 必须是数组");
  if (!Array.isArray(seriesValue)) throw new Error("response.data.aumSeries 必须是数组");
  const rows = rowsValue.map((row, index) => mapReportRow(row, `rows[${index}]`));
  const aumSeries = seriesValue
    .map((point, index) => {
      const mapped = record(point, `aumSeries[${index}]`);
      return {
        date: text(mapped.date, `aumSeries[${index}].date`),
        aum: decimal(mapped.aum, `aumSeries[${index}].aum`) as number,
      };
    })
    .sort((left, right) => left.date.localeCompare(right.date));
  const rawErrors = item.errors ?? meta.errors ?? [];
  if (!Array.isArray(rawErrors)) throw new Error("errors 必须是数组");
  const latest =
    item.latest == null
      ? rows[0] ?? null
      : mapReportRow(item.latest, "response.data.latest");
  const rawStatus = item.status ?? meta.status ?? latest?.status ?? null;
  return {
    product: mapReportProduct(item.product, "response.data.product"),
    latest,
    rows,
    aumSeries,
    status: rawStatus === null ? null : reportStatus(rawStatus, "status"),
    partial: boolean(item.partial ?? meta.partial, "partial"),
    errors: rawErrors.map((error, index) => text(error, `errors[${index}]`)),
    dataDate:
      item.dataDate == null && meta.dataDate == null
        ? latest?.date ?? null
        : text(item.dataDate ?? meta.dataDate, "dataDate"),
    serverTime:
      item.serverTime == null && meta.serverTime == null
        ? null
        : text(item.serverTime ?? meta.serverTime, "serverTime"),
  };
}

export function mapCashFlow(value: unknown, path = "cashFlow"): ProductCashFlow {
  const item = record(value, path);
  const rawAmount = text(item.amount, `${path}.amount`);
  decimal(rawAmount, `${path}.amount`);
  return {
    id: text(item.id, `${path}.id`),
    productId: text(item.productId, `${path}.productId`),
    type: cashFlowType(item.type, `${path}.type`),
    amount: rawAmount,
    currency: text(item.currency, `${path}.currency`),
    occurredAt: text(item.occurredAt, `${path}.occurredAt`),
    note: text(item.note, `${path}.note`, ""),
    status: cashFlowStatus(item.status, `${path}.status`),
    createdAt: text(item.createdAt, `${path}.createdAt`, ""),
    updatedAt: text(item.updatedAt, `${path}.updatedAt`, ""),
  };
}

async function request(path: string, init: RequestInit, fallback: string): Promise<unknown> {
  const response = await fetch(path, {
    credentials: "include",
    cache: "no-store",
    headers: {
      Accept: "application/json",
      ...(init.body ? { "Content-Type": "application/json" } : {}),
      ...init.headers,
    },
    ...init,
  });
  const body = (await response.json().catch(() => ({}))) as JsonRecord;
  if (response.status === 401) throw new Error("请先登录");
  if (!response.ok) {
    throw new Error(typeof body.error === "string" ? body.error : fallback);
  }
  return body;
}

export async function fetchReportProducts(signal?: AbortSignal): Promise<ReportProduct[]> {
  const body = await request(
    productUrl(),
    { method: "GET", signal },
    "无法加载报表产品",
  );
  const { data } = dataEnvelope(body);
  if (!Array.isArray(data)) throw new Error("response.data 必须是数组");
  return data.map((item, index) => mapReportProduct(item, `data[${index}]`));
}

export async function fetchProductReport(
  productId: string,
  signal?: AbortSignal,
): Promise<ProductReport> {
  const body = await request(
    productUrl(productId),
    { method: "GET", signal },
    "无法加载产品报表",
  );
  return mapProductReportResponse(body);
}

export async function fetchProductCashFlows(
  productId: string,
  signal?: AbortSignal,
): Promise<ProductCashFlow[]> {
  const body = await request(
    `${productUrl(productId)}/cash-flows`,
    { method: "GET", signal },
    "无法加载资金流水",
  );
  const { data } = dataEnvelope(body);
  if (!Array.isArray(data)) throw new Error("response.data 必须是数组");
  return data.map((item, index) => mapCashFlow(item, `data[${index}]`));
}

export async function createProductCashFlow(
  productId: string,
  payload: CreateCashFlowPayload,
): Promise<ProductCashFlow> {
  const body = await request(
    `${productUrl(productId)}/cash-flows`,
    { method: "POST", body: JSON.stringify(payload) },
    "登记资金流水失败",
  );
  return mapCashFlow(dataEnvelope(body).data);
}

