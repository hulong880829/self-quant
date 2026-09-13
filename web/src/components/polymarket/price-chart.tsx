"use client";

import * as React from "react";

import type {
  PolymarketFairPricePoint,
  PolymarketPricePoint,
} from "@/types/polymarket";
import {
  computeChartXDomain,
  computeChartMinSpan,
  computeYDomain,
  fairPriceChartTimeMs,
  filterFairPricePointsToWindow,
  filterPricePointsToWindow,
  formatYTick,
} from "@/lib/polymarket-chart";

function formatPrice(value: number) {
  return value.toLocaleString("en-US", {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  });
}

function chartPropsEqual(
  left: {
    points: PolymarketPricePoint[];
    fairPricePoints: PolymarketFairPricePoint[];
    openPrice: number | null;
    windowStart: string | null;
    windowEnd: string | null;
  },
  right: {
    points: PolymarketPricePoint[];
    fairPricePoints: PolymarketFairPricePoint[];
    openPrice: number | null;
    windowStart: string | null;
    windowEnd: string | null;
  },
) {
  if (left.openPrice !== right.openPrice) return false;
  if (left.windowStart !== right.windowStart || left.windowEnd !== right.windowEnd) {
    return false;
  }
  if (left.points.length !== right.points.length) return false;
  if (left.fairPricePoints.length !== right.fairPricePoints.length) return false;
  const lastLeft = left.points[left.points.length - 1];
  const lastRight = right.points[right.points.length - 1];
  const fairLeft = left.fairPricePoints[left.fairPricePoints.length - 1];
  const fairRight = right.fairPricePoints[right.fairPricePoints.length - 1];
  return (
    lastLeft?.timestamp === lastRight?.timestamp &&
    lastLeft?.chainlinkPrice === lastRight?.chainlinkPrice &&
    fairLeft?.sourceWallNS === fairRight?.sourceWallNS &&
    fairLeft?.price === fairRight?.price
  );
}

function PolymarketPriceChartInner({
  points,
  fairPricePoints,
  openPrice,
  windowStart,
  windowEnd,
}: {
  points: PolymarketPricePoint[];
  fairPricePoints: PolymarketFairPricePoint[];
  openPrice: number | null;
  windowStart: string | null;
  windowEnd: string | null;
}) {
  const deferredPoints = React.useDeferredValue(points);
  const deferredFairPricePoints = React.useDeferredValue(fairPricePoints);
  const nowMs = windowEnd ? Date.parse(windowEnd) : 0;
  const xDomain = computeChartXDomain(windowStart, windowEnd, nowMs);
  const validPoints = React.useMemo(
    () =>
      filterPricePointsToWindow(
        deferredPoints,
        windowStart,
        windowEnd,
        nowMs,
      ).filter(
          (point): point is PolymarketPricePoint & { chainlinkPrice: number } =>
            point.chainlinkPrice != null,
        ),
    [deferredPoints, nowMs, windowEnd, windowStart],
  );
  const validFairPricePoints = React.useMemo(
    () =>
      filterFairPricePointsToWindow(
        deferredFairPricePoints,
        windowStart,
        windowEnd,
        nowMs,
      ).filter((point) => Number.isFinite(point.price)),
    [deferredFairPricePoints, nowMs, windowEnd, windowStart],
  );

  const hasOpen = openPrice != null;
  const width = 760;
  const height = 320;
  const margin = { top: 18, right: 22, bottom: 38, left: 58 };
  const chartWidth = width - margin.left - margin.right;
  const chartHeight = height - margin.top - margin.bottom;

  const chainlinkValues = React.useMemo(
    () => validPoints.map((point) => point.chainlinkPrice),
    [validPoints],
  );
  const fairPriceValues = React.useMemo(
    () => validFairPricePoints.map((point) => point.price),
    [validFairPricePoints],
  );
  const chartGeometry = React.useMemo(() => {
    if (validPoints.length === 0 && validFairPricePoints.length === 0) {
      return null;
    }
    const domainValues = [...chainlinkValues, ...fairPriceValues];
    if (hasOpen) domainValues.push(openPrice as number);
    const minSpan = computeChartMinSpan(domainValues, openPrice);
    const domain = computeYDomain(domainValues, { minSpan });
    if (!xDomain) return null;
    const { startMs, spanMs } = xDomain;
    const x = (timestampMs: number) => {
      const ratio = (timestampMs - startMs) / spanMs;
      return margin.left + ratio * chartWidth;
    };
    const y = (value: number) =>
      margin.top + ((domain.yMax - value) / (domain.yMax - domain.yMin)) * chartHeight;
    const buildPath = <T,>(
      pathPoints: T[],
      value: (point: T) => number,
      timestamp: (point: T) => number,
      gapMs?: number,
    ) =>
      pathPoints
        .map((point, index) => {
          const previous = pathPoints[index - 1];
          const hasGap =
            gapMs != null &&
            previous != null &&
            timestamp(point) - timestamp(previous) > gapMs;
          return `${index === 0 || hasGap ? "M" : "L"} ${x(timestamp(point)).toFixed(2)} ${y(value(point)).toFixed(2)}`;
        })
        .join(" ");
    return {
      yMin: domain.yMin,
      yMax: domain.yMax,
      ySpan: domain.yMax - domain.yMin,
      chainlinkPathD: buildPath(
        validPoints,
        (point) => point.chainlinkPrice,
        (point) => new Date(point.timestamp).getTime(),
        10_000,
      ),
      fairPricePathD: buildPath(
        validFairPricePoints,
        (point) => point.price,
        fairPriceChartTimeMs,
      ),
      startMs,
      spanMs,
    };
  }, [
    chainlinkValues,
    chartHeight,
    chartWidth,
    fairPriceValues,
    hasOpen,
    margin.left,
    margin.top,
    openPrice,
    validFairPricePoints,
    validPoints,
    xDomain,
  ]);

  if (chartGeometry == null) {
    return (
      <div className="flex h-72 items-center justify-center rounded-lg border border-dashed text-sm text-muted-foreground">
        等待 Chainlink / Fair Price 真实价格…
      </div>
    );
  }

  const {
    yMin,
    yMax,
    ySpan,
    chainlinkPathD,
    fairPricePathD,
    startMs,
    spanMs,
  } = chartGeometry;
  const x = (timestampMs: number) => {
    const ratio = (timestampMs - startMs) / spanMs;
    return margin.left + ratio * chartWidth;
  };
  const y = (value: number) =>
    margin.top + ((yMax - value) / (yMax - yMin)) * chartHeight;

  const openY = hasOpen ? y(openPrice) : 0;
  const yTicks = Array.from({ length: 5 }, (_, index) => {
    const ratio = index / 4;
    return yMax - ratio * (yMax - yMin);
  });
  const xTickTimes = Array.from({ length: 5 }, (_, index) => {
    const ratio = index / 4;
    return new Date(startMs + ratio * spanMs).toISOString();
  });

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-4 text-xs text-muted-foreground">
        <div className={cnLegendItem(!hasOpen)}>
          <span
            className="inline-block h-0.5 w-5 rounded-full"
            style={{
              background: hasOpen ? "var(--chart-1)" : "var(--muted-foreground)",
              opacity: hasOpen ? 1 : 0.45,
            }}
          />
          Open{hasOpen ? "" : "（待同步）"}
        </div>
        <div className="flex items-center gap-2">
          <span
            className="inline-block h-0.5 w-5 rounded-full"
            style={{ background: "var(--chart-2)" }}
          />
          Chainlink
        </div>
        <div className={cnLegendItem(validFairPricePoints.length === 0)}>
          <span
            className="inline-block h-0.5 w-5 rounded-full"
            style={{ background: "var(--chart-3)" }}
          />
          Fair Price{validFairPricePoints.length === 0 ? "（待同步）" : ""}
        </div>
      </div>
      <svg
        viewBox={`0 0 ${width} ${height}`}
        className="h-auto w-full overflow-visible"
        role="img"
        aria-label={
          hasOpen
            ? "Open、Chainlink 与 Fair Price 价格图"
            : "Chainlink 与 Fair Price 实时价格图"
        }
      >
        {yTicks.map((tick) => (
          <g key={tick}>
            <line
              x1={margin.left}
              x2={width - margin.right}
              y1={y(tick)}
              y2={y(tick)}
              stroke="var(--border)"
              strokeDasharray="4 5"
            />
            <text
              x={margin.left - 10}
              y={y(tick) + 4}
              textAnchor="end"
              fill="var(--muted-foreground)"
              fontSize="10"
              fontFamily="var(--font-geist-mono)"
            >
              ${formatYTick(tick, ySpan)}
            </text>
          </g>
        ))}
        {xTickTimes.map((timestamp, index) => (
          <text
            key={timestamp}
            x={x(new Date(timestamp).getTime())}
            y={height - 12}
            textAnchor={
              index === 0 ? "start" : index === xTickTimes.length - 1 ? "end" : "middle"
            }
            fill="var(--muted-foreground)"
            fontSize="10"
            fontFamily="var(--font-geist-mono)"
          >
            {new Date(timestamp).toLocaleTimeString("zh-CN", {
              hour12: false,
            })}
          </text>
        ))}
        {hasOpen ? (
          <>
            <line
              x1={margin.left}
              x2={width - margin.right}
              y1={openY}
              y2={openY}
              stroke="var(--chart-1)"
              strokeDasharray="6 5"
              strokeWidth="1.6"
            />
            <rect
              x={width - margin.right - 58}
              y={openY - 11}
              width="52"
              height="18"
              rx="9"
              fill="var(--muted)"
            />
            <text
              x={width - margin.right - 32}
              y={openY + 3}
              textAnchor="middle"
              fill="var(--muted-foreground)"
              fontSize="9"
              fontFamily="var(--font-geist-mono)"
            >
              Open
            </text>
          </>
        ) : null}
        {chainlinkPathD ? (
          <path
            d={chainlinkPathD}
            fill="none"
            stroke="var(--chart-2)"
            strokeWidth="2.4"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        ) : null}
        {fairPricePathD ? (
          <path
            d={fairPricePathD}
            fill="none"
            stroke="var(--chart-3)"
            strokeWidth="2.1"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        ) : null}
        {validPoints.length > 0 ? (
          <circle
            cx={x(new Date(validPoints[validPoints.length - 1].timestamp).getTime())}
            cy={y(validPoints[validPoints.length - 1].chainlinkPrice)}
            r="4"
            fill="var(--chart-2)"
            stroke="var(--card)"
            strokeWidth="2"
          >
            <title>
              {new Date(
                validPoints[validPoints.length - 1].timestamp,
              ).toLocaleTimeString("zh-CN")}{" "}
              · Chainlink $
              {formatPrice(validPoints[validPoints.length - 1].chainlinkPrice)}
            </title>
          </circle>
        ) : null}
        {validFairPricePoints.length > 0 ? (
          <circle
            cx={x(fairPriceChartTimeMs(validFairPricePoints[validFairPricePoints.length - 1]))}
            cy={y(validFairPricePoints[validFairPricePoints.length - 1].price)}
            r="4"
            fill="var(--chart-3)"
            stroke="var(--card)"
            strokeWidth="2"
          >
            <title>
              {new Date(
                validFairPricePoints[validFairPricePoints.length - 1].timestamp,
              ).toLocaleTimeString("zh-CN")}{" "}
              · Fair Price $
              {formatPrice(validFairPricePoints[validFairPricePoints.length - 1].price)}
            </title>
          </circle>
        ) : null}
      </svg>
    </div>
  );
}

export const PolymarketPriceChart = React.memo(
  PolymarketPriceChartInner,
  chartPropsEqual,
);

function cnLegendItem(muted: boolean) {
  return muted
    ? "flex items-center gap-2 opacity-60"
    : "flex items-center gap-2";
}
