"use client";

import * as React from "react";
import {
  Activity,
  AlertCircle,
  BarChart3,
  ChevronDown,
  LoaderCircle,
  RefreshCw,
  Wifi,
  WifiOff,
} from "lucide-react";

import {
  OrderbookProvider,
  useOrderbook,
} from "@/components/orderbook/orderbook-provider";
import {
  aggdataMarketKey,
  aggdataProductFromProfile,
} from "@/lib/api/aggdata";
import {
  aggregateBookLevels,
  calculateSpreadBps,
  calculateSpreadCcdf,
  fixedToNumber,
  formatFixed,
  generate125Increments,
  getBestAsk,
  getBestBid,
  inferEffectiveTick,
  orderBookDisplayLevels,
  spreadChartDomain,
  type AggregatedPriceLevel,
  type FixedDecimal,
  type SpreadPoint,
} from "@/lib/orderbook";
import { cn } from "@/lib/utils";

function BookRow({
  level,
  maxQuantity,
}: {
  level: AggregatedPriceLevel;
  maxQuantity: number;
}) {
  const ask = level.side === "ask";
  const quantity = fixedToNumber(level.quantity);
  const contributions = level.contributions
    .map((item) => `${item.exchange} ${formatFixed(item.quantity, true)}`)
    .join(", ");
  return (
    <div className="relative grid h-9 grid-cols-[minmax(0,1fr)_minmax(0,0.8fr)_minmax(0,1.2fr)] items-center overflow-hidden px-4 font-mono text-[11px]">
      <span
        className={cn(
          "absolute inset-y-0 right-0",
          ask ? "bg-negative-soft" : "bg-positive-soft",
        )}
        style={{ width: `${Math.max(3, (quantity / maxQuantity) * 72)}%` }}
      />
      <span className={cn("relative font-semibold", ask ? "text-negative" : "text-positive")}>
        {formatFixed(level.price)}
      </span>
      <span className="relative text-right text-muted-foreground">
        {formatFixed(level.quantity, true)}
      </span>
      <span className="relative truncate text-right font-sans text-[10px] text-muted-foreground" title={contributions}>
        {contributions}
      </span>
    </div>
  );
}

function BookRows({
  side,
  rows,
  maxQuantity,
}: {
  side: "ask" | "bid";
  rows: AggregatedPriceLevel[];
  maxQuantity: number;
}) {
  const container = React.useRef<HTMLDivElement>(null);
  const displayedRows = orderBookDisplayLevels(rows, side);

  React.useLayoutEffect(() => {
    if (side === "ask" && container.current) {
      container.current.scrollTop = container.current.scrollHeight;
    }
  }, [side]);

  return (
    <div
      ref={container}
      className="h-[22.5rem] overflow-y-scroll divide-y divide-border/55"
    >
      {displayedRows.map((level) => (
        <BookRow
          key={`${side}-${formatFixed(level.price)}`}
          level={level}
          maxQuantity={maxQuantity}
        />
      ))}
      {displayedRows.length === 0 && (
        <div className="p-8 text-center text-xs text-muted-foreground">
          暂无{side === "ask" ? "卖" : "买"}盘
        </div>
      )}
    </div>
  );
}

const beijingTimeFormatter = new Intl.DateTimeFormat("zh-CN", {
  timeZone: "Asia/Shanghai",
  hour: "2-digit",
  minute: "2-digit",
  hour12: false,
});

export function formatBeijingTime(timestampMs: number) {
  return beijingTimeFormatter.format(new Date(timestampMs));
}

export function SpreadChart({
  points,
  startMs,
  endMs,
}: {
  points: SpreadPoint[];
  startMs: number | null;
  endMs: number | null;
}) {
  const width = 760;
  const height = 280;
  const margin = { top: 18, right: 22, bottom: 34, left: 48 };
  const values = points.flatMap((point) =>
    point.spreadBps === null ? [] : [point.spreadBps],
  );
  if (values.length === 0) {
    return <div className="flex h-56 items-center justify-center text-xs text-muted-foreground">24 小时内暂无历史数据</div>;
  }
  const domain = spreadChartDomain(values)!;
  const yMin = domain.minimum;
  const yMax = domain.maximum;
  const validTimestamps = points
    .map((point) => Date.parse(point.timestamp))
    .filter(Number.isFinite);
  const xStart = startMs ?? Math.min(...validTimestamps);
  const xEnd = endMs ?? Math.max(...validTimestamps);
  const x = (timestampMs: number) =>
    margin.left + ((timestampMs - xStart) / Math.max(xEnd - xStart, 1)) * (width - margin.left - margin.right);
  const y = (value: number) =>
    margin.top + ((yMax - value) / Math.max(yMax - yMin, 0.0001)) * (height - margin.top - margin.bottom);
  const segments: string[] = [];
  let path = "";
  points.forEach((point) => {
    if (point.spreadBps === null) {
      if (path) segments.push(path);
      path = "";
      return;
    }
    path += `${path ? " L" : "M"} ${x(Date.parse(point.timestamp)).toFixed(2)} ${y(point.spreadBps).toFixed(2)}`;
  });
  if (path) segments.push(path);
  return (
    <svg viewBox={`0 0 ${width} ${height}`} className="h-auto w-full" role="img" aria-label="过去24小时 Spread BPS，北京时间，缺失时段断线显示">
      {[0, 0.25, 0.5, 0.75, 1].map((ratio) => {
        const value = yMax - ratio * (yMax - yMin);
        return (
          <g key={ratio}>
            <line x1={margin.left} x2={width - margin.right} y1={y(value)} y2={y(value)} stroke="var(--border)" strokeDasharray="4 5" />
            <text x={margin.left - 9} y={y(value) + 4} textAnchor="end" fill="var(--muted-foreground)" fontSize="10">{value.toFixed(2)}</text>
          </g>
        );
      })}
      {segments.map((segment, index) => (
        <path key={index} d={segment} fill="none" stroke="var(--primary)" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round" />
      ))}
      {[0, 0.25, 0.5, 0.75, 1].map((ratio) => {
        const timestamp = xStart + ratio * (xEnd - xStart);
        return (
        <text key={ratio} x={x(timestamp)} y={height - 8} textAnchor={ratio === 0 ? "start" : ratio === 1 ? "end" : "middle"} fill="var(--muted-foreground)" fontSize="10">
          {formatBeijingTime(timestamp)}
        </text>
        );
      })}
    </svg>
  );
}

export function DashboardContent() {
  const {
    markets,
    selectedMarket,
    selectedProduct,
    selectProduct,
    selectMarket,
    levels,
    automaticIncrement,
    history,
    distribution,
    historyCoverage,
    historyGapCount,
    historyStartMs,
    historyEndMs,
    loading,
    error,
    historyLoading,
    historyError,
    connection,
    stale,
    lastUpdateAt,
    retry,
  } = useOrderbook();
  const market = selectedMarket;
  const selectedMarketKey = market ? aggdataMarketKey(market) : "";
  const filteredMarkets = markets.filter(
    (item) => aggdataProductFromProfile(item.profile) === selectedProduct,
  );
  const [manualSelection, setManualSelection] = React.useState<{
    marketKey: string;
    increment: FixedDecimal;
  } | null>(null);
  const manualIncrement =
    manualSelection?.marketKey === selectedMarketKey
      ? manualSelection.increment
      : null;
  const tick = React.useMemo(() => inferEffectiveTick(levels), [levels]);
  const increments = React.useMemo(
    () => (tick ? generate125Increments(tick) : []),
    [tick],
  );
  const increment = manualIncrement ?? automaticIncrement;
  const asks = React.useMemo(
    () => (increment ? aggregateBookLevels(levels, "ask", increment) : []),
    [increment, levels],
  );
  const bids = React.useMemo(
    () => (increment ? aggregateBookLevels(levels, "bid", increment) : []),
    [increment, levels],
  );
  const bestAsk = getBestAsk(levels);
  const bestBid = getBestBid(levels);
  const spread = bestBid && bestAsk ? calculateSpreadBps(bestBid.price, bestAsk.price) : null;
  const historyValues = history.flatMap((point) => point.spreadBps === null ? [] : [point.spreadBps]);
  const ccdf = React.useMemo(
    () =>
      calculateSpreadCcdf(
        distribution.map((spreadBps, index) => ({
          timestamp: String(index),
          spreadBps,
        })),
      ),
    [distribution],
  );
  const maxQuantity = Math.max(
    1,
    ...asks.concat(bids).map((level) => fixedToNumber(level.quantity)),
  );
  const venueCount = new Set(levels.map((level) => level.exchange)).size;

  return (
    <div className="min-w-0 space-y-4">
      <section className="rounded-xl border bg-card/80 p-3 shadow-sm">
        <div className="flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
          <div className="flex min-w-0 flex-wrap items-end gap-3">
            <label>
              <span className="mb-1.5 block text-[10px] font-medium uppercase tracking-wider text-muted-foreground">合约类型</span>
              <span className="relative block">
                <select
                  value={selectedProduct}
                  onChange={(event) => {
                    setManualSelection(null);
                    selectProduct(event.target.value as "SPOT" | "PERPETUAL");
                  }}
                  className="h-10 min-w-32 appearance-none rounded-md border bg-background px-3 pr-9 text-sm outline-none"
                >
                  <option value="SPOT">现货</option>
                  <option value="PERPETUAL">永续</option>
                </select>
                <ChevronDown className="pointer-events-none absolute right-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
              </span>
            </label>
            <label>
              <span className="mb-1.5 block text-[10px] font-medium uppercase tracking-wider text-muted-foreground">交易标的</span>
              <span className="relative block">
                <select value={selectedMarketKey} disabled={filteredMarkets.length === 0} onChange={(event) => selectMarket(event.target.value)} className="h-10 min-w-44 appearance-none rounded-md border bg-background px-3 pr-9 font-mono text-sm outline-none">
                  {filteredMarkets.map((item) => {
                    const marketKey = aggdataMarketKey(item);
                    return <option key={marketKey} value={marketKey}>{item.symbol}</option>;
                  })}
                </select>
                <ChevronDown className="pointer-events-none absolute right-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
              </span>
            </label>
            <label>
              <span className="mb-1.5 block text-[10px] font-medium uppercase tracking-wider text-muted-foreground">价格增量</span>
              <span className="relative block">
                <select
                  value={manualIncrement ? `${manualIncrement.mantissa}:${manualIncrement.scale}` : "auto"}
                  disabled={!automaticIncrement}
                  onChange={(event) => {
                    const selected = increments.find((item) => `${item.mantissa}:${item.scale}` === event.target.value);
                    setManualSelection(
                      selected
                        ? { marketKey: selectedMarketKey, increment: selected }
                        : null,
                    );
                  }}
                  className="h-10 min-w-40 appearance-none rounded-md border bg-background px-3 pr-9 font-mono text-sm outline-none"
                >
                  <option value="auto">自动 · {automaticIncrement ? formatFixed(automaticIncrement) : "—"}</option>
                  {increments.map((item) => (
                    <option key={`${item.mantissa}:${item.scale}`} value={`${item.mantissa}:${item.scale}`}>{formatFixed(item)}</option>
                  ))}
                </select>
                <ChevronDown className="pointer-events-none absolute right-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
              </span>
            </label>
          </div>
          <div className={cn("inline-flex h-8 items-center gap-2 self-start rounded-full border px-3 text-[11px]", stale || error ? "text-amber-600" : "text-muted-foreground")}>
            {connection === "live" && !stale ? <Wifi className="size-3.5" /> : <WifiOff className="size-3.5" />}
            {stale ? "数据已过期" : connection === "live" ? "实时连接" : connection === "reconnecting" ? "正在重连" : "正在连接"}
            {lastUpdateAt && <span>· {new Date(lastUpdateAt).toLocaleTimeString()}</span>}
          </div>
        </div>
      </section>

      {error && (
        <div className="flex items-center justify-between rounded-lg border border-amber-500/40 bg-amber-500/10 px-4 py-3 text-sm text-amber-700">
          <span className="flex items-center gap-2"><AlertCircle className="size-4" />{error}</span>
          <button type="button" onClick={retry} className="inline-flex items-center gap-1.5 rounded-md border px-2.5 py-1.5 text-xs"><RefreshCw className="size-3.5" />重试</button>
        </div>
      )}

      {loading && levels.length === 0 ? (
        <div className="flex h-80 items-center justify-center rounded-xl border bg-card/80 text-sm text-muted-foreground">
          <LoaderCircle className="mr-2 size-5 animate-spin" />正在加载聚合盘口…
        </div>
      ) : (
        <section className="grid min-w-0 items-start gap-4 xl:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]">
          <div className={cn("overflow-hidden rounded-xl border bg-card/85 shadow-sm", stale && "opacity-75")}>
            <div className="flex items-center justify-between border-b px-4 py-3.5">
              <div>
                <h2 className="text-sm font-semibold">{market?.symbol || "—"} 聚合盘口</h2>
                <p className="mt-1 text-[11px] text-muted-foreground">{increment ? `${formatFixed(increment)} ${market?.quoteAsset ?? ""}` : "等待有效价格档位"} · ask 向上 / bid 向下聚合</p>
              </div>
              <span className="text-[10px] text-muted-foreground">{venueCount} VENUES</span>
            </div>
            <div className="grid h-9 grid-cols-[minmax(0,1fr)_minmax(0,0.8fr)_minmax(0,1.2fr)] items-center border-b bg-muted/35 px-4 text-[10px] uppercase tracking-wider text-muted-foreground">
              <span>价格 ({market?.quoteAsset})</span><span className="text-right">数量 ({market?.baseAsset})</span><span className="text-right">交易所贡献</span>
            </div>
            {(["ask", "bid"] as const).map((side) => {
              const rows = side === "ask" ? asks : bids;
              return (
                <React.Fragment key={side}>
                  <div className={cn("flex h-8 items-center justify-between border-b px-4 text-[10px] uppercase", side === "ask" ? "bg-negative-soft text-negative" : "bg-positive-soft text-positive")}>
                    <span>{side}</span><span className="text-muted-foreground">{rows.length} 档</span>
                  </div>
                  <BookRows side={side} rows={rows} maxQuantity={maxQuantity} />
                  {side === "ask" && <div className="flex items-center justify-between border-y bg-muted/45 px-4 py-3 font-mono text-xs"><span>SPREAD</span><strong className="text-primary">{spread === null ? "—" : `${spread.toFixed(3)} BPS`}</strong></div>}
                </React.Fragment>
              );
            })}
          </div>

          <div className="space-y-3 xl:sticky xl:top-16">
            <div className="grid grid-cols-2 gap-2">
              {[
                ["Best Bid", bestBid ? formatFixed(bestBid.price) : "—", "text-positive"],
                ["Best Ask", bestAsk ? formatFixed(bestAsk.price) : "—", "text-negative"],
                ["Current Spread", spread === null ? "—" : `${spread.toFixed(3)} BPS`, ""],
                [
                  "24H Range",
                  historyValues.length
                    ? `${Math.min(...historyValues).toFixed(2)}–${Math.max(...historyValues).toFixed(2)}`
                    : historyLoading
                      ? "加载中…"
                      : "—",
                  "",
                ],
              ].map(([label, value, tone]) => (
                <div key={label} className="rounded-lg border bg-card/85 p-3">
                  <div className="text-[10px] uppercase tracking-wider text-muted-foreground">{label}</div>
                  <div className={cn("mt-1.5 font-mono text-base font-semibold", tone)}>{value}</div>
                </div>
              ))}
            </div>
            <div className="rounded-xl border bg-card/85 p-4 shadow-sm">
              <div className="flex items-center gap-2"><Activity className="size-4 text-primary" /><h2 className="text-sm font-semibold">24H Spread BPS</h2></div>
              <p className={cn("mt-1.5 text-xs", historyCoverage < 0.9 ? "text-amber-600" : "text-muted-foreground")}>
                分钟级走势 · 北京时间 · 覆盖率 {(historyCoverage * 100).toFixed(1)}% · {historyGapCount} 个缺口
              </p>
              {historyError && history.length > 0 && (
                <p className="mt-1 text-xs text-amber-600">历史刷新失败，继续显示最近一次成功结果</p>
              )}
              <div className="mt-4">
                {historyLoading && history.length === 0 ? (
                  <div className="flex h-56 items-center justify-center text-xs text-muted-foreground">
                    <LoaderCircle className="mr-2 size-4 animate-spin" />
                    正在加载 24H 历史…
                  </div>
                ) : historyError && history.length === 0 ? (
                  <div className="flex h-56 items-center justify-center text-xs text-amber-600">
                    24H 历史暂不可用，实时盘口不受影响
                  </div>
                ) : (
                  <SpreadChart
                    points={history}
                    startMs={historyStartMs}
                    endMs={historyEndMs}
                  />
                )}
              </div>
            </div>
            <div className="rounded-xl border bg-card/85 p-4 shadow-sm">
              <div className="flex items-center gap-2"><BarChart3 className="size-4 text-primary" /><h2 className="text-sm font-semibold">24H Spread CCDF</h2></div>
              <p className={cn("mt-1.5 text-xs", historyCoverage < 0.9 ? "text-amber-600" : "text-muted-foreground")}>
                基于已采集样本计算 · 24H 覆盖率 {(historyCoverage * 100).toFixed(1)}%
              </p>
              <div className="mt-4 space-y-2.5">
                {ccdf.map((point) => (
                  <div key={point.thresholdBps} className="grid grid-cols-[4.5rem_minmax(0,1fr)_3rem] items-center gap-2 text-xs sm:grid-cols-[5rem_minmax(0,1fr)_3rem] sm:gap-3">
                    <span className="font-mono text-muted-foreground">≥ {point.thresholdBps.toFixed(2)}</span>
                    <div className="h-2 overflow-hidden rounded-full bg-muted"><div className="h-full bg-primary" style={{ width: `${point.probability * 100}%` }} /></div>
                    <strong className="text-right font-mono">{(point.probability * 100).toFixed(0)}%</strong>
                  </div>
                ))}
                {ccdf.length === 0 && (
                  <p className="py-6 text-center text-xs text-muted-foreground">
                    {historyLoading
                      ? "历史分布加载中…"
                      : historyError
                        ? "历史分布暂不可用"
                        : "暂无分布数据"}
                  </p>
                )}
              </div>
            </div>
          </div>
        </section>
      )}
    </div>
  );
}

export function OrderbookDashboard() {
  return (
    <OrderbookProvider>
      <DashboardContent />
    </OrderbookProvider>
  );
}
