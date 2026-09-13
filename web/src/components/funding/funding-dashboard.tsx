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
import { Activity } from "react";
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
import { FundingOpportunityRanking } from "@/components/funding/funding-opportunity-ranking";
import { FundingSpreadView } from "@/components/funding/funding-spread-view";
import { WorkspacePanel } from "@/components/layout/responsive";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  annualize24h,
  annualize7d,
  formatCompactNumber,
  formatCurrency,
  formatDateTime,
  formatFundingRate,
  formatPercent,
  formatSettlementCountdown,
  rateColor,
  resolveFundingRate,
} from "@/lib/market-format";
import { fetchFundingHistory } from "@/lib/api/funding";
import {
  canStartFundingTrade,
  contractKindFromVenue,
  formatHistoryWindow,
} from "@/lib/funding-coverage";
import { BasisSpreadPanel } from "@/components/funding/basis-spread-chart";
import { StartTradeButton } from "@/components/funding/start-trade-button";
import { buildArbitragePrefillUrlFromOpportunity } from "@/lib/trading-prefill";
import { useVisibleMeasure } from "@/lib/visible-measure";
import { cn } from "@/lib/utils";
import type {
  Exchange,
  FundingFilters,
  FundingHistoryPoint,
  FundingOpportunity,
  FundingSpread,
  ContractKind,
} from "@/types/market";

type FundingViewMode = "single" | "spread" | "ranking";
type FundingListItem =
  | { kind: "row"; key: string; opportunity: FundingOpportunity }
  | { kind: "detail"; key: string; opportunity: FundingOpportunity };

const DATA_ROW_HEIGHT_PX = 56;
const DETAIL_ROW_HEIGHT_PX = 328;

const exchanges: Exchange[] = [
  "Binance",
  "OKX",
  "Bybit",
  "Bitget",
  "Gate",
  "Hyperliquid",
  "Aster",
  "Lighter",
  "Entropy",
];

const contractKinds: { id: ContractKind; label: string }[] = [
  { id: "crypto", label: "加密永续" },
  { id: "tradifi", label: "TradFi" },
  { id: "hip3", label: "HIP-3" },
];

const defaultFilters: FundingFilters = {
  search: "",
  minPositionNotional: 1_000_000,
  minDailyVolume: 1_000_000,
  intervalHours: "all",
  exchanges: [],
  contractKinds: [],
  direction: "all",
};

const exchangeDot: Record<Exchange, string> = {
  Binance: "bg-amber-400",
  OKX: "bg-foreground",
  Bybit: "bg-orange-500",
  Bitget: "bg-cyan-500",
  Gate: "bg-blue-500",
  Hyperliquid: "bg-emerald-500",
  Aster: "bg-rose-500",
  Lighter: "bg-indigo-500",
  Entropy: "bg-fuchsia-500",
  Polymarket: "bg-violet-500",
};

const fundingHeaderDescriptions: Record<string, string> = {
  annualizedRate: "根据历史已结算资金费线性外推的年化值",
  annualized24h: "将过去 24 小时已结算资金费累计线性外推至一年",
  annualized7d: "将过去 7 天已结算资金费累计线性外推至一年",
};

interface HistoryState {
  data: FundingHistoryPoint[];
  loading: boolean;
  error: string | null;
}

const HISTORY_CACHE_TTL_MS = 5 * 60_000;

function SortHeader({
  label,
  description,
  sorted,
  onClick,
  align = "left",
}: {
  label: string;
  description?: string;
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
      title={description}
      aria-label={description ? `${label}：${description}` : label}
    >
      {label}
      {description && <Info className="size-3 opacity-55" />}
      <Icon className={cn("size-3", !sorted && "opacity-35")} />
    </button>
  );
}

const SettlementCountdown = React.memo(function SettlementCountdown({
  value,
}: {
  value: string;
}) {
  const [now, setNow] = React.useState(() => Date.now());

  React.useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1_000);
    return () => window.clearInterval(timer);
  }, []);

  return <>{formatSettlementCountdown(value, now)}</>;
});

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
  mode,
  onModeChange,
}: {
  filters: FundingFilters;
  setFilters: React.Dispatch<React.SetStateAction<FundingFilters>>;
  resultCount: number;
  mode: FundingViewMode;
  onModeChange: (mode: FundingViewMode) => void;
}) {
  const toggleExchange = (exchange: Exchange) => {
    setFilters((current) => ({
      ...current,
      exchanges: current.exchanges.includes(exchange)
        ? current.exchanges.filter((item) => item !== exchange)
        : [...current.exchanges, exchange],
    }));
  };
  const toggleContractKind = (kind: ContractKind) => {
    setFilters((current) => ({
      ...current,
      contractKinds: current.contractKinds.includes(kind)
        ? current.contractKinds.filter((item) => item !== kind)
        : [...current.contractKinds, kind],
    }));
  };

  return (
    <section className="min-w-0 rounded-xl border bg-card/80 shadow-sm backdrop-blur">
      <div className="flex flex-col gap-4 p-4 xl:flex-row xl:flex-wrap xl:items-end">
        <RangeFilter
          label={mode === "single" ? "持仓名义价值" : "较小腿持仓"}
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
          label={mode === "single" ? "日成交额" : "较小腿日成交额"}
          value={filters.minDailyVolume}
          min={0}
          max={100_000_000}
          step={1_000_000}
          formatter={(value) => formatCurrency(value, true)}
          onChange={(value) =>
            setFilters((current) => ({ ...current, minDailyVolume: value }))
          }
        />
        {mode !== "ranking" && <div className="min-w-44">
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
        </div>}
        <div className="relative min-w-52 flex-[1.25]">
          <label className="mb-2 block text-xs text-muted-foreground">
            {mode === "single" ? "搜索币种" : "搜索币种 / 交易所"}
          </label>
          <Search className="pointer-events-none absolute bottom-2.5 left-3 size-4 text-muted-foreground" />
          <Input
            value={filters.search}
            placeholder={mode === "single" ? "BTC、ETH、USDT..." : "BTC、Binance、OKX..."}
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
          <div
            className="mr-2 inline-flex items-center rounded-lg border bg-muted/60 p-0.5"
            aria-label="资金费视图"
          >
            {(
              [
                ["single", "单所"],
                ["spread", "跨所"],
                ["ranking", "机会排名"],
              ] as const
            ).map(([value, label]) => (
              <button
                type="button"
                key={value}
                onClick={() => onModeChange(value)}
                className={cn(
                  "rounded-md px-3 py-1.5 text-xs font-medium transition-all",
                  mode === value
                    ? "bg-background text-foreground shadow-sm"
                    : "text-muted-foreground hover:text-foreground",
                )}
              >
                {label}
              </button>
            ))}
          </div>
          {mode !== "ranking" && <span className="mr-1 text-xs text-muted-foreground">交易所</span>}
          {mode !== "ranking" && exchanges.map((exchange) => {
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
          {mode !== "ranking" && <span className="ml-2 mr-1 text-xs text-muted-foreground">合约类型</span>}
          {mode !== "ranking" && contractKinds.map(({ id, label }) => {
            const selected =
              filters.contractKinds.length === 0 || filters.contractKinds.includes(id);
            return (
              <button
                type="button"
                key={id}
                onClick={() => toggleContractKind(id)}
                className={cn(
                  "inline-flex items-center rounded-md border px-2.5 py-1.5 text-xs transition-all",
                  selected
                    ? "border-primary/25 bg-primary/8 text-foreground"
                    : "border-transparent bg-muted/50 text-muted-foreground opacity-50",
                )}
              >
                {label}
              </button>
            );
          })}
        </div>

        {mode !== "single" && (
          <div className="flex items-center gap-1 rounded-md bg-muted/70 p-1">
            <span className="px-2 font-mono text-[11px] text-muted-foreground">
              {resultCount} 条
            </span>
          </div>
        )}
      </div>
    </section>
  );
}

const DetailPanel = React.memo(function DetailPanel({
  opportunity,
  history,
  historyLoading,
  historyError,
  compact = false,
}: {
  opportunity: FundingOpportunity;
  history: FundingHistoryPoint[];
  historyLoading: boolean;
  historyError: string | null;
  compact?: boolean;
}) {
  const displayedFundingRate = resolveFundingRate(
    opportunity.nextFundingRate,
    opportunity.currentFundingRate,
  );
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
          <div className="flex shrink-0 flex-col items-end gap-2">
            <Badge className="gap-1 bg-positive-soft text-positive">
              <Sparkles className="size-3" />
              机会排名
            </Badge>
            {canStartFundingTrade(opportunity.exchange, "basis", {
              venueContractType: opportunity.venueContractType,
              exchangeSymbol: opportunity.exchangeSymbol,
            }) && (
              <StartTradeButton
                href={buildArbitragePrefillUrlFromOpportunity(opportunity)}
              />
            )}
          </div>
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
            <div
              className="text-[11px] uppercase tracking-wider text-muted-foreground"
              title="根据历史已结算资金费线性外推的年化值"
            >
              历史年化费率
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
            资金费率
          </div>
          <div className={cn("mt-1.5 font-mono font-semibold", rateColor(displayedFundingRate))}>
            {formatFundingRate(displayedFundingRate)}
          </div>
        </div>
        <div className="bg-card p-3">
          <div className="flex items-center gap-1 text-[11px] text-muted-foreground">
            <CalendarClock className="size-3" />
            距离结算
          </div>
          <div className="mt-1.5 font-mono font-semibold">
            <SettlementCountdown value={opportunity.nextSettlementAt} />
          </div>
        </div>
      </div>

      <div className="border-b p-4">
        <div className="flex items-center justify-between">
          <h3 className="text-sm font-medium">历史资金费</h3>
          <div className="flex items-center gap-4 font-mono text-xs">
            <span>
              <span
                className="mr-1 text-muted-foreground"
                title="将过去 24 小时已结算资金费累计线性外推至一年"
              >
                24H 年化
              </span>
              <span className={rateColor(annualize24h(opportunity.cumulative24h))}>
                {formatHistoryWindow(
                  opportunity.history24hComplete,
                  annualize24h(opportunity.cumulative24h),
                  (value) => formatPercent(value, 1),
                )}
              </span>
            </span>
            <span>
              <span
                className="mr-1 text-muted-foreground"
                title="将过去 7 天已结算资金费累计线性外推至一年"
              >
                7D 年化
              </span>
              <span className={rateColor(annualize7d(opportunity.cumulative7d))}>
                {formatHistoryWindow(
                  opportunity.history7dComplete,
                  annualize7d(opportunity.cumulative7d),
                  (value) => formatPercent(value, 1),
                )}
              </span>
            </span>
          </div>
        </div>

        <div className="mt-3 space-y-1">
          {historyLoading ? (
            <div className="flex h-20 items-center justify-center text-xs text-muted-foreground">
              <LoaderCircle className="mr-2 size-3.5 animate-spin" />
              加载历史资金费
            </div>
          ) : historyError ? (
            <div className="flex h-20 items-center justify-center text-xs text-amber-600 dark:text-amber-300">
              {historyError}
            </div>
          ) : history.length === 0 ? (
            <div className="flex h-20 items-center justify-center text-xs text-muted-foreground">
              暂无历史资金费
            </div>
          ) : (
            history.slice(0, 10).map((point) => (
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
            ))
          )}
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
});

export function FundingDashboard() {
  const {
    snapshot,
    spreadSnapshot,
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
  const fundingSpreads = React.useMemo(
    () => spreadSnapshot?.data ?? [],
    [spreadSnapshot],
  );
  const [mode, setMode] = React.useState<FundingViewMode>("single");
  const [visitedModes, setVisitedModes] = React.useState<Set<FundingViewMode>>(
    () => new Set<FundingViewMode>(["single"]),
  );
  const selectMode = React.useCallback((next: FundingViewMode) => {
    setVisitedModes((current) => {
      if (current.has(next)) return current;
      const nextSet = new Set(current);
      nextSet.add(next);
      return nextSet;
    });
    setMode(next);
  }, []);
  const [rankingResultCount, setRankingResultCount] = React.useState(0);
  const [filters, setFilters] = React.useState<FundingFilters>(defaultFilters);
  const [sorting, setSorting] = React.useState<SortingState>([
    { id: "annualizedRate", desc: true },
  ]);
  const [selectedId, setSelectedId] = React.useState<string | null>(null);

  const selectOpportunity = React.useCallback(
    (id: string) => {
      setSelectedId((current) => (current === id ? null : id));
    },
    [],
  );

  const selected = React.useMemo(
    () => fundingOpportunities.find((item) => item.id === selectedId) ?? null,
    [fundingOpportunities, selectedId],
  );
  const historyCache = React.useRef(
    new Map<string, { data: FundingHistoryPoint[]; loadedAt: number }>(),
  );
  const [historyState, setHistoryState] = React.useState<HistoryState>({
    data: [],
    loading: false,
    error: null,
  });

  React.useEffect(() => {
    if (!selected) {
      setHistoryState({ data: [], loading: false, error: null });
      return;
    }
    if (selected.fundingHistory.length > 0) {
      setHistoryState({
        data: selected.fundingHistory,
        loading: false,
        error: null,
      });
      return;
    }
    const cached = historyCache.current.get(selected.id);
    if (cached && Date.now() - cached.loadedAt < HISTORY_CACHE_TTL_MS) {
      setHistoryState({ data: cached.data, loading: false, error: null });
      return;
    }
    const controller = new AbortController();
    setHistoryState((current) => ({
      data: current.data,
      loading: true,
      error: null,
    }));
    void fetchFundingHistory(
      selected.exchange,
      selected.exchangeSymbol,
      controller.signal,
    )
      .then((data) => {
        historyCache.current.set(selected.id, { data, loadedAt: Date.now() });
        setHistoryState({ data, loading: false, error: null });
      })
      .catch((reason) => {
        if (controller.signal.aborted) return;
        setHistoryState({
          data: [],
          loading: false,
          error: reason instanceof Error ? reason.message : "历史资金费加载失败",
        });
      });
    return () => controller.abort();
  }, [selected]);

  const filteredData = React.useMemo(() => {
    const query = filters.search.trim().toLowerCase();
    return fundingOpportunities.filter((item) => {
      const displayedRate = resolveFundingRate(
        item.nextFundingRate,
        item.currentFundingRate,
      );
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
      if (filters.contractKinds.length > 0) {
        const kind = contractKindFromVenue(
          item.venueContractType,
          item.exchangeSymbol,
        );
        if (!filters.contractKinds.includes(kind)) {
          return false;
        }
      }
      if (
        filters.direction === "positive" &&
        (displayedRate === null || displayedRate <= 0)
      ) {
        return false;
      }
      if (
        filters.direction === "negative" &&
        (displayedRate === null || displayedRate >= 0)
      ) {
        return false;
      }
      return true;
    });
  }, [filters, fundingOpportunities]);

  const filteredSpreads = React.useMemo(() => {
    const query = filters.search.trim().toLowerCase();
    return fundingSpreads.filter((item: FundingSpread) => {
      if (
        query &&
        !item.symbol.toLowerCase().includes(query) &&
        !item.longLeg.exchange.toLowerCase().includes(query) &&
        !item.shortLeg.exchange.toLowerCase().includes(query)
      ) {
        return false;
      }
      if (item.minPositionNotional < filters.minPositionNotional) return false;
      if (item.minDailyVolume < filters.minDailyVolume) return false;
      if (filters.exchanges.length > 0) {
        if (filters.exchanges.length < 2) return false;
        if (
          !filters.exchanges.includes(item.longLeg.exchange) ||
          !filters.exchanges.includes(item.shortLeg.exchange)
        ) {
          return false;
        }
      }
      if (
        filters.intervalHours !== "all" &&
        item.longLeg.settlementIntervalHours !== filters.intervalHours &&
        item.shortLeg.settlementIntervalHours !== filters.intervalHours
      ) {
        return false;
      }
      if (filters.contractKinds.length > 0) {
        const longKind = contractKindFromVenue(
          item.longLeg.venueContractType,
          item.longLeg.exchangeSymbol,
        );
        const shortKind = contractKindFromVenue(
          item.shortLeg.venueContractType,
          item.shortLeg.exchangeSymbol,
        );
        if (
          !filters.contractKinds.includes(longKind) ||
          !filters.contractKinds.includes(shortKind)
        ) {
          return false;
        }
      }
      return true;
    });
  }, [filters, fundingSpreads]);

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
        accessorKey: "positionNotional",
        header: "持仓市值 (USDT)",
        cell: ({ row }) => (
          <div
            className="text-right"
            title={`${formatCompactNumber(row.original.positionQuantity)} ${row.original.baseAsset}`}
          >
            <div className="font-mono text-xs">
              {formatCurrency(row.original.positionNotional, true)}
            </div>
            <div className="mt-0.5 text-[10px] text-muted-foreground">
              USDT
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
        header: "历史年化费率",
        cell: ({ row }) => (
          <div
            className={cn(
              "text-right font-mono text-xs font-semibold",
              rateColor(row.original.annualizedRate),
            )}
          >
            {formatHistoryWindow(
              row.original.history7dComplete,
              row.original.annualizedRate,
              (value) => formatPercent(value, 1),
            )}
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
              <SettlementCountdown value={row.original.nextSettlementAt} />
            </div>
            <div className="mt-0.5 text-[10px] text-muted-foreground">
              {row.original.settlementIntervalHours}H 周期
            </div>
          </div>
        ),
      },
      {
        id: "displayedFundingRate",
        accessorFn: (row) =>
          resolveFundingRate(row.nextFundingRate, row.currentFundingRate),
        header: "资金费率",
        cell: ({ row }) => {
          const displayedRate = resolveFundingRate(
            row.original.nextFundingRate,
            row.original.currentFundingRate,
          );
          return (
            <div
              className={cn(
                "text-right font-mono text-xs font-semibold",
                rateColor(displayedRate),
              )}
            >
              {formatFundingRate(displayedRate)}
            </div>
          );
        },
      },
      {
        id: "annualized24h",
        accessorFn: (row) => annualize24h(row.cumulative24h),
        header: "24H 窗口年化",
        cell: ({ row }) => (
          <div
            className={cn(
              "text-right font-mono text-xs",
              rateColor(annualize24h(row.original.cumulative24h)),
            )}
          >
            {formatHistoryWindow(
              row.original.history24hComplete,
              annualize24h(row.original.cumulative24h),
              (value) => formatPercent(value, 1),
            )}
          </div>
        ),
      },
      {
        id: "annualized7d",
        accessorFn: (row) => annualize7d(row.cumulative7d),
        header: "7D 窗口年化",
        cell: ({ row }) => (
          <div
            className={cn(
              "text-right font-mono text-xs",
              rateColor(annualize7d(row.original.cumulative7d)),
            )}
          >
            {formatHistoryWindow(
              row.original.history7dComplete,
              annualize7d(row.original.cumulative7d),
              (value) => formatPercent(value, 1),
            )}
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
    [],
  );

  const table = useLegacyTable({
    data: filteredData,
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
    const items: FundingListItem[] = [];
    for (const row of rows) {
      items.push({
        kind: "row",
        key: `row-${row.original.id}`,
        opportunity: row.original,
      });
      if (selected?.id === row.original.id) {
        items.push({
          kind: "detail",
          key: `detail-${row.original.id}`,
          opportunity: row.original,
        });
      }
    }
    return items;
  }, [rows, selected]);
  const tableContainerRef = React.useRef<HTMLDivElement>(null);
  // TanStack Virtual intentionally exposes imperative functions that React Compiler skips.
  // eslint-disable-next-line react-hooks/incompatible-library
  const rowVirtualizer = useVirtualizer({
    count: listItems.length,
    getScrollElement: () => tableContainerRef.current,
    estimateSize: (index) =>
      listItems[index]?.kind === "detail" ? DETAIL_ROW_HEIGHT_PX : DATA_ROW_HEIGHT_PX,
    overscan: 10,
    getItemKey: (index) => listItems[index]?.key ?? index,
  });
  useVisibleMeasure(
    () => {
      rowVirtualizer.measure();
    },
    mode === "single",
  );
  const virtualRows = rowVirtualizer.getVirtualItems();
  const paddingTop = virtualRows.length > 0 ? virtualRows[0].start : 0;
  const paddingBottom =
    virtualRows.length > 0
      ? rowVirtualizer.getTotalSize() - virtualRows[virtualRows.length - 1].end
      : 0;

  return (
    <div className="relative min-w-0 space-y-4">
      <Filters
        filters={filters}
        setFilters={setFilters}
        resultCount={
          mode === "single"
            ? filteredData.length
            : mode === "spread"
              ? filteredSpreads.length
              : rankingResultCount
        }
        mode={mode}
        onModeChange={selectMode}
      />

      {mode !== "ranking" && refreshing && snapshot && !error && (
        <div
          className="pointer-events-none absolute right-4 top-3 z-20 inline-flex items-center gap-1.5 rounded-md border bg-background/90 px-2 py-1 text-[11px] text-muted-foreground shadow-sm backdrop-blur"
          role="status"
        >
          <LoaderCircle className="size-3 animate-spin" />
          后台刷新
        </div>
      )}

      {mode !== "ranking" && (loading || error || snapshot?.hasStaleSources) && (
        <div
          className={cn(
            "flex flex-wrap items-center justify-between gap-3 rounded-lg border px-3 py-2 text-xs",
            error || snapshot?.hasStaleSources
              ? "border-amber-500/30 bg-amber-500/8 text-amber-700 dark:text-amber-300"
              : "bg-muted/40 text-muted-foreground",
          )}
        >
          <div className="flex items-center gap-2">
            {loading ? (
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

      {visitedModes.has("single") ? (
      <Activity mode={mode === "single" ? "visible" : "hidden"}>
      <WorkspacePanel
        className="grid-cols-[minmax(0,1fr)_380px]"
      >
        <div
          ref={tableContainerRef}
          data-wide-table-scroll
          className="min-w-0 overflow-auto"
        >
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
                            description={fundingHeaderDescriptions[header.id]}
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
              {rows.length > 0 ? (
                <>
                  {paddingTop > 0 && (
                    <tr aria-hidden="true">
                      <td colSpan={columns.length} style={{ height: paddingTop }} />
                    </tr>
                  )}
                  {virtualRows.map((virtualRow) => {
                    const item = listItems[virtualRow.index];
                    if (!item) return null;
                    if (item.kind === "detail") {
                      return (
                        <tr
                          key={item.key}
                          className="border-b bg-muted/20 last:border-b-0"
                        >
                          <td colSpan={columns.length} className="px-4 py-3">
                            <BasisSpreadPanel
                              venue={item.opportunity.exchange}
                              baseAsset={item.opportunity.baseAsset}
                              quoteAsset={item.opportunity.quoteAsset}
                            />
                          </td>
                        </tr>
                      );
                    }
                    const row = rowsById.get(item.opportunity.id);
                    if (!row) return null;
                    return (
                      <tr
                        key={item.key}
                        tabIndex={0}
                        onClick={() => selectOpportunity(item.opportunity.id)}
                        onKeyDown={(event) => {
                          if (event.key === "Enter" || event.key === " ") {
                            event.preventDefault();
                            selectOpportunity(item.opportunity.id);
                          }
                        }}
                        className={cn(
                          "cursor-pointer border-b transition-colors last:border-b-0 hover:bg-muted/45 focus-visible:bg-muted focus-visible:outline-none",
                          selected?.id === item.opportunity.id &&
                            "bg-primary/[0.055] hover:bg-primary/[0.075]",
                        )}
                      >
                        {row.getVisibleCells().map((cell) => (
                          <td key={cell.id} className="h-14 px-3">
                            {flexRender(cell.column.columnDef.cell, cell.getContext())}
                          </td>
                        ))}
                      </tr>
                    );
                  })}
                  {paddingBottom > 0 && (
                    <tr aria-hidden="true">
                      <td colSpan={columns.length} style={{ height: paddingBottom }} />
                    </tr>
                  )}
                </>
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

        <aside className="border-l bg-card">
          {selected ? (
            <DetailPanel
              opportunity={selected}
              history={historyState.data}
              historyLoading={historyState.loading}
              historyError={historyState.error}
            />
          ) : (
            <div className="flex h-full items-center justify-center text-xs text-muted-foreground">
              选择合约后查看详情
            </div>
          )}
        </aside>
      </WorkspacePanel>
      </Activity>
      ) : null}
      {visitedModes.has("spread") ? (
        <Activity mode={mode === "spread" ? "visible" : "hidden"}>
          <FundingSpreadView data={filteredSpreads} loading={loading} />
        </Activity>
      ) : null}
      {visitedModes.has("ranking") ? (
        <Activity mode={mode === "ranking" ? "visible" : "hidden"}>
          <FundingOpportunityRanking
            filters={filters}
            onResultCountChange={setRankingResultCount}
          />
        </Activity>
      ) : null}

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

    </div>
  );
}
