export type BasisSpreadRange = "1h" | "4h" | "8h" | "24h" | "7d";
export type BasisSpreadAvailability = "available" | "unavailable";

export interface BasisSpreadPoint {
  ts: string;
  spreadBps: number;
  spotAsk: number;
  perpetualAsk: number;
  samples: number;
}

export interface BasisSpreadSummary {
  currentBps: number;
  minBps: number;
  maxBps: number;
  avgBps: number;
  coverage: number;
}

export interface BasisSpreadHistory {
  venue: string;
  compareVenue?: string;
  baseAsset: string;
  quoteAsset: string;
  canonicalSymbol: string;
  range: BasisSpreadRange;
  resolutionSeconds: number;
  availability: BasisSpreadAvailability;
  asOf: string;
  points: BasisSpreadPoint[];
  summary: BasisSpreadSummary;
}

export type BasisSpreadFetchResult =
  | { status: "updated"; history: BasisSpreadHistory; etag: string | null }
  | { status: "unchanged"; etag: string | null };

const ranges = new Set<BasisSpreadRange>(["1h", "4h", "8h", "24h", "7d"]);

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

function rangeValue(value: unknown, path: string): BasisSpreadRange {
  const parsed = text(value, path) as BasisSpreadRange;
  if (!ranges.has(parsed)) {
    throw new Error(`${path} 不是支持的周期`);
  }
  return parsed;
}

function availability(value: unknown, path: string): BasisSpreadAvailability {
  const parsed = text(value, path);
  if (parsed !== "available" && parsed !== "unavailable") {
    throw new Error(`${path} 必须是 available 或 unavailable`);
  }
  return parsed;
}

function point(value: unknown, path: string): BasisSpreadPoint {
  const item = record(value, path);
  return {
    ts: isoTime(item.ts, `${path}.ts`),
    spreadBps: decimal(item.spreadBps, `${path}.spreadBps`),
    spotAsk: decimal(item.spotAsk, `${path}.spotAsk`),
    perpetualAsk: decimal(item.perpetualAsk, `${path}.perpetualAsk`),
    samples: nonNegativeInteger(item.samples, `${path}.samples`),
  };
}

export function mapBasisSpreadHistory(value: unknown): BasisSpreadHistory {
  const item = record(value, "response");
  if (!Array.isArray(item.points)) {
    throw new Error("response.points 必须是数组");
  }
  const summary = record(item.summary, "response.summary");
  return {
    venue: text(item.venue, "response.venue"),
    compareVenue:
      item.compareVenue === undefined || item.compareVenue === ""
        ? undefined
        : text(item.compareVenue, "response.compareVenue"),
    baseAsset: text(item.baseAsset, "response.baseAsset"),
    quoteAsset: text(item.quoteAsset, "response.quoteAsset"),
    canonicalSymbol: text(item.canonicalSymbol, "response.canonicalSymbol"),
    range: rangeValue(item.range, "response.range"),
    resolutionSeconds: integer(item.resolutionSeconds, "response.resolutionSeconds"),
    availability: availability(item.availability, "response.availability"),
    asOf: isoTime(item.asOf, "response.asOf"),
    points: item.points.map((entry, index) => point(entry, `response.points[${index}]`)),
    summary: {
      currentBps: decimal(summary.currentBps, "response.summary.currentBps"),
      minBps: decimal(summary.minBps, "response.summary.minBps"),
      maxBps: decimal(summary.maxBps, "response.summary.maxBps"),
      avgBps: decimal(summary.avgBps, "response.summary.avgBps"),
      coverage: decimal(summary.coverage, "response.summary.coverage"),
    },
  };
}

function historyUrl(
  venue: string,
  baseAsset: string,
  quoteAsset: string,
  range: BasisSpreadRange,
  compareVenue?: string,
) {
  const baseUrl = (process.env.NEXT_PUBLIC_API_BASE_URL ?? "").replace(/\/+$/, "");
  const query = new URLSearchParams({ range });
  if (compareVenue) {
    query.set("compareVenue", compareVenue.toLowerCase());
  }
  return `${baseUrl}/api/v1/basis-spreads/${encodeURIComponent(venue.toLowerCase())}/${encodeURIComponent(baseAsset.toUpperCase())}/${encodeURIComponent(quoteAsset.toUpperCase())}/history?${query.toString()}`;
}

export async function fetchBasisSpreadHistory(
  venue: string,
  baseAsset: string,
  quoteAsset: string,
  range: BasisSpreadRange,
  etag?: string | null,
  signal?: AbortSignal,
  compareVenue?: string,
): Promise<BasisSpreadFetchResult> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (etag) {
    headers["If-None-Match"] = etag;
  }
  const response = await fetch(historyUrl(venue, baseAsset, quoteAsset, range, compareVenue), {
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
    throw new Error(`期现价差 API 请求失败 (${response.status})`);
  }
  return {
    status: "updated",
    history: mapBasisSpreadHistory(await response.json()),
    etag: response.headers.get("ETag"),
  };
}
