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
