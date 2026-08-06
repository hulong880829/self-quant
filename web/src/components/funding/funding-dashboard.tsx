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
import {
  ArrowDown,
  ArrowDownUp,
  ArrowUp,
  CalendarClock,
  ChevronRight,
  Clock3,
  FilterX,
  Info,
  LoaderCircle,
  RefreshCw,
  Search,
  Sparkles,
} from "lucide-react";

import { useFundingSnapshot } from "@/components/funding/funding-provider";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import {
  formatCompactNumber,
  formatCurrency,
  formatDateTime,
  formatFundingRate,
  formatPercent,
  formatSettlementCountdown,
  rateColor,
} from "@/lib/market-format";
import { cn } from "@/lib/utils";
import type {
  Exchange,
  FundingFilters,
  FundingOpportunity,
  RateDirection,
} from "@/types/market";

const exchanges: Exchange[] = [
  "Binance",
  "OKX",
  "Bybit",
  "Bitget",
  "Gate",
  "Hyperliquid",
];

const defaultFilters: FundingFilters = {
  search: "",
  minPositionNotional: 1_000_000,
  minDailyVolume: 1_000_000,
  intervalHours: "all",
  exchanges: [],
  direction: "all",
};

const exchangeDot: Record<Exchange, string> = {
  Binance: "bg-amber-400",
  OKX: "bg-foreground",
  Bybit: "bg-orange-500",
  Bitget: "bg-cyan-500",
  Gate: "bg-blue-500",
  Hyperliquid: "bg-emerald-500",
};

function SortHeader({
  label,
  sorted,
  onClick,
  align = "left",
}: {
  label: string;
  sorted: false | "asc" | "desc";
  onClick: () => void;
  align?: "left" | "right";
}) {
  const Icon = sorted === "asc" ? ArrowUp : sorted === "desc" ? ArrowDown : ArrowDownUp;
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        "inline-flex items-center gap-1 whitespace-nowrap text-[11px] font-medium uppercase tracking-wider text-muted-foreground transition-colors hover:text-foreground",
        align === "right" && "w-full justify-end",
      )}
    >
      {label}
      <Icon className={cn("size-3", !sorted && "opacity-35")} />
    </button>
  );
}

function RangeFilter({
  label,
  value,
  min,
  max,
  step,
  formatter,
  onChange,
}: {
  label: string;
  value: number;
  min: number;
  max: number;
  step: number;
  formatter: (value: number) => string;
  onChange: (value: number) => void;
}) {
  const progress = ((value - min) / (max - min)) * 100;
  return (
    <div className="min-w-48 flex-1">
      <div className="mb-2 flex items-center justify-between text-xs">
        <span className="text-muted-foreground">{label}</span>
        <span className="font-mono font-medium">{formatter(value)}+</span>
      </div>
      <input
        type="range"
        min={min}
        max={max}
        step={step}
        value={value}
        aria-label={label}
        onChange={(event) => onChange(Number(event.target.value))}
        className="h-1.5 w-full cursor-pointer appearance-none rounded-full bg-muted accent-primary"
        style={{
          background: `linear-gradient(to right, var(--primary) 0%, var(--primary) ${progress}%, var(--muted) ${progress}%, var(--muted) 100%)`,
        }}
      />
    </div>
  );
}

function Filters({
  filters,
  setFilters,
  resultCount,
}: {
  filters: FundingFilters;
  setFilters: React.Dispatch<React.SetStateAction<FundingFilters>>;
  resultCount: number;
}) {
  const toggleExchange = (exchange: Exchange) => {
    setFilters((current) => ({
      ...current,
      exchanges: current.exchanges.includes(exchange)
        ? current.exchanges.filter((item) => item !== exchange)
        : [...current.exchanges, exchange],
    }));
  };

  return (
    <section className="rounded-xl border bg-card/80 shadow-sm backdrop-blur">
      <div className="flex flex-col gap-4 p-4 xl:flex-row xl:items-end">
        <RangeFilter
          label="持仓名义价值"
          value={filters.minPositionNotional}
          min={0}
          max={50_000_000}
          step={1_000_000}
          formatter={(value) => formatCurrency(value, true)}
          onChange={(value) =>
            setFilters((current) => ({ ...current, minPositionNotional: value }))
          }
        />
        <RangeFilter
          label="日成交额"
          value={filters.minDailyVolume}
          min={0}
          max={100_000_000}
          step={1_000_000}
          formatter={(value) => formatCurrency(value, true)}
          onChange={(value) =>
            setFilters((current) => ({ ...current, minDailyVolume: value }))
          }
        />
        <div className="min-w-44">
          <label className="mb-2 block text-xs text-muted-foreground">结算间隔</label>
          <select
            value={filters.intervalHours}
            onChange={(event) =>
              setFilters((current) => ({
                ...current,
                intervalHours:
                  event.target.value === "all"
                    ? "all"
                    : Number(event.target.value),
              }))
            }
            className="h-9 w-full rounded-md border bg-background px-3 text-sm outline-none ring-ring transition-shadow focus:ring-2"
          >
            <option value="all">全部间隔</option>
            <option value={1}>每 1 小时</option>
            <option value={4}>每 4 小时</option>
            <option value={8}>每 8 小时</option>
          </select>
        </div>
        <div className="relative min-w-52 flex-[1.25]">
          <label className="mb-2 block text-xs text-muted-foreground">搜索币种</label>
          <Search className="pointer-events-none absolute bottom-2.5 left-3 size-4 text-muted-foreground" />
          <Input
            value={filters.search}
            placeholder="BTC、ETH、USDT..."
            onChange={(event) =>
              setFilters((current) => ({ ...current, search: event.target.value }))
            }
            className="pl-9"
          />
        </div>
        <Button
          variant="outline"
          onClick={() => setFilters(defaultFilters)}
          className="gap-2"
        >
          <FilterX className="size-4" />
          重置
        </Button>
      </div>

      <div className="flex flex-col gap-3 border-t px-4 py-3 lg:flex-row lg:items-center lg:justify-between">
        <div className="flex flex-wrap items-center gap-2">
          <span className="mr-1 text-xs text-muted-foreground">交易所</span>
          {exchanges.map((exchange) => {
            const selected =
              filters.exchanges.length === 0 || filters.exchanges.includes(exchange);
            return (
              <button
                type="button"
                key={exchange}
                onClick={() => toggleExchange(exchange)}
                className={cn(
                  "inline-flex items-center gap-1.5 rounded-md border px-2.5 py-1.5 text-xs transition-all",
                  selected
                    ? "border-primary/25 bg-primary/8 text-foreground"
                    : "border-transparent bg-muted/50 text-muted-foreground opacity-50",
                )}
              >
                <span className={cn("size-1.5 rounded-full", exchangeDot[exchange])} />
                {exchange}
              </button>
            );
          })}
        </div>

        <div className="flex items-center gap-1 rounded-md bg-muted/70 p-1">
          {(
            [
              ["all", "全部费率"],
              ["positive", "正费率"],
              ["negative", "负费率"],
            ] as [RateDirection, string][]
          ).map(([value, label]) => (
            <button
              type="button"
              key={value}
              onClick={() =>
                setFilters((current) => ({ ...current, direction: value }))
              }
              className={cn(
                "rounded px-2.5 py-1 text-xs transition-colors",
                filters.direction === value
                  ? "bg-background font-medium text-foreground shadow-sm"
                  : "text-muted-foreground hover:text-foreground",
              )}
            >
              {label}
            </button>
          ))}
          <span className="px-2 font-mono text-[11px] text-muted-foreground">
            {resultCount} 条
          </span>
        </div>
      </div>
    </section>
  );
}

function DetailPanel({
  opportunity,
  now,
  compact = false,
}: {
  opportunity: FundingOpportunity;
  now: number;
  compact?: boolean;
}) {
  return (
    <div className={cn("flex h-full flex-col", compact && "overflow-y-auto")}>
      <div className="border-b p-4">
        <div className="flex items-start justify-between gap-3">
          <div>
            <div className="flex items-center gap-2">
              <span
                className={cn(
                  "size-2 rounded-full",
                  exchangeDot[opportunity.exchange],
                )}
              />
              <span className="text-xs text-muted-foreground">
                {opportunity.exchange}
              </span>
              <Badge variant="outline" className="h-5 font-mono text-[10px]">
                PERP
              </Badge>
            </div>
            <h2 className="mt-2 font-mono text-xl font-semibold">
              {opportunity.symbol}
            </h2>
          </div>
          <Badge className="gap-1 bg-positive-soft text-positive">
            <Sparkles className="size-3" />
            机会排名
          </Badge>
        </div>

        <div className="mt-5 grid grid-cols-2 gap-3">
          <div>
            <div className="text-[11px] uppercase tracking-wider text-muted-foreground">
              最新价格
            </div>
            <div className="mt-1 font-mono text-lg font-semibold">
              {formatCurrency(opportunity.latestPrice)}
            </div>
            <div className={cn("mt-0.5 font-mono text-xs", rateColor(opportunity.priceChange24h))}>
              {formatPercent(opportunity.priceChange24h)} 今日
            </div>
          </div>
          <div>
            <div className="text-[11px] uppercase tracking-wider text-muted-foreground">
              年化费率
            </div>
            <div
              className={cn(
                "mt-1 font-mono text-lg font-semibold",
                rateColor(opportunity.annualizedRate),
              )}
            >
              {formatPercent(opportunity.annualizedRate, 1)}
            </div>
            <div className="mt-0.5 text-xs text-muted-foreground">
              每 {opportunity.settlementIntervalHours} 小时结算
            </div>
          </div>
        </div>
      </div>

      <div className="grid grid-cols-2 gap-px border-b bg-border">
        <div className="bg-card p-3">
          <div className="flex items-center gap-1 text-[11px] text-muted-foreground">
            <Clock3 className="size-3" />
            下次资金费率
          </div>
          <div className={cn("mt-1.5 font-mono font-semibold", rateColor(opportunity.nextFundingRate))}>
            {formatFundingRate(opportunity.nextFundingRate)}
          </div>
        </div>
        <div className="bg-card p-3">
          <div className="flex items-center gap-1 text-[11px] text-muted-foreground">
            <CalendarClock className="size-3" />
            距离结算
          </div>
          <div className="mt-1.5 font-mono font-semibold">
            {formatSettlementCountdown(opportunity.nextSettlementAt, now)}
          </div>
        </div>
      </div>

      <div className="border-b p-4">
        <div className="flex items-center justify-between">
          <h3 className="text-sm font-medium">历史资金费</h3>
          <div className="flex items-center gap-4 font-mono text-xs">
            <span>
              <span className="mr-1 text-muted-foreground">24H</span>
              <span className={rateColor(opportunity.cumulative24h)}>
                {formatPercent(opportunity.cumulative24h)}
              </span>
            </span>
            <span>
              <span className="mr-1 text-muted-foreground">7D</span>
              <span className={rateColor(opportunity.cumulative7d)}>
                {formatPercent(opportunity.cumulative7d)}
              </span>
            </span>
          </div>
        </div>

        <div className="mt-3 space-y-1">
          {opportunity.fundingHistory.slice(0, 10).map((point) => (
            <div
              key={point.settledAt}
              className="group flex items-center justify-between rounded-md px-2 py-2 font-mono text-xs transition-colors hover:bg-muted/70"
            >
              <span
                className={cn(
                  "rounded px-1.5 py-0.5",
                  rateColor(point.rate),
                  point.rate >= 0 ? "bg-positive-soft" : "bg-negative-soft",
                )}
              >
                {formatFundingRate(point.rate)}
              </span>
              <span className="text-muted-foreground">{point.settledAt}</span>
            </div>
          ))}
        </div>
      </div>

      <div className="mt-auto p-4">
        <div className="rounded-lg border bg-muted/35 p-3">
          <div className="flex items-center justify-between gap-2 font-mono text-xs">
            <span className="text-muted-foreground">{opportunity.index.name}</span>
            <span>{opportunity.index.value.toFixed(5)}</span>
            <span className="text-muted-foreground">{opportunity.index.weight}%</span>
          </div>
          <div className="mt-3 flex items-center justify-between border-t pt-3">
            <div>
              <div className="text-[11px] text-muted-foreground">
                {opportunity.baseAsset} 最新价格
              </div>
              <div className="mt-1 font-mono text-base font-semibold">
                {formatCurrency(opportunity.latestPrice)}
              </div>
            </div>
            <div className="text-right text-[10px] text-muted-foreground">
              <div>更新于</div>
              <div className="mt-1 font-mono">
                {formatDateTime(opportunity.updatedAt)}
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

export function FundingDashboard() {
  const {
    snapshot,
    loading,
    refreshing,
    error,
    lastSuccessAt,
    retry,
  } = useFundingSnapshot();
  const fundingOpportunities = React.useMemo(
    () => snapshot?.data ?? [],
    [snapshot],
  );
  const [filters, setFilters] = React.useState<FundingFilters>(defaultFilters);
  const [sorting, setSorting] = React.useState<SortingState>([
    { id: "annualizedRate", desc: true },
  ]);
  const [selectedId, setSelectedId] = React.useState<string | null>(null);
  const [mobileDetailOpen, setMobileDetailOpen] = React.useState(false);
  const [now, setNow] = React.useState(() => Date.now());

  React.useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1_000);
    return () => window.clearInterval(timer);
  }, []);

  const selected = React.useMemo(
    () =>
      fundingOpportunities.find((item) => item.id === selectedId) ??
      fundingOpportunities[0] ??
      null,
    [fundingOpportunities, selectedId],
  );

  const filteredData = React.useMemo(() => {
    const query = filters.search.trim().toLowerCase();
    return fundingOpportunities.filter((item) => {
      if (
        query &&
        !item.symbol.toLowerCase().includes(query) &&
        !item.exchange.toLowerCase().includes(query)
      ) {
        return false;
      }
      if (item.positionNotional < filters.minPositionNotional) return false;
      if (item.dailyVolume < filters.minDailyVolume) return false;
      if (
        filters.intervalHours !== "all" &&
        item.settlementIntervalHours !== filters.intervalHours
      ) {
        return false;
      }
      if (
        filters.exchanges.length > 0 &&
        !filters.exchanges.includes(item.exchange)
      ) {
        return false;
      }
      if (
        filters.direction === "positive" &&
        (item.nextFundingRate === null || item.nextFundingRate <= 0)
      ) {
        return false;
      }
      if (
        filters.direction === "negative" &&
        (item.nextFundingRate === null || item.nextFundingRate >= 0)
      ) {
        return false;
      }
      return true;
    });
  }, [filters, fundingOpportunities]);

  const columns = React.useMemo<LegacyColumnDef<FundingOpportunity>[]>(
    () => [
      {
        accessorKey: "exchange",
        header: "交易所",
        cell: ({ row }) => (
          <div className="flex items-center gap-2">
            <span
              className={cn(
                "size-1.5 shrink-0 rounded-full",
                exchangeDot[row.original.exchange],
              )}
            />
            <span className="text-xs font-medium">{row.original.exchange}</span>
          </div>
        ),
      },
      {
        accessorKey: "symbol",
        header: "币种",
        cell: ({ row }) => (
          <div>
            <div className="font-mono text-xs font-semibold">
              {row.original.baseAsset}
              <span className="font-normal text-muted-foreground">
                /{row.original.quoteAsset}
              </span>
            </div>
            <div className="mt-0.5 text-[10px] text-muted-foreground">永续合约</div>
          </div>
        ),
      },
      {
        accessorKey: "positionQuantity",
        header: "持仓量",
        cell: ({ row }) => (
          <div
            className="text-right"
            title={`美元名义价值 ${formatCurrency(row.original.positionNotional, true)}`}
          >
            <div className="font-mono text-xs">
              {formatCompactNumber(row.original.positionQuantity)}
            </div>
            <div className="mt-0.5 text-[10px] text-muted-foreground">
              {row.original.baseAsset}
            </div>
          </div>
        ),
      },
      {
        accessorKey: "dailyVolume",
        header: "日成交额",
        cell: ({ row }) => (
          <div className="text-right font-mono text-xs">
            {formatCurrency(row.original.dailyVolume, true)}
          </div>
        ),
      },
      {
        accessorKey: "annualizedRate",
        header: "年化资金费率",
        cell: ({ row }) => (
          <div
            className={cn(
              "text-right font-mono text-xs font-semibold",
              rateColor(row.original.annualizedRate),
            )}
          >
            {formatPercent(row.original.annualizedRate, 1)}
          </div>
        ),
      },
      {
        id: "settlementCountdown",
        accessorFn: (row) => Date.parse(row.nextSettlementAt),
        header: "距离结算",
        cell: ({ row }) => (
          <div className="text-right">
            <div className="font-mono text-xs">
              {formatSettlementCountdown(row.original.nextSettlementAt, now)}
            </div>
            <div className="mt-0.5 text-[10px] text-muted-foreground">
              {row.original.settlementIntervalHours}H 周期
            </div>
          </div>
        ),
      },
      {
        accessorKey: "nextFundingRate",
        header: "预计下次费率",
        cell: ({ row }) => (
          <div
            className={cn(
              "text-right font-mono text-xs font-semibold",
              rateColor(row.original.nextFundingRate),
            )}
          >
            {formatFundingRate(row.original.nextFundingRate)}
          </div>
        ),
      },
      {
        accessorKey: "cumulative24h",
        header: "24H 累计",
        cell: ({ row }) => (
          <div
            className={cn(
              "text-right font-mono text-xs",
              rateColor(row.original.cumulative24h),
            )}
          >
            {formatPercent(row.original.cumulative24h)}
          </div>
        ),
      },
      {
        accessorKey: "cumulative7d",
        header: "7D 累计",
        cell: ({ row }) => (
          <div
            className={cn(
              "text-right font-mono text-xs",
              rateColor(row.original.cumulative7d),
            )}
          >
            {formatPercent(row.original.cumulative7d)}
          </div>
        ),
      },
      {
        id: "open",
        enableSorting: false,
        header: "",
        cell: () => (
          <ChevronRight className="ml-auto size-4 text-muted-foreground" />
        ),
      },
    ],
    [now],
  );

  const table = useLegacyTable({
    data: filteredData,
    columns,
    state: { sorting },
    onSortingChange: setSorting,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
  });

  return (
    <div className="space-y-4">
      <Filters
        filters={filters}
        setFilters={setFilters}
        resultCount={filteredData.length}
      />

      {(loading || refreshing || error || snapshot?.hasStaleSources) && (
        <div
          className={cn(
            "flex flex-wrap items-center justify-between gap-3 rounded-lg border px-3 py-2 text-xs",
            error || snapshot?.hasStaleSources
              ? "border-amber-500/30 bg-amber-500/8 text-amber-700 dark:text-amber-300"
              : "bg-muted/40 text-muted-foreground",
          )}
        >
          <div className="flex items-center gap-2">
            {loading || refreshing ? (
              <LoaderCircle className="size-3.5 animate-spin" />
            ) : (
              <Info className="size-3.5" />
            )}
            <span>
              {loading
                ? "正在加载资金费快照…"
                : error && snapshot
                  ? `刷新失败，继续展示上次成功数据：${error}`
                  : error
                    ? `加载失败：${error}`
                    : snapshot?.hasStaleSources
                      ? "部分交易所数据已过期，当前展示最近可用快照"
                      : "正在刷新资金费快照…"}
            </span>
          </div>
          {error && (
            <Button variant="outline" size="sm" className="h-7 gap-1.5" onClick={retry}>
              <RefreshCw className="size-3" />
              重试
            </Button>
          )}
        </div>
      )}

      <div className="grid min-h-[650px] overflow-hidden rounded-xl border bg-card shadow-sm xl:grid-cols-[minmax(0,1fr)_340px] 2xl:grid-cols-[minmax(0,1fr)_380px]">
        <div className="min-w-0 overflow-x-auto">
          <table className="w-full min-w-[1050px] border-collapse">
            <thead className="sticky top-0 z-10 bg-muted/80 backdrop-blur">
              {table.getHeaderGroups().map((headerGroup) => (
                <tr key={headerGroup.id} className="border-b">
                  {headerGroup.headers.map((header, index) => {
                    const rightAligned = index >= 2 && index <= 8;
                    return (
                      <th
                        key={header.id}
                        className={cn(
                          "h-11 px-3 text-left",
                          rightAligned && "text-right",
                          index === 9 && "w-8",
                        )}
                      >
                        {header.isPlaceholder ? null : header.column.getCanSort() ? (
                          <SortHeader
                            label={String(header.column.columnDef.header)}
                            sorted={header.column.getIsSorted()}
                            onClick={() => header.column.toggleSorting()}
                            align={rightAligned ? "right" : "left"}
                          />
                        ) : (
                          flexRender(
                            header.column.columnDef.header,
                            header.getContext(),
                          )
                        )}
                      </th>
                    );
                  })}
                </tr>
              ))}
            </thead>
            <tbody>
              {table.getRowModel().rows.length > 0 ? (
                table.getRowModel().rows.map((row) => (
                  <tr
                    key={row.id}
                    tabIndex={0}
                    onClick={() => {
                      setSelectedId(row.original.id);
                      setMobileDetailOpen(true);
                    }}
                    onKeyDown={(event) => {
                      if (event.key === "Enter" || event.key === " ") {
                        setSelectedId(row.original.id);
                        setMobileDetailOpen(true);
                      }
                    }}
                    className={cn(
                      "cursor-pointer border-b transition-colors last:border-b-0 hover:bg-muted/45 focus-visible:bg-muted focus-visible:outline-none",
                      selected?.id === row.original.id &&
                        "bg-primary/[0.055] hover:bg-primary/[0.075]",
                    )}
                  >
                    {row.getVisibleCells().map((cell) => (
                      <td key={cell.id} className="h-14 px-3">
                        {flexRender(cell.column.columnDef.cell, cell.getContext())}
                      </td>
                    ))}
                  </tr>
                ))
              ) : (
                <tr>
                  <td colSpan={columns.length} className="h-72 text-center">
                    <div className="mx-auto flex max-w-xs flex-col items-center">
                      {loading ? (
                        <LoaderCircle className="size-7 animate-spin text-muted-foreground" />
                      ) : (
                        <Info className="size-7 text-muted-foreground" />
                      )}
                      <div className="mt-3 text-sm font-medium">
                        {loading
                          ? "正在加载资金费数据"
                          : error && !snapshot
                            ? "暂时无法获取资金费数据"
                            : "没有符合条件的合约"}
                      </div>
                      <div className="mt-1 text-xs text-muted-foreground">
                        {error && !snapshot
                          ? "请检查 API Gateway 后重试"
                          : "降低筛选阈值或重置筛选条件"}
                      </div>
                    </div>
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>

        <aside className="hidden border-l bg-card xl:block">
          {selected ? (
            <DetailPanel opportunity={selected} now={now} />
          ) : (
            <div className="flex h-full items-center justify-center text-xs text-muted-foreground">
              选择合约后查看详情
            </div>
          )}
        </aside>
      </div>

      <div className="flex items-center justify-between px-1 text-[11px] text-muted-foreground">
        <span>实时快照仅供研究参考，不构成投资建议</span>
        <span className="font-mono">
          {snapshot
            ? `SNAPSHOT ${snapshot.meta.snapshotVersion} · ${formatDateTime(snapshot.meta.serverTime)}`
            : lastSuccessAt
              ? `LAST SUCCESS · ${formatDateTime(new Date(lastSuccessAt).toISOString())}`
              : "WAITING FOR API"}
        </span>
      </div>

      {selected && (
        <Sheet open={mobileDetailOpen} onOpenChange={setMobileDetailOpen}>
          <SheetContent className="w-full overflow-y-auto p-0 sm:max-w-md xl:hidden">
            <SheetHeader className="sr-only">
              <SheetTitle>{selected.symbol} 资金费详情</SheetTitle>
              <SheetDescription>
                展示当前合约的费率、历史结算与价格信息
              </SheetDescription>
            </SheetHeader>
            <DetailPanel opportunity={selected} now={now} compact />
          </SheetContent>
        </Sheet>
      )}
    </div>
  );
}
