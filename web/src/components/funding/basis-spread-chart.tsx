"use client";

import * as React from "react";
import { LoaderCircle, RefreshCw } from "lucide-react";

import type {
  BasisSpreadHistory,
  BasisSpreadRange,
} from "@/lib/api/spread";
import { fetchBasisSpreadHistory } from "@/lib/api/spread";
import { cn } from "@/lib/utils";

const periods: BasisSpreadRange[] = ["1h", "4h", "8h", "24h", "7d"];
const REFRESH_MS = 30_000;
const CACHE_TTL_MS = 30_000;
const DETAIL_HEIGHT_PX = 320;

const beijingTimeFormatter = new Intl.DateTimeFormat("zh-CN", {
  timeZone: "Asia/Shanghai",
  hour: "2-digit",
  minute: "2-digit",
  hour12: false,
});

type CacheEntry = {
  history: BasisSpreadHistory;
  etag: string | null;
  loadedAt: number;
};

const historyCache = new Map<string, CacheEntry>();

export function resetBasisSpreadCacheForTests() {
  historyCache.clear();
}

function cacheKey(
  venue: string,
  baseAsset: string,
  quoteAsset: string,
  range: BasisSpreadRange,
  compareVenue?: string,
) {
  return `${venue.toLowerCase()}|${(compareVenue ?? "").toLowerCase()}|${baseAsset.toUpperCase()}|${quoteAsset.toUpperCase()}|${range}`;
}

function formatBps(value: number) {
  const sign = value > 0 ? "+" : "";
  return `${sign}${value.toFixed(2)} bps`;
}

function formatCoverage(value: number) {
  return `${(value * 100).toFixed(1)}%`;
}

function formatBeijingTime(timestampMs: number) {
  return beijingTimeFormatter.format(new Date(timestampMs));
}

export function spreadYDomain(values: number[]): { min: number; max: number } {
  if (values.length === 0) {
    return { min: -0.5, max: 0.5 };
  }
  const yMin = Math.min(...values);
  const yMax = Math.max(...values);
  const yPad = Math.max((yMax - yMin) * 0.12, 0.5);
  return { min: yMin - yPad, max: yMax + yPad };
}

export function BasisSpreadPanel({
  venue,
  baseAsset,
  quoteAsset,
  compareVenue,
}: {
  venue: string;
  baseAsset: string;
  quoteAsset: string;
  compareVenue?: string;
}) {
  const [range, setRange] = React.useState<BasisSpreadRange>("24h");
  const [history, setHistory] = React.useState<BasisSpreadHistory | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const etagRef = React.useRef<string | null>(null);
  const retryControllerRef = React.useRef<AbortController | null>(null);
  const crossVenue = Boolean(compareVenue);
  const title = crossVenue ? "跨所 Best Ask 价差" : "期现 Best Ask 价差";
  const formula = crossVenue
    ? `${venue} Ask / ${compareVenue} Ask - 1 · ${baseAsset}/${quoteAsset}`
    : `Perpetual Ask / Spot Ask - 1 · ${baseAsset}/${quoteAsset}`;

  const load = React.useCallback(
    async (signal: AbortSignal) => {
      await Promise.resolve();
      const key = cacheKey(venue, baseAsset, quoteAsset, range, compareVenue);
      const existing = historyCache.get(key);
      if (existing && Date.now() - existing.loadedAt < CACHE_TTL_MS) {
        etagRef.current = existing.etag;
        setHistory(existing.history);
        setError(null);
        return;
      }
      try {
        const result = await fetchBasisSpreadHistory(
          venue,
          baseAsset,
          quoteAsset,
          range,
          etagRef.current,
          signal,
          compareVenue,
        );
        if (signal.aborted) return;
        if (result.status === "unchanged") {
          etagRef.current = result.etag;
          if (existing) {
            historyCache.set(key, { ...existing, loadedAt: Date.now(), etag: result.etag });
          }
          return;
        }
        etagRef.current = result.etag;
        historyCache.set(key, {
          history: result.history,
          etag: result.etag,
          loadedAt: Date.now(),
        });
        setHistory(result.history);
        setError(null);
      } catch (reason) {
        if (signal.aborted) return;
        setError(reason instanceof Error ? reason.message : "期现价差加载失败");
      }
    },
    [venue, baseAsset, quoteAsset, range, compareVenue],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    etagRef.current =
      historyCache.get(cacheKey(venue, baseAsset, quoteAsset, range, compareVenue))?.etag ?? null;
    const immediate = window.setTimeout(() => {
      void load(controller.signal);
    }, 0);
    const timer = window.setInterval(() => {
      void load(controller.signal);
    }, REFRESH_MS);
    return () => {
      controller.abort();
      retryControllerRef.current?.abort();
      window.clearTimeout(immediate);
      window.clearInterval(timer);
    };
  }, [load, venue, baseAsset, quoteAsset, range, compareVenue]);

  return (
    <section
      className="space-y-3"
      aria-label={title}
      style={{ minHeight: DETAIL_HEIGHT_PX - 48 }}
    >
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h3 className="text-sm font-medium">{title}</h3>
          <p className="mt-0.5 text-[11px] text-muted-foreground">{formula}</p>
        </div>
        <div className="flex items-center gap-1 rounded-md border bg-muted/40 p-0.5">
          {periods.map((item) => (
            <button
              key={item}
              type="button"
              onClick={() => setRange(item)}
              className={cn(
                "h-6 rounded px-2 font-mono text-[11px]",
                range === item
                  ? "bg-background text-foreground shadow-sm"
                  : "text-muted-foreground hover:text-foreground",
              )}
            >
              {item}
            </button>
          ))}
        </div>
      </div>

      {history?.availability === "available" && (
        <div className="grid grid-cols-4 gap-2 text-[11px]">
          <Metric label="当前" value={formatBps(history.summary.currentBps)} />
          <Metric label="周期均值" value={formatBps(history.summary.avgBps)} />
          <Metric
            label="最小 / 最大"
            value={`${formatBps(history.summary.minBps)} / ${formatBps(history.summary.maxBps)}`}
          />
          <Metric label="覆盖率" value={formatCoverage(history.summary.coverage)} />
        </div>
      )}

      {!history && !error ? (
        <div className="flex h-44 items-center justify-center text-xs text-muted-foreground">
          <LoaderCircle className="mr-2 size-3.5 animate-spin" />
          {crossVenue ? "加载跨所价差" : "加载期现价差"}
        </div>
      ) : error && !history ? (
        <div className="flex h-44 flex-col items-center justify-center gap-2 text-xs text-amber-600 dark:text-amber-300">
          <span>{error}</span>
          <button
            type="button"
            className="inline-flex items-center gap-1 text-foreground"
            onClick={() => {
              retryControllerRef.current?.abort();
              retryControllerRef.current = new AbortController();
              void load(retryControllerRef.current.signal);
            }}
          >
            <RefreshCw className="size-3" />
            重试
          </button>
        </div>
      ) : history?.availability === "unavailable" ? (
        <div className="flex h-44 items-center justify-center text-xs text-muted-foreground">
          {crossVenue ? "该组合暂无对应永续 BBO 数据" : "该交易所暂无对应 Spot BBO 数据"}
        </div>
      ) : history && history.points.length === 0 ? (
        <div className="flex h-44 items-center justify-center text-xs text-muted-foreground">
          当前周期暂无配对价差
        </div>
      ) : history ? (
        <SpreadLineChart history={history} label={title} />
      ) : null}
    </section>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-md border bg-muted/30 px-2 py-1.5">
      <div className="text-muted-foreground">{label}</div>
      <div className="mt-0.5 font-mono text-xs">{value}</div>
    </div>
  );
}

function SpreadLineChart({
  history,
  label,
}: {
  history: BasisSpreadHistory;
  label: string;
}) {
  const width = 920;
  const height = 188;
  const margin = { top: 12, right: 16, bottom: 28, left: 48 };
  const values = history.points.map((point) => point.spreadBps);
  const timestamps = history.points.map((point) => Date.parse(point.ts));
  const xStart = Math.min(...timestamps);
  const xEnd = Math.max(...timestamps);
  const { min: domainMin, max: domainMax } = spreadYDomain(values);
  const x = (timestampMs: number) =>
    margin.left +
    ((timestampMs - xStart) / Math.max(xEnd - xStart, 1)) *
      (width - margin.left - margin.right);
  const y = (value: number) =>
    margin.top +
    ((domainMax - value) / Math.max(domainMax - domainMin, 0.0001)) *
      (height - margin.top - margin.bottom);
  const gapMs = history.resolutionSeconds * 2.5 * 1000;
  const segments: string[] = [];
  let path = "";
  history.points.forEach((point, index) => {
    const timestamp = Date.parse(point.ts);
    const previous = index === 0 ? timestamp : Date.parse(history.points[index - 1]!.ts);
    if (index > 0 && timestamp - previous > gapMs && path) {
      segments.push(path);
      path = "";
    }
    path += `${path ? " L" : "M"} ${x(timestamp).toFixed(2)} ${y(point.spreadBps).toFixed(2)}`;
  });
  if (path) segments.push(path);
  const plot = {
    x: margin.left,
    y: margin.top,
    width: width - margin.left - margin.right,
    height: height - margin.top - margin.bottom,
  };
  const clipId = "basis-spread-plot";
  return (
    <svg
      viewBox={`0 0 ${width} ${height}`}
      className="h-44 w-full"
      overflow="hidden"
      role="img"
      aria-label={`${label}走势，北京时间，缺失时段断线显示`}
    >
      <defs>
        <clipPath id={clipId}>
          <rect x={plot.x} y={plot.y} width={plot.width} height={plot.height} />
        </clipPath>
      </defs>
      {[0, 0.25, 0.5, 0.75, 1].map((ratio) => {
        const value = domainMax - ratio * (domainMax - domainMin);
        return (
          <g key={ratio}>
            <line
              x1={margin.left}
              x2={width - margin.right}
              y1={y(value)}
              y2={y(value)}
              stroke="var(--border)"
              strokeDasharray="4 5"
            />
            <text
              x={margin.left - 8}
              y={y(value) + 3}
              textAnchor="end"
              fill="var(--muted-foreground)"
              fontSize="10"
            >
              {value.toFixed(1)}
            </text>
          </g>
        );
      })}
      {domainMin <= 0 && domainMax >= 0 && (
        <line
          x1={margin.left}
          x2={width - margin.right}
          y1={y(0)}
          y2={y(0)}
          stroke="var(--muted-foreground)"
          strokeOpacity="0.45"
        />
      )}
      <g clipPath={`url(#${clipId})`}>
        {segments.map((segment, index) => (
          <path
            key={index}
            d={segment}
            fill="none"
            stroke="var(--primary)"
            strokeWidth="2"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        ))}
      </g>
      {[0, 0.5, 1].map((ratio) => {
        const timestamp = xStart + ratio * (xEnd - xStart);
        return (
          <text
            key={ratio}
            x={x(timestamp)}
            y={height - 8}
            textAnchor={ratio === 0 ? "start" : ratio === 1 ? "end" : "middle"}
            fill="var(--muted-foreground)"
            fontSize="10"
          >
            {formatBeijingTime(timestamp)}
          </text>
        );
      })}
    </svg>
  );
}
