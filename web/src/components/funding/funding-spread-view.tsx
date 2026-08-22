"use client";

import * as React from "react";
import {
  type SortingState,
  flexRender,
} from "@tanstack/react-table";
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
  CalendarClock,
  ChevronRight,
  Info,
  LoaderCircle,
  ShieldAlert,
} from "lucide-react";

import { WorkspacePanel } from "@/components/layout/responsive";
import { Badge } from "@/components/ui/badge";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { useElementWidth } from "@/hooks/use-element-width";
import {
  formatCurrency,
  formatDateTime,
  formatFundingRate,
  formatPercent,
  formatSettlementCountdown,
  rateColor,
} from "@/lib/market-format";
import { FUNDING_DETAIL_WIDTH_PX, shouldUseSplitLayout } from "@/lib/layout";
import { cn } from "@/lib/utils";
import type { Exchange, FundingSpread, FundingSpreadLeg } from "@/types/market";

const exchangeDot: Record<Exchange, string> = {
  Binance: "bg-amber-400",
  OKX: "bg-foreground",
  Bybit: "bg-orange-500",
  Bitget: "bg-cyan-500",
  Gate: "bg-blue-500",
  Hyperliquid: "bg-emerald-500",
  Polymarket: "bg-violet-500",
};

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

function SettlementCountdown({ value }: { value: string }) {
  const [now, setNow] = React.useState(() => Date.now());
  React.useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1_000);
    return () => window.clearInterval(timer);
  }, []);
  return <>{formatSettlementCountdown(value, now)}</>;
}

function IntervalBadge({ leg }: { leg: FundingSpreadLeg }) {
  return (
    <Badge variant="outline" className="h-5 px-1.5 font-mono text-[10px]">
      {leg.settlementIntervalHours}h
    </Badge>
  );
}

function LegCell({ leg, side }: { leg: FundingSpreadLeg; side: "long" | "short" }) {
  return (
    <div className="min-w-32">
      <div className="flex items-center gap-1.5">
        <span className={cn("size-1.5 rounded-full", exchangeDot[leg.exchange])} />
        <span className="text-xs font-medium">{leg.exchange}</span>
        <IntervalBadge leg={leg} />
      </div>
      <div className="mt-1 flex items-center justify-between gap-3 font-mono text-[10px]">
        <span className={cn(side === "long" ? "text-positive" : "text-negative")}>
          {side === "long" ? "多" : "空"} {formatFundingRate(leg.fundingRate)}
        </span>
        <span className="text-muted-foreground">
          <SettlementCountdown value={leg.nextSettlementAt} />
        </span>
      </div>
    </div>
  );
}

function LegCard({ leg, side }: { leg: FundingSpreadLeg; side: "long" | "short" }) {
  const isLong = side === "long";
  return (
    <div className={cn("rounded-lg border p-3", isLong ? "bg-positive-soft/40" : "bg-negative-soft/40")}>
      <div className="flex items-center justify-between">
        <Badge className={isLong ? "bg-positive-soft text-positive" : "bg-negative-soft text-negative"}>
          {isLong ? "做多" : "做空"}
        </Badge>
        <div className="flex items-center gap-1.5 text-xs font-medium">
          <span className={cn("size-1.5 rounded-full", exchangeDot[leg.exchange])} />
          {leg.exchange}
          <IntervalBadge leg={leg} />
        </div>
      </div>
      <div className="mt-3 grid grid-cols-2 gap-3 text-xs">
        <div>
          <div className="text-muted-foreground">资金费率</div>
          <div className={cn("mt-1 font-mono font-semibold", rateColor(leg.fundingRate))}>
            {formatFundingRate(leg.fundingRate)}
          </div>
        </div>
        <div className="text-right">
          <div className="text-muted-foreground">最新价格</div>
          <div className="mt-1 font-mono font-semibold">{formatCurrency(leg.latestPrice)}</div>
        </div>
        <div>
          <div className="text-muted-foreground">下次结算</div>
          <div className="mt-1 font-mono"><SettlementCountdown value={leg.nextSettlementAt} /></div>
        </div>
        <div className="text-right">
          <div className="text-muted-foreground">结算时间</div>
          <div className="mt-1 font-mono text-[10px]">{formatDateTime(leg.nextSettlementAt)}</div>
        </div>
      </div>
    </div>
  );
}

function SpreadDetail({ spread }: { spread: FundingSpread }) {
  return (
    <div className="flex h-full flex-col overflow-y-auto">
      <div className="border-b p-4">
        <div className="flex items-start justify-between gap-2">
          <div>
            <div className="text-xs text-muted-foreground">跨所资金费套利</div>
            <h2 className="mt-1 font-mono text-xl font-semibold">{spread.symbol}</h2>
          </div>
          {spread.stale && (
            <Badge variant="outline" className="gap-1 border-amber-500/30 text-amber-600">
              <ShieldAlert className="size-3" /> 数据延迟
            </Badge>
          )}
        </div>
        <div className="mt-4 space-y-2">
          <LegCard leg={spread.longLeg} side="long" />
          <LegCard leg={spread.shortLeg} side="short" />
        </div>
      </div>
      <div className="grid grid-cols-3 gap-px border-b bg-border">
        {[
          ["即时年化", spread.spreadAnnualized],
          ["24H 年化", spread.spread24hAnnualized],
          ["7D 年化", spread.spread7dAnnualized],
        ].map(([label, value]) => (
          <div key={String(label)} className="bg-card p-3 text-center">
            <div className="text-[10px] text-muted-foreground">{label}</div>
            <div className={cn("mt-1 font-mono text-xs font-semibold", rateColor(Number(value)))}>
              {formatPercent(Number(value), 1)}
            </div>
          </div>
        ))}
      </div>
      <div className="p-4">
        <h3 className="text-sm font-medium">可用容量（较小腿）</h3>
        <div className="mt-3 grid grid-cols-2 gap-3">
          <div className="rounded-md bg-muted/45 p-3">
            <div className="text-[11px] text-muted-foreground">持仓名义价值</div>
            <div className="mt-1 font-mono text-sm font-semibold">
              {formatCurrency(spread.minPositionNotional, true)}
            </div>
          </div>
          <div className="rounded-md bg-muted/45 p-3">
            <div className="text-[11px] text-muted-foreground">24H 成交额</div>
            <div className="mt-1 font-mono text-sm font-semibold">
              {formatCurrency(spread.minDailyVolume, true)}
            </div>
          </div>
        </div>
        <div className="mt-4 flex items-center gap-1.5 text-[10px] text-muted-foreground">
          <CalendarClock className="size-3" />
          较旧一腿更新于 {formatDateTime(spread.updatedAt)}
        </div>
        {spread.stale && (
          <div className="mt-3 rounded-md border border-amber-500/30 bg-amber-500/8 p-3 text-xs text-amber-700 dark:text-amber-300">
            任一腿数据过期都会影响方向与收益估算，请等待新快照后再决策。
          </div>
        )}
      </div>
    </div>
  );
}

export function FundingSpreadView({
  data,
  loading,
}: {
  data: FundingSpread[];
  loading: boolean;
}) {
  const [sorting, setSorting] = React.useState<SortingState>([
    { id: "spreadAnnualized", desc: true },
  ]);
  const [selectedId, setSelectedId] = React.useState<string | null>(null);
  const [mobileDetailOpen, setMobileDetailOpen] = React.useState(false);
  const [workspaceRef, width] = useElementWidth<HTMLDivElement>();
  const showSplit = shouldUseSplitLayout(width, FUNDING_DETAIL_WIDTH_PX);
  const selected = data.find((item) => item.id === selectedId) ?? data[0] ?? null;

  React.useEffect(() => {
    if (showSplit) setMobileDetailOpen(false);
  }, [showSplit]);

  const columns = React.useMemo<LegacyColumnDef<FundingSpread>[]>(
    () => [
      {
        accessorKey: "symbol",
        header: "币对",
        cell: ({ row }) => (
          <div>
            <div className="font-mono text-xs font-semibold">{row.original.baseAsset}/{row.original.quoteAsset}</div>
            <div className="mt-0.5 text-[10px] text-muted-foreground">永续对冲</div>
          </div>
        ),
      },
      { id: "longLeg", accessorFn: (row) => row.longLeg.exchange, header: "做多腿", cell: ({ row }) => <LegCell leg={row.original.longLeg} side="long" /> },
      { id: "shortLeg", accessorFn: (row) => row.shortLeg.exchange, header: "做空腿", cell: ({ row }) => <LegCell leg={row.original.shortLeg} side="short" /> },
      ...([
        ["spreadAnnualized", "即时差值年化"],
        ["spread24hAnnualized", "24H 差值年化"],
        ["spread7dAnnualized", "7D 差值年化"],
      ] as const).map(([key, header]) => ({
        accessorKey: key,
        header,
        cell: ({ row }: { row: { original: FundingSpread } }) => (
          <div className={cn("text-right font-mono text-xs font-semibold", rateColor(row.original[key]))}>
            {formatPercent(row.original[key], 1)}
          </div>
        ),
      })),
      {
        accessorKey: "minPositionNotional",
        header: "较小持仓",
        cell: ({ row }) => <div className="text-right font-mono text-xs">{formatCurrency(row.original.minPositionNotional, true)}</div>,
      },
      {
        accessorKey: "minDailyVolume",
        header: "较小成交额",
        cell: ({ row }) => <div className="text-right font-mono text-xs">{formatCurrency(row.original.minDailyVolume, true)}</div>,
      },
      { id: "open", enableSorting: false, header: "", cell: () => <ChevronRight className="ml-auto size-4 text-muted-foreground" /> },
    ],
    [],
  );
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
  const containerRef = React.useRef<HTMLDivElement>(null);
  // TanStack Virtual intentionally exposes imperative functions that React Compiler skips.
  // eslint-disable-next-line react-hooks/incompatible-library
  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => containerRef.current,
    estimateSize: () => 64,
    overscan: 10,
    getItemKey: (index) => rows[index]?.original.id ?? index,
  });
  const virtualRows = virtualizer.getVirtualItems();
  const top = virtualRows[0]?.start ?? 0;
  const bottom = virtualRows.length
    ? virtualizer.getTotalSize() - virtualRows[virtualRows.length - 1].end
    : 0;
  const select = (id: string) => {
    setSelectedId(id);
    if (!showSplit) setMobileDetailOpen(true);
  };

  return (
    <>
      <WorkspacePanel ref={workspaceRef} className={cn(showSplit && "grid-cols-[minmax(0,1fr)_380px]")}>
        <div ref={containerRef} data-wide-table-scroll className="min-w-0 overflow-auto">
          <table className="w-full min-w-[1120px] border-collapse">
            <thead className="sticky top-0 z-10 bg-muted/80 backdrop-blur">
              {table.getHeaderGroups().map((group) => (
                <tr key={group.id} className="border-b">
                  {group.headers.map((header, index) => (
                    <th key={header.id} className={cn("h-11 px-3 text-left", index >= 3 && index <= 7 && "text-right")}>
                      {header.isPlaceholder ? null : header.column.getCanSort() ? (
                        <SortHeader
                          label={String(header.column.columnDef.header)}
                          sorted={header.column.getIsSorted()}
                          onClick={() => header.column.toggleSorting()}
                          align={index >= 3 && index <= 7 ? "right" : "left"}
                        />
                      ) : flexRender(header.column.columnDef.header, header.getContext())}
                    </th>
                  ))}
                </tr>
              ))}
            </thead>
            <tbody>
              {rows.length ? (
                <>
                  {top > 0 && <tr aria-hidden><td colSpan={columns.length} style={{ height: top }} /></tr>}
                  {virtualRows.map((virtualRow) => {
                    const row = rows[virtualRow.index];
                    return (
                      <tr
                        key={row.original.id}
                        tabIndex={0}
                        onClick={() => select(row.original.id)}
                        onKeyDown={(event) => {
                          if (event.key === "Enter" || event.key === " ") {
                            event.preventDefault();
                            select(row.original.id);
                          }
                        }}
                        className={cn(
                          "cursor-pointer border-b transition-colors hover:bg-muted/45 focus-visible:bg-muted focus-visible:outline-none",
                          selected?.id === row.original.id && "bg-primary/[0.055]",
                        )}
                      >
                        {row.getVisibleCells().map((cell) => (
                          <td key={cell.id} className="h-16 px-3">{flexRender(cell.column.columnDef.cell, cell.getContext())}</td>
                        ))}
                      </tr>
                    );
                  })}
                  {bottom > 0 && <tr aria-hidden><td colSpan={columns.length} style={{ height: bottom }} /></tr>}
                </>
              ) : (
                <tr>
                  <td colSpan={columns.length} className="h-72 text-center">
                    <div className="mx-auto flex max-w-xs flex-col items-center text-muted-foreground">
                      {loading ? <LoaderCircle className="size-7 animate-spin" /> : <Info className="size-7" />}
                      <div className="mt-3 text-sm font-medium text-foreground">
                        {loading ? "正在加载套利组合" : "没有符合条件的套利组合"}
                      </div>
                      <div className="mt-1 text-xs">降低较小腿阈值或放宽交易所与周期筛选</div>
                    </div>
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
        <aside className={cn("border-l bg-card", showSplit ? "block" : "hidden")}>
          {selected ? <SpreadDetail spread={selected} /> : <div className="flex h-full items-center justify-center text-xs text-muted-foreground">选择组合后查看双腿详情</div>}
        </aside>
      </WorkspacePanel>
      {selected && !showSplit && (
        <Sheet open={mobileDetailOpen} onOpenChange={setMobileDetailOpen}>
          <SheetContent className="w-full overflow-y-auto p-0 sm:max-w-md">
            <SheetHeader className="sr-only">
              <SheetTitle>{selected.symbol} 跨所套利详情</SheetTitle>
              <SheetDescription>展示推荐方向、双腿结算与差值年化</SheetDescription>
            </SheetHeader>
            <SpreadDetail spread={selected} />
          </SheetContent>
        </Sheet>
      )}
    </>
  );
}
