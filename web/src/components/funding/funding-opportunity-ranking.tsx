"use client";

import * as React from "react";
import { flexRender, type SortingState } from "@tanstack/react-table";
import {
  getCoreRowModel,
  getSortedRowModel,
  type LegacyColumnDef,
  useLegacyTable,
} from "@tanstack/react-table/legacy";
import { useVirtualizer } from "@tanstack/react-virtual";
import {
  ArrowDown,
  ArrowDownUp,
  ArrowUp,
  ChevronRight,
  Info,
  LoaderCircle,
  RefreshCw,
} from "lucide-react";

import { BasisSpreadPanel } from "@/components/funding/basis-spread-chart";
import { StartTradeButton } from "@/components/funding/start-trade-button";
import { WorkspacePanel } from "@/components/layout/responsive";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { useElementWidth } from "@/hooks/use-element-width";
import {
  fetchFundingOpportunities,
  type FundingOpportunitySnapshot,
} from "@/lib/api/funding-opportunities";
import { bboCanonicalSymbol } from "@/lib/funding-coverage";
import { FUNDING_DETAIL_WIDTH_PX, shouldUseSplitLayout } from "@/lib/layout";
import { buildArbitragePrefillUrlFromSpread } from "@/lib/trading-prefill";
import {
  formatCurrency,
  formatDateTime,
  formatFundingRate,
  formatPaybackPeriod,
  formatPercent,
  rateColor,
} from "@/lib/market-format";
import { cn } from "@/lib/utils";
import { useVisibleMeasure } from "@/lib/visible-measure";
import type {
  FundingFilters,
  FundingOpportunityPeriod,
  FundingSpreadLeg,
  RankedFundingOpportunity,
} from "@/types/market";

type RankingListItem =
  | { kind: "row"; key: string; item: RankedFundingOpportunity }
  | { kind: "detail"; key: string; item: RankedFundingOpportunity };

const DATA_ROW_HEIGHT_PX = 64;
const DETAIL_ROW_HEIGHT_PX = 328;

const periods: FundingOpportunityPeriod[] = ["8h", "24h"];

function SortHeader({
  label,
  sorted,
  align = "left",
  onClick,
}: {
  label: string;
  sorted: false | "asc" | "desc";
  align?: "left" | "right";
  onClick: () => void;
}) {
  const Icon = sorted === "asc" ? ArrowUp : sorted === "desc" ? ArrowDown : ArrowDownUp;
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        "inline-flex items-center gap-1 whitespace-nowrap text-[11px] font-medium uppercase tracking-wider text-muted-foreground hover:text-foreground",
        align === "right" && "w-full justify-end",
      )}
    >
      {label}
      <Icon className={cn("size-3", !sorted && "opacity-35")} />
    </button>
  );
}

function LegSummary({ leg, side }: { leg: FundingSpreadLeg; side: "long" | "short" }) {
  return (
    <div className="min-w-28">
      <div className={cn("text-xs font-semibold", side === "long" ? "text-positive" : "text-negative")}>
        {side === "long" ? "做多" : "做空"} · {leg.exchange}
      </div>
      <div className="mt-1 font-mono text-[10px] text-muted-foreground">
        {formatFundingRate(leg.fundingRate)} · {leg.settlementIntervalHours}h
      </div>
    </div>
  );
}

function LegDetail({ leg, side }: { leg: FundingSpreadLeg; side: "long" | "short" }) {
  const isLong = side === "long";
  return (
    <div className={cn("rounded-lg border p-3", isLong ? "bg-positive-soft/40" : "bg-negative-soft/40")}>
      <div className="flex items-center justify-between">
        <Badge className={isLong ? "bg-positive-soft text-positive" : "bg-negative-soft text-negative"}>
          {isLong ? "明确做多" : "明确做空"}
        </Badge>
        <span className="text-xs font-semibold">{leg.exchange}</span>
      </div>
      <div className="mt-3 grid grid-cols-2 gap-3 text-xs">
        <div><div className="text-muted-foreground">交易合约</div><div className="mt-1 font-mono">{leg.exchangeSymbol}</div></div>
        <div className="text-right"><div className="text-muted-foreground">资金费率</div><div className={cn("mt-1 font-mono font-semibold", rateColor(leg.fundingRate))}>{formatFundingRate(leg.fundingRate)}</div></div>
        <div><div className="text-muted-foreground">最新价格</div><div className="mt-1 font-mono">{formatCurrency(leg.latestPrice)}</div></div>
        <div className="text-right"><div className="text-muted-foreground">结算周期</div><div className="mt-1 font-mono">{leg.settlementIntervalHours}h</div></div>
      </div>
    </div>
  );
}

function RankingDetail({ item }: { item: RankedFundingOpportunity }) {
  return (
    <div className="flex h-full flex-col overflow-y-auto">
      <div className="border-b p-4">
        <div className="flex items-start justify-between gap-2">
          <div>
            <div className="text-xs text-muted-foreground">机会排名 #{item.rank} · {item.period}</div>
            <h2 className="mt-1 font-mono text-xl font-semibold">{item.symbol}</h2>
          </div>
          <div className="flex shrink-0 items-center gap-2">
            <StartTradeButton
              href={buildArbitragePrefillUrlFromSpread(item)}
            />
          </div>
        </div>
        <div className="mt-4 space-y-2">
          <LegDetail leg={item.longLeg} side="long" />
          <LegDetail leg={item.shortLeg} side="short" />
        </div>
      </div>
      <div className="grid grid-cols-3 gap-px border-b bg-border">
        <div className="bg-card p-3 text-center">
          <div className="text-[10px] text-muted-foreground">周期预期收益</div>
          <div className={cn("mt-1 font-mono text-xs font-semibold", rateColor(item.periodExpectedReturn))}>
            {formatPercent(item.periodExpectedReturn, 2)}
          </div>
        </div>
        <div className="bg-card p-3 text-center">
          <div className="text-[10px] text-muted-foreground">盈利概率</div>
          <div className="mt-1 font-mono text-xs font-semibold">
            {formatPercent(item.profitProbability, 2)}
          </div>
        </div>
        <div className="bg-card p-3 text-center">
          <div className="text-[10px] text-muted-foreground">有效样本</div>
          <div className="mt-1 font-mono text-xs font-semibold">{item.sampleCount}</div>
        </div>
      </div>
      <div className="space-y-4 p-4 text-xs">
        <div>
          <h3 className="font-medium">收益拆解（年化）</h3>
          <div className="mt-2 grid grid-cols-2 gap-2">
            {[
              ["资金费", item.fundingExpectedAnnualized],
              ["价差（已扣 12bps）", item.spreadExpectedAnnualized],
              ["合计", item.combinedExpectedAnnualized],
            ].map(([label, value]) => (
              <div key={String(label)} className="min-w-0 rounded-md bg-muted/45 p-2 text-center">
                <div className="truncate text-[10px] text-muted-foreground">{label}</div>
                <div className={cn("mt-1 truncate font-mono text-xs font-semibold", rateColor(Number(value)))}>
                  {formatPercent(Number(value), 1)}
                </div>
              </div>
            ))}
            <div className="min-w-0 rounded-md bg-muted/45 p-2 text-center">
              <div className="text-[10px] text-muted-foreground">预计回本周期</div>
              <div className="mt-1 truncate font-mono text-xs font-semibold">
                {formatPaybackPeriod(item.paybackStatus, item.expectedPaybackMinutes)}
              </div>
            </div>
          </div>
          <div className="mt-2 text-[10px] text-muted-foreground">
            基于近 7 日真实 BBO 与已结算资金费的非重叠窗口回放；每个完整开平窗口固定扣除 12bps。
          </div>
        </div>
        <div className="grid grid-cols-2 gap-x-4 gap-y-2">
          <span className="text-muted-foreground">当前可执行价差</span><span className="text-right font-mono">{item.currentExecutableSpreadBps.toFixed(1)} bps</span>
          <span className="text-muted-foreground">预计回本周期</span><span className="text-right font-mono">{formatPaybackPeriod(item.paybackStatus, item.expectedPaybackMinutes)}</span>
          <span className="text-muted-foreground">P5 收益</span><span className={cn("text-right font-mono", rateColor(item.p5Return))}>{formatPercent(item.p5Return, 2)}</span>
          <span className="text-muted-foreground">窗口覆盖率 / 有效样本</span><span className="text-right font-mono">{formatPercent(item.coverage, 0)} / {item.sampleCount}</span>
          <span className="text-muted-foreground">较小腿持仓 / 成交额</span><span className="text-right font-mono">{formatCurrency(item.minPositionNotional, true)} / {formatCurrency(item.minDailyVolume, true)}</span>
        </div>
        <div className="text-[10px] text-muted-foreground">来源更新于 {formatDateTime(item.updatedAt)}</div>
      </div>
    </div>
  );
}

export function FundingOpportunityRanking({
  filters,
  onResultCountChange,
}: {
  filters: FundingFilters;
  onResultCountChange?: (count: number) => void;
}) {
  const [period, setPeriod] = React.useState<FundingOpportunityPeriod>("8h");
  const [snapshot, setSnapshot] = React.useState<FundingOpportunitySnapshot | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [refreshing, setRefreshing] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [sorting, setSorting] = React.useState<SortingState>([]);
  const [selectedId, setSelectedId] = React.useState<string | null>(null);
  const [expandedId, setExpandedId] = React.useState<string | null>(null);
  const [mobileDetailOpen, setMobileDetailOpen] = React.useState(false);
  const [workspaceRef, width] = useElementWidth<HTMLDivElement>();
  const showSplit = shouldUseSplitLayout(width, FUNDING_DETAIL_WIDTH_PX);
  const etag = React.useRef<string | null>(null);
  const activeRequest = React.useRef<AbortController | null>(null);

  const load = React.useCallback(async () => {
    activeRequest.current?.abort();
    const controller = new AbortController();
    activeRequest.current = controller;
    setRefreshing(true);
    try {
      const result = await fetchFundingOpportunities(
        {
          period,
          minPositionNotional: filters.minPositionNotional,
          minDailyVolume: filters.minDailyVolume,
        },
        etag.current,
        controller.signal,
      );
      if (result.status === "updated") setSnapshot(result.snapshot);
      etag.current = result.etag;
      setError(null);
    } catch (reason) {
      if (!controller.signal.aborted) {
        setError(reason instanceof Error ? reason.message : "机会排名加载失败");
      }
    } finally {
      if (activeRequest.current === controller) {
        activeRequest.current = null;
        setLoading(false);
        setRefreshing(false);
      }
    }
  }, [filters.minDailyVolume, filters.minPositionNotional, period]);

  React.useEffect(() => {
    etag.current = null;
    const initialTimer = window.setTimeout(() => void load(), 0);
    const timer = window.setInterval(() => void load(), 60_000);
    return () => {
      window.clearTimeout(initialTimer);
      window.clearInterval(timer);
      activeRequest.current?.abort();
    };
  }, [load, period]);

  React.useEffect(() => {
    if (showSplit) setMobileDetailOpen(false);
  }, [showSplit]);

  const data = React.useMemo(() => {
    const query = filters.search.trim().toLowerCase();
    return (snapshot?.data ?? []).filter((item) => {
      if (
        query &&
        !item.symbol.toLowerCase().includes(query) &&
        !item.longLeg.exchange.toLowerCase().includes(query) &&
        !item.shortLeg.exchange.toLowerCase().includes(query)
      ) return false;
      return filters.exchanges.length === 0 ||
        filters.exchanges.includes(item.longLeg.exchange) ||
        filters.exchanges.includes(item.shortLeg.exchange);
    });
  }, [filters.exchanges, filters.search, snapshot]);

  React.useEffect(() => onResultCountChange?.(data.length), [data.length, onResultCountChange]);
  const selected = data.find((item) => item.id === selectedId) ?? data[0] ?? null;

  const columns = React.useMemo<LegacyColumnDef<RankedFundingOpportunity>[]>(() => [
    { accessorKey: "symbol", header: "币对", cell: ({ row }) => <div className="font-mono text-xs font-semibold">{row.original.baseAsset}/{row.original.quoteAsset}</div> },
    { id: "longLeg", accessorFn: (row) => row.longLeg.exchange, header: "做多腿", cell: ({ row }) => <LegSummary leg={row.original.longLeg} side="long" /> },
    { id: "shortLeg", accessorFn: (row) => row.shortLeg.exchange, header: "做空腿", cell: ({ row }) => <LegSummary leg={row.original.shortLeg} side="short" /> },
    { accessorKey: "currentExecutableSpreadBps", header: "可执行价差", cell: ({ row }) => <div className="text-right font-mono text-xs">{row.original.currentExecutableSpreadBps.toFixed(1)} bps</div> },
    { accessorKey: "periodExpectedReturn", header: "周期预期收益", cell: ({ row }) => <div className={cn("text-right font-mono text-xs font-semibold", rateColor(row.original.periodExpectedReturn))}>{formatPercent(row.original.periodExpectedReturn, 2)}</div> },
    { accessorKey: "combinedExpectedAnnualized", header: "综合预期年化", cell: ({ row }) => <div className={cn("text-right font-mono text-xs font-semibold", rateColor(row.original.combinedExpectedAnnualized))}>{formatPercent(row.original.combinedExpectedAnnualized, 1)}</div> },
    { accessorKey: "profitProbability", header: "盈利概率", cell: ({ row }) => <div className="text-right font-mono text-xs">{formatPercent(row.original.profitProbability, 1)}</div> },
    { accessorKey: "expectedPaybackMinutes", header: "预计回本", cell: ({ row }) => <div className="text-right font-mono text-xs">{formatPaybackPeriod(row.original.paybackStatus, row.original.expectedPaybackMinutes)}</div> },
    { id: "open", enableSorting: false, header: "", cell: () => <ChevronRight className="ml-auto size-4 text-muted-foreground" /> },
  ], []);
  const table = useLegacyTable({
    data,
    columns,
    state: { sorting },
    onSortingChange: setSorting,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getRowId: (row) => row.id,
  });
  const rows = table.getRowModel().rows;
  const rowsById = React.useMemo(() => {
    return new Map(rows.map((row) => [row.original.id, row]));
  }, [rows]);
  const listItems = React.useMemo(() => {
    const items: RankingListItem[] = [];
    for (const row of rows) {
      items.push({
        kind: "row",
        key: `row-${row.original.id}`,
        item: row.original,
      });
      if (expandedId === row.original.id) {
        items.push({
          kind: "detail",
          key: `detail-${row.original.id}`,
          item: row.original,
        });
      }
    }
    return items;
  }, [rows, expandedId]);
  const containerRef = React.useRef<HTMLDivElement>(null);
  // eslint-disable-next-line react-hooks/incompatible-library
  const virtualizer = useVirtualizer({
    count: listItems.length,
    getScrollElement: () => containerRef.current,
    estimateSize: (index) =>
      listItems[index]?.kind === "detail" ? DETAIL_ROW_HEIGHT_PX : DATA_ROW_HEIGHT_PX,
    overscan: 10,
    getItemKey: (index) => listItems[index]?.key ?? index,
  });
  useVisibleMeasure(() => virtualizer.measure(), true);
  const virtualRows = virtualizer.getVirtualItems();
  const top = virtualRows[0]?.start ?? 0;
  const bottom = virtualRows.length ? virtualizer.getTotalSize() - virtualRows[virtualRows.length - 1].end : 0;
  const select = (id: string) => {
    setSelectedId(id);
    setExpandedId((current) => (current === id ? null : id));
    if (!showSplit) setMobileDetailOpen(true);
  };
  const unavailable = Boolean(error && !snapshot) ||
    snapshot?.meta.status === "unavailable";
  const warming = snapshot?.meta.status === "warming";

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="inline-flex rounded-lg border bg-muted/60 p-0.5" aria-label="排名周期">
          {periods.map((value) => (
            <button key={value} type="button" onClick={() => setPeriod(value)} className={cn("rounded-md px-3 py-1.5 text-xs font-medium", period === value ? "bg-background shadow-sm" : "text-muted-foreground")}>{value}</button>
          ))}
        </div>
        <div className="flex items-center gap-2 text-xs text-muted-foreground">
          {refreshing && <><LoaderCircle className="size-3 animate-spin" />后台刷新</>}
          {snapshot && <span className="font-mono">
            CALC {formatDateTime(snapshot.meta.calculatedAt)} · DATA {formatDateTime(snapshot.meta.dataThrough)}
          </span>}
        </div>
      </div>
      <p className="text-[11px] text-muted-foreground">
        Hyperliquid / Lighter USDC 永续可与其他交易所同币种 USDT 永续配对；USDC/USDT 按 1:1 比较，收益可能包含稳定币基差。
      </p>
      {(error || unavailable || warming) && (
        <div className="flex items-center justify-between rounded-lg border border-amber-500/30 bg-amber-500/8 px-3 py-2 text-xs text-amber-700 dark:text-amber-300">
          <span>{error ? (snapshot ? `刷新失败，继续展示上次数据：${error}` : `加载失败：${error}`) : unavailable ? "排名数据暂时不可用，请检查 BBO 覆盖" : "排名历史正在预热，已有周期会先展示"}</span>
          {error && <Button variant="outline" size="sm" className="h-7 gap-1" onClick={() => void load()}><RefreshCw className="size-3" />重试</Button>}
        </div>
      )}
      <>
        <WorkspacePanel ref={workspaceRef} className={cn(showSplit && "grid-cols-[minmax(0,1fr)_380px]")}>
          <div ref={containerRef} data-wide-table-scroll className="min-w-0 overflow-auto">
            <table className="w-full min-w-[1120px] border-collapse">
              <thead className="sticky top-0 z-10 bg-muted/80 backdrop-blur">
                {table.getHeaderGroups().map((group) => <tr key={group.id} className="border-b">{group.headers.map((header, index) => <th key={header.id} className={cn("h-11 px-3 text-left", index >= 3 && index <= 7 && "text-right")}>{header.isPlaceholder ? null : header.column.getCanSort() ? <SortHeader label={String(header.column.columnDef.header)} sorted={header.column.getIsSorted()} onClick={() => header.column.toggleSorting()} align={index >= 3 && index <= 7 ? "right" : "left"} /> : flexRender(header.column.columnDef.header, header.getContext())}</th>)}</tr>)}
              </thead>
              <tbody>
                {rows.length ? <>
                  {top > 0 && <tr aria-hidden><td colSpan={columns.length} style={{ height: top }} /></tr>}
                  {virtualRows.map((virtualRow) => {
                    const item = listItems[virtualRow.index];
                    if (!item) return null;
                    if (item.kind === "detail") {
                      return (
                        <tr key={item.key} className="border-b bg-muted/20 last:border-b-0">
                          <td colSpan={columns.length} className="px-4 py-3">
                            <BasisSpreadPanel
                              venue={item.item.shortLeg.exchange}
                              compareVenue={item.item.longLeg.exchange}
                              baseAsset={item.item.baseAsset}
                              quoteAsset={item.item.quoteAsset}
                              venueSymbol={bboCanonicalSymbol(item.item.shortLeg)}
                              compareVenueSymbol={bboCanonicalSymbol(item.item.longLeg)}
                            />
                          </td>
                        </tr>
                      );
                    }
                    const row = rowsById.get(item.item.id);
                    if (!row) return null;
                    return (
                      <tr
                        key={item.key}
                        tabIndex={0}
                        onClick={() => select(item.item.id)}
                        onKeyDown={(event) => {
                          if (event.key === "Enter" || event.key === " ") {
                            event.preventDefault();
                            select(item.item.id);
                          }
                        }}
                        className={cn(
                          "cursor-pointer border-b hover:bg-muted/45 focus-visible:bg-muted focus-visible:outline-none",
                          selected?.id === item.item.id && "bg-primary/[0.055]",
                        )}
                      >
                        {row.getVisibleCells().map((cell) => (
                          <td key={cell.id} className="h-16 px-3">{flexRender(cell.column.columnDef.cell, cell.getContext())}</td>
                        ))}
                      </tr>
                    );
                  })}
                  {bottom > 0 && <tr aria-hidden><td colSpan={columns.length} style={{ height: bottom }} /></tr>}
                </> : <tr><td colSpan={columns.length} className="h-72 text-center"><div className="mx-auto flex max-w-xs flex-col items-center text-muted-foreground">{loading ? <LoaderCircle className="size-7 animate-spin" /> : <Info className="size-7" />}<div className="mt-3 text-sm font-medium text-foreground">{loading ? "正在加载机会排名" : unavailable ? "排名数据暂时不可用" : "没有符合条件的排名机会"}</div><div className="mt-1 text-xs">{unavailable ? "请检查 Funding 服务与 ClickHouse 数据后重试" : "降低较小腿阈值或切换排名周期"}</div></div></td></tr>}
              </tbody>
            </table>
          </div>
          <aside className={cn("border-l bg-card", showSplit ? "block" : "hidden")}>{selected ? <RankingDetail item={selected} /> : <div className="flex h-full items-center justify-center text-xs text-muted-foreground">选择机会后查看明确双腿方向</div>}</aside>
        </WorkspacePanel>
        {selected && !showSplit && <Sheet open={mobileDetailOpen} onOpenChange={setMobileDetailOpen}><SheetContent className="w-full overflow-y-auto p-0 sm:max-w-md"><SheetHeader className="sr-only"><SheetTitle>{selected.symbol} 机会详情</SheetTitle><SheetDescription>展示明确做多与做空腿及模型收益指标</SheetDescription></SheetHeader><RankingDetail item={selected} /></SheetContent></Sheet>}
      </>
    </div>
  );
}
