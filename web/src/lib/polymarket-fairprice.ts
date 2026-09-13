import type { AggdataFairPricePoint } from "@/lib/api/aggdata";
import {
  fairPriceChartTimeMs,
  filterFairPricePointsToWindow,
} from "@/lib/polymarket-chart";
import type { PolymarketFairPricePoint } from "@/types/polymarket";

export function toPolymarketFairPricePoint(
  point: AggdataFairPricePoint,
): PolymarketFairPricePoint {
  return {
    timestamp: point.timestamp,
    sourceWallNS: point.sourceWallNS.toString(),
    ringEpoch: point.ringEpoch.toString(),
    sequence: point.sequence.toString(),
    modelId: point.modelId,
    price: point.price,
    degraded: point.degraded,
    degradedReasons: [...point.degradedReasons],
  };
}

export function sameFairPriceSequence(
  left: PolymarketFairPricePoint | null,
  right: PolymarketFairPricePoint,
) {
  return left?.ringEpoch === right.ringEpoch && left.sequence === right.sequence;
}

export function mergeFairPricePoints(
  current: PolymarketFairPricePoint[],
  incoming: PolymarketFairPricePoint[],
  limit = 2000,
  window?: { start: string; end: string; nowMs?: number },
) {
  const modelId = incoming.at(-1)?.modelId ?? current.at(-1)?.modelId;
  const buckets = new Map<number, PolymarketFairPricePoint>();
  const add = (point: PolymarketFairPricePoint) => {
    if (modelId && point.modelId !== modelId) return;
    const timestampMs = fairPriceChartTimeMs(point);
    if (!Number.isFinite(timestampMs)) return;
    const bucket = Math.floor(timestampMs / 1000);
    const existing = buckets.get(bucket);
    if (
      !existing ||
      BigInt(point.sourceWallNS) >= BigInt(existing.sourceWallNS)
    ) {
      buckets.set(bucket, point);
    }
  };
  current.forEach(add);
  incoming.forEach(add);
  const merged = [...buckets.values()].sort(
    (left, right) => fairPriceChartTimeMs(left) - fairPriceChartTimeMs(right),
  );
  const filtered = window
    ? filterFairPricePointsToWindow(
        merged,
        window.start,
        window.end,
        window.nowMs,
      )
    : merged;
  return filtered.slice(-limit);
}

export function isFairPriceStale(
  point: PolymarketFairPricePoint | null,
  nowMs = Date.now(),
  staleAfterMs = 15_000,
  futureToleranceMs = 5_000,
) {
  if (point == null) return true;
  const sourceMs = fairPriceChartTimeMs(point);
  return !Number.isFinite(sourceMs) ||
    sourceMs > nowMs + futureToleranceMs ||
    nowMs - sourceMs > staleAfterMs;
}
