import type {
  PolymarketFairPricePoint,
  PolymarketPricePoint,
} from "@/types/polymarket";

export function fairPriceChartTimeMs(point: PolymarketFairPricePoint): number {
  try {
    const wallMs = Number(BigInt(point.sourceWallNS) / 1_000_000n);
    if (Number.isFinite(wallMs) && wallMs > 0) return wallMs;
  } catch {
    // Fall back to the presentation timestamp for legacy history rows.
  }
  return new Date(point.timestamp).getTime();
}

export function computeChartXDomain(
  windowStart: string | null,
  windowEnd: string | null,
  nowMs = Date.now(),
): { startMs: number; endMs: number; spanMs: number } | null {
  if (!windowStart || !windowEnd) return null;
  const startMs = new Date(windowStart).getTime();
  const configuredEnd = new Date(windowEnd).getTime();
  if (!Number.isFinite(startMs) || !Number.isFinite(configuredEnd) ||
      configuredEnd <= startMs) {
    return null;
  }
  const endMs = Math.max(startMs + 1, Math.min(configuredEnd, nowMs));
  return { startMs, endMs, spanMs: endMs - startMs };
}

export function filterPricePointsToWindow(
  points: PolymarketPricePoint[],
  windowStart: string | null,
  windowEnd: string | null,
  nowMs = Date.now(),
) {
  const domain = computeChartXDomain(windowStart, windowEnd, nowMs);
  if (!domain) return [];
  return points.filter((point) => {
    const timestamp = new Date(point.timestamp).getTime();
    return timestamp >= domain.startMs && timestamp <= domain.endMs;
  });
}

export function filterFairPricePointsToWindow(
  points: PolymarketFairPricePoint[],
  windowStart: string | null,
  windowEnd: string | null,
  nowMs = Date.now(),
) {
  const domain = computeChartXDomain(windowStart, windowEnd, nowMs);
  if (!domain) return [];
  return points.filter((point) => {
    const timestamp = fairPriceChartTimeMs(point);
    return timestamp >= domain.startMs && timestamp <= domain.endMs;
  });
}

export function computeYDomain(
  values: number[],
  options: { minSpan?: number } = {},
): { yMin: number; yMax: number } {
  if (values.length === 0) {
    return { yMin: 0, yMax: 1 };
  }
  const min = Math.min(...values);
  const max = Math.max(...values);
  const span = Math.max(max - min, 0);
  const effectiveSpan = Math.max(span, options.minSpan ?? 0);
  const padding = effectiveSpan * 0.12;
  const midpoint = (min + max) / 2;
  return {
    yMin: midpoint - effectiveSpan / 2 - padding,
    yMax: midpoint + effectiveSpan / 2 + padding,
  };
}

export function computeChartMinSpan(
  values: number[],
  openPrice?: number | null,
): number {
  const finiteValues = values.filter(Number.isFinite);
  if (finiteValues.length === 0) {
    return 1e-6;
  }
  const min = Math.min(...finiteValues);
  const max = Math.max(...finiteValues);
  const actualSpan = Math.max(max - min, 0);
  const sorted = [...finiteValues].sort((left, right) => left - right);
  const middle = Math.floor(sorted.length / 2);
  const median =
    sorted.length % 2 === 0
      ? (sorted[middle - 1] + sorted[middle]) / 2
      : sorted[middle];
  const referencePrice =
    openPrice != null && Number.isFinite(openPrice) && openPrice > 0
      ? openPrice
      : median;
  const relativeFloor = referencePrice > 0 ? referencePrice * 0.0002 : 0;
  return Math.max(actualSpan, relativeFloor, 1e-6);
}

export function formatYTick(value: number, span: number): string {
  let fractionDigits = 2;
  if (span < 0.0001) fractionDigits = 8;
  else if (span < 0.01) fractionDigits = 6;
  else if (span < 1) fractionDigits = 4;
  else if (span < 5) fractionDigits = 3;
  return value.toLocaleString("en-US", {
    minimumFractionDigits: fractionDigits,
    maximumFractionDigits: fractionDigits,
  });
}
