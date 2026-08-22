import type { AggdataFairPricePoint } from "@/lib/api/aggdata";
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
) {
  const modelId = incoming.at(-1)?.modelId ?? current.at(-1)?.modelId;
  const buckets = new Map<number, PolymarketFairPricePoint>();
  const add = (point: PolymarketFairPricePoint) => {
    if (modelId && point.modelId !== modelId) return;
    const timestampMs = new Date(point.timestamp).getTime();
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
  return [...buckets.values()]
    .sort(
      (left, right) =>
        new Date(left.timestamp).getTime() - new Date(right.timestamp).getTime(),
    )
    .slice(-limit);
}
