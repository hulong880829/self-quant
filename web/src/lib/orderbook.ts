export type BookSide = "ask" | "bid";

export interface FixedDecimal {
  mantissa: bigint;
  scale: number;
}

export interface AggregatedBookLevel {
  side: BookSide;
  price: FixedDecimal;
  quantity: FixedDecimal;
  exchange: string;
}

export interface SpreadPoint {
  timestamp: string;
  spreadBps: number | null;
}

export interface VenueContribution {
  exchange: string;
  quantity: FixedDecimal;
}

export interface AggregatedPriceLevel {
  side: BookSide;
  price: FixedDecimal;
  quantity: FixedDecimal;
  contributions: VenueContribution[];
}

const POWERS_OF_TEN = Array.from({ length: 19 }, (_, index) => 10n ** BigInt(index));

function powerOfTen(scale: number) {
  if (!Number.isInteger(scale) || scale < 0 || scale > 18) {
    throw new Error("decimal scale must be an integer from 0 through 18");
  }
  return POWERS_OF_TEN[scale];
}

export function fixed(mantissa: bigint | string | number, scale: number): FixedDecimal {
  return { mantissa: BigInt(mantissa), scale };
}

export function formatFixed(value: FixedDecimal, trim = false) {
  const negative = value.mantissa < 0n;
  const absolute = negative ? -value.mantissa : value.mantissa;
  if (value.scale === 0) return `${negative ? "-" : ""}${absolute}`;
  const digits = absolute.toString().padStart(value.scale + 1, "0");
  const integer = digits.slice(0, -value.scale);
  let fraction = digits.slice(-value.scale);
  if (trim) fraction = fraction.replace(/0+$/, "");
  return `${negative ? "-" : ""}${integer}${fraction ? `.${fraction}` : ""}`;
}

export function fixedToNumber(value: FixedDecimal) {
  return Number(value.mantissa) / 10 ** value.scale;
}

function mantissaAtScale(value: FixedDecimal, scale: number) {
  if (scale < value.scale) {
    const divisor = powerOfTen(value.scale - scale);
    if (value.mantissa % divisor !== 0n) {
      throw new Error("decimal cannot be represented at requested scale");
    }
    return value.mantissa / divisor;
  }
  return value.mantissa * powerOfTen(scale - value.scale);
}

function compareFixed(left: FixedDecimal, right: FixedDecimal) {
  const scale = Math.max(left.scale, right.scale);
  const leftValue = mantissaAtScale(left, scale);
  const rightValue = mantissaAtScale(right, scale);
  return leftValue < rightValue ? -1 : leftValue > rightValue ? 1 : 0;
}

function addFixed(left: FixedDecimal, right: FixedDecimal): FixedDecimal {
  const scale = Math.max(left.scale, right.scale);
  return fixed(mantissaAtScale(left, scale) + mantissaAtScale(right, scale), scale);
}

export function calculateSpreadBps(bestBid: FixedDecimal, bestAsk: FixedDecimal) {
  const bid = fixedToNumber(bestBid);
  const ask = fixedToNumber(bestAsk);
  if (bid <= 0 || ask <= 0) throw new Error("best prices must be positive");
  return ((ask - bid) / ((ask + bid) / 2)) * 10_000;
}

export function calculateSpreadCcdf(points: SpreadPoint[], thresholds?: number[]) {
  const values = points.flatMap((point) =>
    point.spreadBps === null || !Number.isFinite(point.spreadBps)
      ? []
      : [point.spreadBps],
  );
  if (values.length === 0) return [];
  const selected =
    thresholds ??
    Array.from({ length: 8 }, (_, index) => {
      const lower = Math.floor(Math.min(...values) * 10) / 10;
      const upper = Math.max(Math.ceil(Math.max(...values) * 10) / 10, lower + 0.1);
      return Number((lower + ((upper - lower) * index) / 7).toFixed(3));
    });
  return selected.toSorted((a, b) => a - b).map((thresholdBps) => ({
    thresholdBps,
    probability: values.filter((value) => value > thresholdBps).length / values.length,
  }));
}

export function spreadChartDomain(values: number[]) {
  const finite = values.filter(Number.isFinite);
  if (finite.length === 0) return null;
  const minimum = Math.min(...finite);
  const maximum = Math.max(...finite);
  const padding = Math.max((maximum - minimum) * 0.15, 0.02);
  return {
    minimum: Math.min(0, minimum - padding),
    maximum: Math.max(0, maximum + padding),
  };
}

export function getBestBid(levels: AggregatedBookLevel[]) {
  return levels
    .filter((level) => level.side === "bid")
    .reduce<AggregatedBookLevel | null>(
      (best, level) => (!best || compareFixed(level.price, best.price) > 0 ? level : best),
      null,
    );
}

export function getBestAsk(levels: AggregatedBookLevel[]) {
  return levels
    .filter((level) => level.side === "ask")
    .reduce<AggregatedBookLevel | null>(
      (best, level) => (!best || compareFixed(level.price, best.price) < 0 ? level : best),
      null,
    );
}

function floorDiv(value: bigint, divisor: bigint) {
  const quotient = value / divisor;
  return value < 0n && value % divisor !== 0n ? quotient - 1n : quotient;
}

export function aggregateBookLevels(
  levels: AggregatedBookLevel[],
  side: BookSide,
  increment: FixedDecimal,
) {
  if (increment.mantissa <= 0n) throw new Error("price increment must be positive");
  const priceScale = Math.max(increment.scale, ...levels.map((level) => level.price.scale));
  const incrementMantissa = mantissaAtScale(increment, priceScale);
  const buckets = new Map<
    bigint,
    { quantity: FixedDecimal; contributions: Map<string, FixedDecimal> }
  >();
  for (const level of levels) {
    if (level.side !== side) continue;
    const price = mantissaAtScale(level.price, priceScale);
    const floor = floorDiv(price, incrementMantissa);
    const bucketIndex =
      side === "ask" && price % incrementMantissa !== 0n ? floor + 1n : floor;
    const current = buckets.get(bucketIndex) ?? {
      quantity: fixed(0n, level.quantity.scale),
      contributions: new Map(),
    };
    current.quantity = addFixed(current.quantity, level.quantity);
    current.contributions.set(
      level.exchange,
      addFixed(
        current.contributions.get(level.exchange) ?? fixed(0n, level.quantity.scale),
        level.quantity,
      ),
    );
    buckets.set(bucketIndex, current);
  }
  return [...buckets.entries()]
    .map<AggregatedPriceLevel>(([index, bucket]) => ({
      side,
      price: fixed(index * incrementMantissa, priceScale),
      quantity: bucket.quantity,
      contributions: [...bucket.contributions].map(([exchange, quantity]) => ({
        exchange,
        quantity,
      })),
    }))
    .toSorted((left, right) => {
      const order = compareFixed(left.price, right.price);
      return side === "ask" ? order : -order;
    });
}

export function orderBookDisplayLevels(
  levels: AggregatedPriceLevel[],
  side: BookSide,
) {
  return side === "ask" ? levels.toReversed() : levels;
}

function gcd(left: bigint, right: bigint): bigint {
  let a = left < 0n ? -left : left;
  let b = right < 0n ? -right : right;
  while (b !== 0n) [a, b] = [b, a % b];
  return a;
}

export function inferEffectiveTick(levels: AggregatedBookLevel[]): FixedDecimal | null {
  const prices = levels.map((level) => level.price);
  if (prices.length < 2) return null;
  const scale = Math.max(...prices.map((price) => price.scale));
  const sorted = [...new Set(prices.map((price) => mantissaAtScale(price, scale).toString()))]
    .map(BigInt)
    .toSorted((a, b) => (a < b ? -1 : a > b ? 1 : 0));
  let tick = 0n;
  for (let index = 1; index < sorted.length; index += 1) {
    const difference = sorted[index] - sorted[index - 1];
    if (difference > 0n) tick = tick === 0n ? difference : gcd(tick, difference);
  }
  return tick > 0n ? fixed(tick, scale) : null;
}

export function generate125Increments(tick: FixedDecimal, count = 8) {
  const result: FixedDecimal[] = [];
  let coefficient = 1n;
  let decade = 1n;
  while (result.length < count) {
    result.push(fixed(tick.mantissa * coefficient * decade, tick.scale));
    coefficient = coefficient === 1n ? 2n : coefficient === 2n ? 5n : 1n;
    if (coefficient === 1n) decade *= 10n;
  }
  return result;
}

export function chooseVisibleIncrement(
  levels: AggregatedBookLevel[],
  increments: FixedDecimal[],
  minRows = 12,
  maxRows = 30,
) {
  for (const increment of increments) {
    const asks = aggregateBookLevels(levels, "ask", increment).length;
    const bids = aggregateBookLevels(levels, "bid", increment).length;
    if (asks >= minRows && asks <= maxRows && bids >= minRows && bids <= maxRows) {
      return increment;
    }
  }
  return increments.reduce((best, increment) => {
    const rows =
      aggregateBookLevels(levels, "ask", increment).length +
      aggregateBookLevels(levels, "bid", increment).length;
    const target = (minRows + maxRows) / 2;
    const bestRows =
      aggregateBookLevels(levels, "ask", best).length +
      aggregateBookLevels(levels, "bid", best).length;
    return Math.abs(rows / 2 - target) < Math.abs(bestRows / 2 - target)
      ? increment
      : best;
  }, increments[0]);
}

export function fillHistoryTimeline(
  points: SpreadPoint[],
  startMs: number,
  endMs: number,
  resolutionMs: number,
): SpreadPoint[] {
  if (
    !Number.isFinite(startMs) ||
    !Number.isFinite(endMs) ||
    !Number.isFinite(resolutionMs) ||
    resolutionMs <= 0 ||
    endMs <= startMs
  ) {
    throw new Error("history timeline bounds must be finite and increasing");
  }
  const byTimestamp = new Map(
    points.map((point) => [
      Date.parse(point.timestamp),
      point.spreadBps,
    ]),
  );
  const bucketCount = Math.ceil((endMs - startMs) / resolutionMs);
  return Array.from({ length: bucketCount }, (_, index) => {
    const timestamp = startMs + index * resolutionMs;
    return {
      timestamp: new Date(timestamp).toISOString(),
      spreadBps: byTimestamp.get(timestamp) ?? null,
    };
  });
}
