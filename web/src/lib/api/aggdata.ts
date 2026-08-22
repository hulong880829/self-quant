import {
  fillHistoryTimeline,
  fixed,
  formatFixed,
  type AggregatedBookLevel,
  type BookSide,
  type FixedDecimal,
  type SpreadPoint,
} from "../orderbook";

export interface AggdataMarket {
  profile: string;
  symbol: string;
  baseAsset: string;
  quoteAsset: string;
  priceScale: number;
  quantityScale: number;
  hasOrderBook: boolean;
}

export interface AggdataSnapshot {
  symbol: string;
  sequence: bigint;
  generation: bigint;
  timestamp: string;
  levels: AggregatedBookLevel[];
}

export interface AggdataFrame {
  kind: "snapshot";
  sequence: bigint;
  generation: bigint;
  timestampMs: bigint;
  priceScale: number;
  quantityScale: number;
  levels: AggregatedBookLevel[];
}

export interface AggdataHistory {
  points: SpreadPoint[];
  distribution: number[];
  startMs: number;
  endMs: number;
  resolutionMs: number;
  coverage: number;
  gapCount: number;
}

export interface AggdataFairPricePoint {
  timestamp: string;
  sourceWallNS: bigint;
  ringEpoch: bigint;
  sequence: bigint;
  modelId: string;
  price: number;
  priceFixed: FixedDecimal;
  degraded: boolean;
  degradedReasons: string[];
}

export interface AggdataFairPriceFrame extends AggdataFairPricePoint {
  type: "data";
  channel: "fairprice";
  profile: string;
  symbol: string;
}

export interface AggdataFairPriceReset {
  type: "reset";
  channel: "fairprice";
  profile: string;
  symbol: string;
  reason: string;
}

export interface AggdataFairPriceAck {
  type: "ack";
  ok: boolean;
  error: string | null;
}

export type AggdataFairPriceMessage =
  | AggdataFairPriceFrame
  | AggdataFairPriceReset
  | AggdataFairPriceAck;

export interface AggdataFairPriceHistory {
  profile: string;
  symbol: string;
  modelId: string;
  resolutionMs: number;
  points: AggdataFairPricePoint[];
}

const textDecoder = new TextDecoder();

function object(value: unknown, path: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`${path} must be an object`);
  }
  return value as Record<string, unknown>;
}

function text(value: unknown, path: string) {
  if (typeof value !== "string" || value.length === 0) {
    throw new Error(`${path} must be a non-empty string`);
  }
  return value;
}

function scale(value: unknown, path: string) {
  const result = Number(value);
  if (!Number.isInteger(result) || result < 0 || result > 18) {
    throw new Error(`${path} must be an integer from 0 through 18`);
  }
  return result;
}

function bigint(value: unknown, path: string) {
  if (
    (typeof value !== "string" && typeof value !== "number" && typeof value !== "bigint") ||
    (typeof value === "number" && !Number.isInteger(value))
  ) {
    throw new Error(`${path} must be an integer string or integer`);
  }
  try {
    return BigInt(value);
  } catch {
    throw new Error(`${path} must be an integer`);
  }
}

function decimal(
  value: unknown,
  fallbackScale: number,
  path: string,
): FixedDecimal {
  if (typeof value === "object" && value !== null) {
    const wire = object(value, path);
    return fixed(
      bigint(wire.mantissa ?? wire.value, `${path}.mantissa`),
      scale(wire.scale ?? fallbackScale, `${path}.scale`),
    );
  }
  return fixed(bigint(value, path), fallbackScale);
}

export function fixedToSafeNumber(value: FixedDecimal) {
  const result = Number(formatFixed(value));
  if (!Number.isFinite(result)) throw new Error("fixed decimal must be finite");
  return result;
}

function stringArray(value: unknown, path: string) {
  if (!Array.isArray(value) || value.some((item) => typeof item !== "string")) {
    throw new Error(`${path} must be a string array`);
  }
  return value as string[];
}

function unwrapArray(value: unknown, keys: string[], path: string): unknown[] {
  if (Array.isArray(value)) return value;
  const envelope = object(value, path);
  for (const key of keys) {
    if (Array.isArray(envelope[key])) return envelope[key];
  }
  throw new Error(`${path} must contain an array`);
}

export function mapMarketsResponse(value: unknown): AggdataMarket[] {
  return unwrapArray(value, ["markets", "data"], "markets response").map(
    (raw, index) => {
      const item = object(raw, `markets[${index}]`);
      return {
        symbol: text(item.symbol ?? item.market, `markets[${index}].symbol`),
        profile: text(item.profile, `markets[${index}].profile`),
        baseAsset: text(
          item.baseAsset ?? item.base_asset ?? item.base,
          `markets[${index}].baseAsset`,
        ),
        quoteAsset: text(
          item.quoteAsset ?? item.quote_asset ?? item.quote,
          `markets[${index}].quoteAsset`,
        ),
        priceScale: scale(
          item.priceScale ?? item.price_scale,
          `markets[${index}].priceScale`,
        ),
        quantityScale: scale(
          item.quantityScale ?? item.quantity_scale,
          `markets[${index}].quantityScale`,
        ),
        hasOrderBook: item.has_order_book === true,
      };
    },
  );
}

function mapLevelContributions(
  value: unknown,
  index: number,
  levelSide: BookSide,
  priceScale: number,
  quantityScale: number,
): AggregatedBookLevel[] {
  const item = object(value, `levels[${index}]`);
  const price = decimal(item.price, priceScale, `levels[${index}].price`);
  const contributions = unwrapArray(
    item.contributions,
    ["contributions"],
    `levels[${index}].contributions`,
  );
  return contributions.map((rawContribution, contributionIndex) => {
    const contribution = object(
      rawContribution,
      `levels[${index}].contributions[${contributionIndex}]`,
    );
    return {
      side: levelSide,
      price,
      quantity: decimal(
        contribution.quantity,
        quantityScale,
        `levels[${index}].contributions[${contributionIndex}].quantity`,
      ),
      exchange: text(
        contribution.venue,
        `levels[${index}].contributions[${contributionIndex}].venue`,
      ),
    };
  });
}

export function mapSnapshotResponse(
  value: unknown,
  market: AggdataMarket,
): AggdataSnapshot {
  const outer = object(value, "snapshot response");
  const item =
    typeof outer.data === "object" && outer.data !== null
      ? object(outer.data, "snapshot response.data")
      : outer;
  const book = object(item.orderbook, "snapshot.orderbook");
  const bids = unwrapArray(book.bids, ["bids"], "snapshot.orderbook.bids");
  const asks = unwrapArray(book.asks, ["asks"], "snapshot.orderbook.asks");
  const wallNS = bigint(book.wall_ns, "snapshot.orderbook.wall_ns");
  return {
    symbol: text(item.symbol ?? item.market ?? market.symbol, "snapshot.symbol"),
    sequence: bigint(book.sequence, "snapshot.orderbook.sequence"),
    generation: bigint(book.generation, "snapshot.orderbook.generation"),
    timestamp: new Date(Number(wallNS / 1_000_000n)).toISOString(),
    levels: [
      ...bids.flatMap((level, index) =>
        mapLevelContributions(
          level,
          index,
          "bid",
          market.priceScale,
          market.quantityScale,
        ),
      ),
      ...asks.flatMap((level, index) =>
        mapLevelContributions(
          level,
          index,
          "ask",
          market.priceScale,
          market.quantityScale,
        ),
      ),
    ],
  };
}

export function mapHistoryResponse(value: unknown) {
  const response = object(value, "history response");
  const startNS = bigint(response.start_ns, "history response.start_ns");
  const endNS = bigint(response.end_ns, "history response.end_ns");
  const resolutionNS = bigint(
    response.resolution_ns,
    "history response.resolution_ns",
  );
  const startMs = Number(startNS / 1_000_000n);
  const endMs = Number(endNS / 1_000_000n);
  const resolutionMs = Number(resolutionNS / 1_000_000n);
  const bucketValues = unwrapArray(
    response.buckets,
    ["buckets"],
    "history buckets",
  );
  let coveredBuckets = 0;
  const bucketPoints = bucketValues.map(
    (raw, index): SpreadPoint => {
      const item = object(raw, `buckets[${index}]`);
      const startNS = bigint(item.start_ns, `buckets[${index}].start_ns`);
      const close = Number(item.close);
      if (!Number.isFinite(close)) throw new Error(`buckets[${index}].close must be finite`);
      const coverage = item.coverage === undefined ? 1 : Number(item.coverage);
      if (!Number.isFinite(coverage) || coverage < 0 || coverage > 1) {
        throw new Error(`buckets[${index}].coverage must be between zero and one`);
      }
      coveredBuckets += coverage;
      return {
        timestamp: new Date(Number(startNS / 1_000_000n)).toISOString(),
        spreadBps: close,
      };
    },
  );
  const gaps = unwrapArray(response.gaps ?? [], ["gaps"], "history gaps");
  gaps.forEach((raw, index) => {
    const gap = object(raw, `gaps[${index}]`);
    const gapStart = bigint(gap.start_ns, `gaps[${index}].start_ns`);
    const gapEnd = bigint(gap.end_ns, `gaps[${index}].end_ns`);
    if (gapEnd <= gapStart) {
      throw new Error(`gaps[${index}] must have increasing bounds`);
    }
  });
  const points = fillHistoryTimeline(
    bucketPoints,
    startMs,
    endMs,
    resolutionMs,
  );
  const rawDistribution = Array.isArray(response.distribution_bps)
    ? response.distribution_bps
    : [];
  const distribution = rawDistribution.map((value, index) => {
    const parsed = Number(value);
    if (!Number.isFinite(parsed)) {
      throw new Error(`distribution_bps[${index}] must be finite`);
    }
    return parsed;
  });
  return {
    points,
    distribution,
    startMs,
    endMs,
    resolutionMs,
    coverage: points.length === 0 ? 0 : coveredBuckets / points.length,
    gapCount: gaps.length,
  };
}

export function aggdataBaseUrl() {
  return (process.env.NEXT_PUBLIC_AGGDATA_BASE_URL ?? "http://127.0.0.1:9093").replace(
    /\/+$/,
    "",
  );
}

export function aggdataWebSocketUrl(baseUrl = aggdataBaseUrl()) {
  const configured = process.env.NEXT_PUBLIC_AGGDATA_WS_URL;
  const url = new URL(configured ??
    `${baseUrl.replace(/^http:/, "ws:").replace(/^https:/, "wss:")}/v1/stream`);
  const token = process.env.NEXT_PUBLIC_AGGDATA_TOKEN;
  if (token) url.searchParams.set("token", token);
  return url.toString();
}

export function mapFairPriceHistoryResponse(value: unknown): AggdataFairPriceHistory {
  const response = object(value, "fair price history response");
  const profile = text(response.profile, "fair price history.profile");
  const symbol = text(response.symbol, "fair price history.symbol");
  const modelId = text(response.model_id, "fair price history.model_id");
  const resolutionMs = Number(response.resolution_ms);
  if (!Number.isInteger(resolutionMs) || resolutionMs <= 0) {
    throw new Error("fair price history.resolution_ms must be positive");
  }
  const points = unwrapArray(response.points, ["points"], "fair price history.points").map(
    (raw, index): AggdataFairPricePoint => {
      const point = object(raw, `fair price history.points[${index}]`);
      const priceFixed = decimal(
        point.price,
        0,
        `fair price history.points[${index}].price`,
      );
      return {
        timestamp: text(
          point.observed_at,
          `fair price history.points[${index}].observed_at`,
        ),
        sourceWallNS: bigint(
          point.source_wall_ns,
          `fair price history.points[${index}].source_wall_ns`,
        ),
        ringEpoch: bigint(
          point.ring_epoch,
          `fair price history.points[${index}].ring_epoch`,
        ),
        sequence: bigint(point.seq, `fair price history.points[${index}].seq`),
        modelId,
        price: fixedToSafeNumber(priceFixed),
        priceFixed,
        degraded: point.degraded === true,
        degradedReasons: stringArray(
          point.degraded_reasons ?? [],
          `fair price history.points[${index}].degraded_reasons`,
        ),
      };
    },
  );
  return { profile, symbol, modelId, resolutionMs, points };
}

export function decodeFairPriceMessage(data: string): AggdataFairPriceMessage {
  const raw = object(JSON.parse(data), "fair price message");
  if (raw.op === "reset" && raw.channel === "fairprice") {
    return {
      type: "reset",
      channel: "fairprice",
      profile: typeof raw.profile === "string" ? raw.profile : "",
      symbol: text(raw.symbol, "fair price reset.symbol"),
      reason: typeof raw.reason === "string" ? raw.reason : "unknown",
    };
  }
  if (typeof raw.ok === "boolean") {
    return {
      type: "ack",
      ok: raw.ok,
      error: typeof raw.error === "string" ? raw.error : null,
    };
  }
  if (raw.type !== "data" || raw.channel !== "fairprice") {
    throw new Error("unsupported fair price message");
  }
  const priceFixed = decimal(raw.price, 0, "fair price.price");
  const wallNS = bigint(raw.wall_ns, "fair price.wall_ns");
  return {
    type: "data",
    channel: "fairprice",
    profile: text(raw.profile, "fair price.profile"),
    symbol: text(raw.symbol, "fair price.symbol"),
    modelId: text(raw.model_id, "fair price.model_id"),
    timestamp: new Date(Number(wallNS / 1_000_000n)).toISOString(),
    sourceWallNS: wallNS,
    ringEpoch: bigint(raw.ring_epoch, "fair price.ring_epoch"),
    sequence: bigint(raw.seq, "fair price.seq"),
    price: fixedToSafeNumber(priceFixed),
    priceFixed,
    degraded: raw.degraded === true,
    degradedReasons: stringArray(raw.degraded_reasons ?? [], "fair price.degraded_reasons"),
  };
}

async function getJson(path: string, signal?: AbortSignal) {
  const token = process.env.NEXT_PUBLIC_AGGDATA_TOKEN;
  const response = await fetch(`${aggdataBaseUrl()}${path}`, {
    headers: {
      Accept: "application/json",
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    cache: "no-store",
    signal,
  });
  if (!response.ok) throw new Error(`aggdata request failed (${response.status})`);
  return response.json() as Promise<unknown>;
}

export async function fetchAggdataMarkets(signal?: AbortSignal) {
  return mapMarketsResponse(await getJson("/v1/markets", signal)).filter(
    (market) => market.hasOrderBook,
  );
}

export async function fetchAggdataSnapshot(
  market: AggdataMarket,
  signal?: AbortSignal,
) {
  return mapSnapshotResponse(
    await getJson(`/v1/markets/${encodeURIComponent(market.symbol)}/snapshot?depth=50`, signal),
    market,
  );
}

export async function fetchAggdataHistory(
  symbol: string,
  signal?: AbortSignal,
) {
  return mapHistoryResponse(
    await getJson(
      `/v1/markets/${encodeURIComponent(symbol)}/spread-history?range=24h&type=gated`,
      signal,
    ),
  );
}

export async function fetchFairPriceHistory(
  market: AggdataMarket,
  start: string,
  end: string,
  signal?: AbortSignal,
) {
  const query = new URLSearchParams({
    profile: market.profile,
    start,
    end,
    resolution: "auto",
  });
  return mapFairPriceHistoryResponse(
    await getJson(
      `/v1/markets/${encodeURIComponent(market.symbol)}/fair-price-history?${query}`,
      signal,
    ),
  );
}

export function resolveFairPriceMarket(
  markets: AggdataMarket[],
  asset: string,
  configuredProfile = process.env.NEXT_PUBLIC_AGGDATA_FAIRPRICE_PROFILE ?? "",
  preferredQuote = process.env.NEXT_PUBLIC_AGGDATA_FAIRPRICE_QUOTE ?? "USDT",
) {
  const candidates = markets.filter(
    (market) =>
      market.baseAsset.toUpperCase() === asset.toUpperCase() &&
      (!configuredProfile ||
        market.profile.toLowerCase() === configuredProfile.toLowerCase()),
  );
  const preferred = candidates.filter(
    (market) => market.quoteAsset.toUpperCase() === preferredQuote.toUpperCase(),
  );
  const selected = preferred.length > 0 ? preferred : candidates;
  return selected.length === 1 ? selected[0] : null;
}

function venueName(id: number) {
  const names: Record<number, string> = {
      1: "binance",
      2: "okx",
      3: "bybit",
      4: "gate",
      5: "bitget",
      6: "polymarket",
      7: "sse",
      8: "hyperliquid",
  };
  return names[id] ?? `venue-${id}`;
}

function readCompactLevel(
  view: DataView,
  offset: number,
  sideValue: BookSide,
  venueIDs: number[],
  priceScale: number,
  quantityScale: number,
) {
  if (offset + 21 > view.byteLength) throw new Error("truncated SQAB level");
  const price = view.getBigInt64(offset, true);
  const venueMask = view.getUint32(offset + 16, true);
  const contributorCount = view.getUint8(offset + 20);
  offset += 21;
  const levels: AggregatedBookLevel[] = [];
  let seenMask = 0;
  for (let index = 0; index < contributorCount; index += 1) {
    if (offset + 9 > view.byteLength) throw new Error("truncated SQAB contribution");
    const slot = view.getUint8(offset);
    if (slot >= venueIDs.length || (venueMask & (1 << slot)) === 0) {
      throw new Error("invalid SQAB venue slot");
    }
    seenMask |= 1 << slot;
    levels.push({
      side: sideValue,
      price: fixed(price, priceScale),
      quantity: fixed(view.getBigInt64(offset + 1, true), quantityScale),
      exchange: venueName(venueIDs[slot]),
    });
    offset += 9;
  }
  if (seenMask !== venueMask) throw new Error("SQAB venue mask mismatch");
  return { levels, offset };
}

/*
 * Browser compact wire v1, aligned with backend/internal/aggdata/browser.go:
 * little-endian "SQAB", 42-byte header, full snapshots (not deltas), frame-level
 * scales, symbol and venue IDs, then compact levels with int64 mantissas and
 * per-venue quantities. This boundary intentionally owns all wire assumptions.
 */
export function decodeAggdataFrame(data: ArrayBuffer): AggdataFrame {
  const view = new DataView(data);
  if (view.byteLength < 42) throw new Error("SQAB frame is shorter than header");
  if (view.getUint32(0, true) !== 0x42415153) throw new Error("invalid SQAB magic");
  if (view.getUint8(4) !== 1 || view.getUint8(5) !== 0) {
    throw new Error("unsupported SQAB wire version");
  }
  const kind = view.getUint8(6);
  if (kind !== 2) throw new Error(`unsupported SQAB channel kind ${kind}`);
  if (view.getUint8(7) !== 0 || view.getUint16(8, true) !== 42) {
    throw new Error("invalid SQAB header");
  }
  const bodyLength = view.getUint32(10, true);
  if (bodyLength !== view.byteLength - 42) throw new Error("SQAB body length mismatch");
  const symbolLength = view.getUint8(14);
  const priceScale = scale(view.getUint8(15), "frame.priceScale");
  const quantityScale = scale(view.getUint8(16), "frame.quantityScale");
  const venueCount = view.getUint8(17);
  const sequence = view.getBigUint64(18, true);
  const generation = view.getBigUint64(26, true);
  const timestampNS = view.getBigUint64(34, true);
  let offset = 42;
  if (offset + symbolLength + venueCount + 4 > view.byteLength) {
    throw new Error("truncated SQAB metadata");
  }
  const symbol = textDecoder.decode(new Uint8Array(data, offset, symbolLength));
  if (!symbol) throw new Error("SQAB symbol must not be empty");
  offset += symbolLength;
  const venueIDs = [...new Uint8Array(data, offset, venueCount)];
  offset += venueCount;
  const bidCount = view.getUint16(offset, true);
  const askCount = view.getUint16(offset + 2, true);
  offset += 4;
  const levels: AggregatedBookLevel[] = [];
  for (const [sideValue, count] of [
    ["bid", bidCount],
    ["ask", askCount],
  ] as const) {
    for (let index = 0; index < count; index += 1) {
      const decoded = readCompactLevel(
        view,
        offset,
        sideValue,
        venueIDs,
        priceScale,
        quantityScale,
      );
      levels.push(...decoded.levels);
      offset = decoded.offset;
    }
  }
  if (offset !== view.byteLength) throw new Error("SQAB frame has trailing bytes");
  return {
    kind: "snapshot",
    sequence,
    generation,
    timestampMs: timestampNS / 1_000_000n,
    priceScale,
    quantityScale,
    levels,
  };
}
