"use client";

import * as React from "react";
import {
  Activity,
  ArrowLeftRight,
  CheckCircle2,
  CircleAlert,
  Eye,
  Gauge,
  ListFilter,
  Play,
  RefreshCw,
  ShieldCheck,
  TrendingUp,
} from "lucide-react";

import {
  fetchTradingAccounts,
  groupAccountsByProduct,
  type TradingAccount,
} from "@/lib/api/accounts";
import {
  closeArbitrageCombination,
  createArbitrageCombination,
  fetchArbitrageCombination,
  fetchArbitrageCombinations,
  type ArbitrageCombination,
  type ArbitrageCombinationDetail,
  type ArbitrageExecutionMode,
  type ArbitragePreferredLeg,
  type ArbitrageView,
} from "@/lib/api/arbitrage";
import {
  CEX_EXCHANGES,
  fetchTraderInstruments,
  type TraderContractType,
  type TraderInstrument,
} from "@/lib/api/trader";
import { fetchFundingRates } from "@/lib/api/funding";
import type { FundingOpportunity } from "@/types/market";
import {
  fetchBasisSpreadHistory,
  type BasisSpreadHistory,
  type BasisSpreadRange,
} from "@/lib/api/spread";
import { annualize24h, annualize7d, formatPercent, rateColor } from "@/lib/market-format";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

const selectClassName =
  "h-8 w-full rounded-lg border border-input bg-background px-2.5 text-sm outline-none transition-colors focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50";

const periods: BasisSpreadRange[] = ["1h", "4h", "8h", "24h", "7d"];
const SPREAD_REFRESH_MS = 30_000;
const SPREAD_CACHE_TTL_MS = 30_000;

type BasisSpreadQuery = {
  venue: string;
  compareVenue?: string;
  baseAsset: string;
  quoteAsset: string;
  formula: string;
};

type SpreadCacheEntry = {
  history: BasisSpreadHistory;
  etag: string | null;
  loadedAt: number;
};

export function resolveBasisSpreadQuery(
  legA: TraderInstrument | null,
  legB: TraderInstrument | null,
): BasisSpreadQuery | null {
  if (
    !legA ||
    !legB ||
    legA.baseAsset !== legB.baseAsset ||
    legA.quoteAsset !== legB.quoteAsset
  ) {
    return null;
  }
  const venueA = legA.exchange.toLowerCase();
  const venueB = legB.exchange.toLowerCase();
  const symbol = `${legA.baseAsset}/${legA.quoteAsset}`;
  if (
    venueA === venueB &&
    new Set([legA.contractType, legB.contractType]).size === 2
  ) {
    return {
      venue: venueA,
      baseAsset: legA.baseAsset,
      quoteAsset: legA.quoteAsset,
      formula: `Perpetual Ask / Spot Ask - 1 · ${symbol}`,
    };
  }
  if (
    venueA !== venueB &&
    legA.contractType === "perpetual" &&
    legB.contractType === "perpetual"
  ) {
    return {
      venue: venueB,
      compareVenue: venueA,
      baseAsset: legA.baseAsset,
      quoteAsset: legA.quoteAsset,
      formula: `${venueB.toUpperCase()} Ask / ${venueA.toUpperCase()} Ask - 1 · ${symbol}`,
    };
  }
  return null;
}

export type ArbitrageYieldMode = "basis" | "cross";

export function resolveArbitrageYieldMode(
  legA: TraderInstrument | null,
  legB: TraderInstrument | null,
): ArbitrageYieldMode | null {
  const query = resolveBasisSpreadQuery(legA, legB);
  if (!query) return null;
  return query.compareVenue ? "cross" : "basis";
}

export function annualizeBasisAvgBps(avgBps: number, range: "24h" | "7d"): number {
  const days = range === "24h" ? 1 : 7;
  return (avgBps / 10_000) * (365 / days) * 100;
}

export function matchFundingOpportunity(
  items: FundingOpportunity[],
  instrument: TraderInstrument,
): FundingOpportunity | undefined {
  const exchange = instrument.exchange.toLowerCase();
  const symbol = instrument.exchangeSymbol.toLowerCase();
  return (
    items.find(
      (item) =>
        item.exchange.toLowerCase() === exchange &&
        item.exchangeSymbol.toLowerCase() === symbol,
    ) ??
    items.find(
      (item) =>
        item.exchange.toLowerCase() === exchange &&
        item.baseAsset === instrument.baseAsset &&
        item.quoteAsset === instrument.quoteAsset,
    )
  );
}

export function crossExchangeWindowYield(
  legA: FundingOpportunity | undefined,
  legB: FundingOpportunity | undefined,
): { value24h: number; value7d: number } | null {
  if (!legA || !legB) return null;
  return {
    value24h: annualize24h(legB.cumulative24h - legA.cumulative24h),
    value7d: annualize7d(legB.cumulative7d - legA.cumulative7d),
  };
}

function spreadCacheKey(query: BasisSpreadQuery, range: BasisSpreadRange): string {
  return [
    query.venue,
    query.compareVenue ?? "",
    query.baseAsset,
    query.quoteAsset,
    range,
  ].join("|");
}

function formatBps(value: number): string {
  return `${value > 0 ? "+" : ""}${value.toFixed(2)} bps`;
}

function formatCoverage(value: number): string {
  return `${(value * 100).toFixed(1)}%`;
}

function formatDecimal(value: string, suffix = ""): string {
  const parsed = Number(value);
  if (!Number.isFinite(parsed)) return value || "—";
  return `${parsed.toLocaleString(undefined, { maximumFractionDigits: 8 })}${suffix}`;
}

function formatSpread(value: string | null): string {
  if (value == null) return "—";
  const parsed = Number(value);
  return Number.isFinite(parsed) ? `${parsed > 0 ? "+" : ""}${parsed.toFixed(2)} bps` : "—";
}

function formatTime(value: string): string {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

function useAccountInstruments(
  accountID: number | null,
  contractType: TraderContractType,
) {
  const [result, setResult] = React.useState<{
    accountID: number | null;
    contractType: TraderContractType | null;
    items: TraderInstrument[];
  }>({ accountID: null, contractType: null, items: [] });

  React.useEffect(() => {
    if (accountID == null) return;
    let active = true;
    void fetchTraderInstruments(accountID, contractType)
      .then((result) => {
        if (active) setResult({ accountID, contractType, items: result });
      })
      .catch(() => {
        if (active) setResult({ accountID, contractType, items: [] });
      });
    return () => {
      active = false;
    };
  }, [accountID, contractType]);

  return {
    items:
      result.accountID === accountID && result.contractType === contractType
        ? result.items
        : [],
    loading:
      accountID != null &&
      (result.accountID !== accountID || result.contractType !== contractType),
  };
}

export function ArbitrageTradingView() {
  const [accounts, setAccounts] = React.useState<TradingAccount[]>([]);
  const [accountsError, setAccountsError] = React.useState("");
  const [productName, setProductName] = React.useState("");
  const [legAExchange, setLegAExchange] = React.useState("");
  const [legBExchange, setLegBExchange] = React.useState("");
  const [legAAccountID, setLegAAccountID] = React.useState<number | null>(null);
  const [legBAccountID, setLegBAccountID] = React.useState<number | null>(null);
  const [legAInstrumentID, setLegAInstrumentID] = React.useState<number | null>(null);
  const [legBInstrumentID, setLegBInstrumentID] = React.useState<number | null>(null);
  const [legAContractType, setLegAContractType] =
    React.useState<TraderContractType>("perpetual");
  const [legBContractType, setLegBContractType] =
    React.useState<TraderContractType>("perpetual");
  const [period, setPeriod] = React.useState<BasisSpreadRange>("24h");
  const [targetNotional, setTargetNotional] = React.useState("10000");
  const [preferredLeg, setPreferredLeg] = React.useState<ArbitragePreferredLeg>("a");
  const [executionMode, setExecutionMode] =
    React.useState<ArbitrageExecutionMode>("maker_then_hedge");
  const [askThreshold, setAskThreshold] = React.useState("12");
  const [bidThreshold, setBidThreshold] = React.useState("-8");
  const [orderNotional, setOrderNotional] = React.useState("500");
  const [maxDeltaNotional, setMaxDeltaNotional] = React.useState("100");
  const [message, setMessage] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [combinationView, setCombinationView] = React.useState<ArbitrageView>("running");
  const [pages, setPages] = React.useState<Record<ArbitrageView, ArbitrageCombination[]>>({
    running: [],
    closed: [],
  });
  const [counts, setCounts] = React.useState<Record<ArbitrageView, number>>({
    running: 0,
    closed: 0,
  });
  const [listError, setListError] = React.useState("");
  const [listLoading, setListLoading] = React.useState(true);
  const [detailID, setDetailID] = React.useState<string | null>(null);
  const [detail, setDetail] = React.useState<ArbitrageCombinationDetail | null>(null);
  const [detailLoading, setDetailLoading] = React.useState(false);
  const [closingID, setClosingID] = React.useState<string | null>(null);
  const [spreadHistory, setSpreadHistory] = React.useState<BasisSpreadHistory | null>(null);
  const [spreadLoading, setSpreadLoading] = React.useState(false);
  const [spreadError, setSpreadError] = React.useState("");
  const [spreadRetry, setSpreadRetry] = React.useState(0);
  const [yieldMetrics, setYieldMetrics] = React.useState<{
    value24h: number | null;
    value7d: number | null;
  }>({ value24h: null, value7d: null });
  const spreadCache = React.useRef(new Map<string, SpreadCacheEntry>());
  const fundingCache = React.useRef<{
    items: FundingOpportunity[];
    etag: string | null;
  }>({ items: [], etag: null });

  const cexAccounts = React.useMemo(
    () => accounts.filter((item) => CEX_EXCHANGES.has(item.exchangeSlug)),
    [accounts],
  );
  const products = React.useMemo(() => groupAccountsByProduct(cexAccounts), [cexAccounts]);
  const productAccounts = React.useMemo(
    () => products.find((item) => item.productName === productName)?.accounts ?? [],
    [productName, products],
  );
  const exchanges = React.useMemo(
    () => Array.from(new Map(productAccounts.map((item) => [item.exchangeSlug, item.exchange]))),
    [productAccounts],
  );
  const legAAccounts = productAccounts.filter((item) => item.exchangeSlug === legAExchange);
  const legBAccounts = productAccounts.filter((item) => item.exchangeSlug === legBExchange);
  const { items: legAInstruments, loading: legALoading } =
    useAccountInstruments(legAAccountID, legAContractType);
  const { items: legBInstruments, loading: legBLoading } =
    useAccountInstruments(legBAccountID, legBContractType);
  const legAInstrument =
    legAInstruments.find((item) => item.id === legAInstrumentID) ??
    legAInstruments[0] ??
    null;
  const legBInstrument =
    legBInstruments.find((item) => item.id === legBInstrumentID) ??
    legBInstruments[0] ??
    null;
  const visibleLegAInstrumentID = legAInstrument?.id ?? null;
  const visibleLegBInstrumentID = legBInstrument?.id ?? null;
  const spreadQuery = React.useMemo(
    () => resolveBasisSpreadQuery(legAInstrument, legBInstrument),
    [legAInstrument, legBInstrument],
  );
  const yieldMode = React.useMemo(
    () => resolveArbitrageYieldMode(legAInstrument, legBInstrument),
    [legAInstrument, legBInstrument],
  );

  React.useEffect(() => {
    void fetchTradingAccounts()
      .then((items) => {
        const filtered = items.filter((item) => CEX_EXCHANGES.has(item.exchangeSlug));
        const firstProduct = groupAccountsByProduct(filtered)[0];
        const first = firstProduct?.accounts[0];
        const second =
          firstProduct?.accounts.find((item) => item.exchangeSlug !== first?.exchangeSlug) ??
          firstProduct?.accounts[1] ??
          first;
        setAccounts(filtered);
        setProductName(firstProduct?.productName ?? "");
        setLegAExchange(first?.exchangeSlug ?? "");
        setLegAAccountID(first?.id ?? null);
        setLegBExchange(second?.exchangeSlug ?? "");
        setLegBAccountID(second?.id ?? null);
      })
      .catch((error: unknown) => {
        setAccountsError(error instanceof Error ? error.message : "账户加载失败");
      });
  }, []);

  const refreshCombinations = React.useCallback(async (signal?: AbortSignal) => {
    const [running, closed] = await Promise.all([
      fetchArbitrageCombinations("running", { limit: 50, signal }),
      fetchArbitrageCombinations("closed", { limit: 50, signal }),
    ]);
    setPages({ running: running.items, closed: closed.items });
    setCounts({ running: running.total, closed: closed.total });
    setListError("");
    setListLoading(false);
  }, []);

  React.useEffect(() => {
    let controller: AbortController | null = null;
    let stopped = false;
    const poll = async () => {
      controller?.abort();
      controller = new AbortController();
      try {
        await refreshCombinations(controller.signal);
      } catch (error) {
        if (!controller.signal.aborted && !stopped) {
          setListError(error instanceof Error ? error.message : "套利组合加载失败");
          setListLoading(false);
        }
      }
    };
    void poll();
    const timer = window.setInterval(() => void poll(), 1500);
    return () => {
      stopped = true;
      window.clearInterval(timer);
      controller?.abort();
    };
  }, [refreshCombinations]);

  const pairError = React.useMemo(() => {
    if (!legAInstrument || !legBInstrument) return "请选择两侧永续合约";
    if (legAInstrument.baseAsset !== legBInstrument.baseAsset) {
      return "Leg A 与 Leg B 的基础资产必须一致";
    }
    if (legAInstrument.quoteAsset !== legBInstrument.quoteAsset) {
      return "Leg A 与 Leg B 的 Quote Asset 必须一致";
    }
    return "";
  }, [legAInstrument, legBInstrument]);
  const visibleSpreadHistory =
    spreadHistory &&
    spreadQuery &&
    spreadHistory.venue.toLowerCase() === spreadQuery.venue &&
    (spreadHistory.compareVenue?.toLowerCase() ?? "") === (spreadQuery.compareVenue ?? "") &&
    spreadHistory.baseAsset === spreadQuery.baseAsset &&
    spreadHistory.quoteAsset === spreadQuery.quoteAsset &&
    spreadHistory.range === period
      ? spreadHistory
      : null;
  const spreadSummary =
    visibleSpreadHistory?.availability === "available"
      ? visibleSpreadHistory.summary
      : null;

  React.useEffect(() => {
    if (!spreadQuery) {
      return;
    }
    const key = spreadCacheKey(spreadQuery, period);
    const controller = new AbortController();
    let active = true;
    const load = async (force = false) => {
      await Promise.resolve();
      const cached = spreadCache.current.get(key);
      if (!force && cached && Date.now() - cached.loadedAt < SPREAD_CACHE_TTL_MS) {
        setSpreadHistory(cached.history);
        setSpreadError("");
        setSpreadLoading(false);
        return;
      }
      setSpreadLoading(true);
      setSpreadError("");
      try {
        const result = await fetchBasisSpreadHistory(
          spreadQuery.venue,
          spreadQuery.baseAsset,
          spreadQuery.quoteAsset,
          period,
          cached?.etag ?? null,
          controller.signal,
          spreadQuery.compareVenue,
        );
        if (!active) return;
        if (result.status === "updated") {
          spreadCache.current.set(key, {
            history: result.history,
            etag: result.etag,
            loadedAt: Date.now(),
          });
          setSpreadHistory(result.history);
        } else if (cached) {
          spreadCache.current.set(key, {
            ...cached,
            etag: result.etag,
            loadedAt: Date.now(),
          });
          setSpreadHistory(cached.history);
        }
        setSpreadError("");
      } catch (error) {
        if (!controller.signal.aborted && active) {
          setSpreadError(error instanceof Error ? error.message : "价差走势加载失败");
        }
      } finally {
        if (active) setSpreadLoading(false);
      }
    };
    void load();
    const timer = window.setInterval(() => void load(true), SPREAD_REFRESH_MS);
    return () => {
      active = false;
      controller.abort();
      window.clearInterval(timer);
    };
  }, [period, spreadQuery, spreadRetry]);

  React.useEffect(() => {
    if (!spreadQuery || !legAInstrument || !legBInstrument || !yieldMode) {
      setYieldMetrics({ value24h: null, value7d: null });
      return;
    }
    const controller = new AbortController();
    let active = true;
    const loadCachedRange = async (range: "24h" | "7d") => {
      const key = spreadCacheKey(spreadQuery, range);
      const cached = spreadCache.current.get(key);
      if (cached && Date.now() - cached.loadedAt < SPREAD_CACHE_TTL_MS) {
        return cached.history;
      }
      const result = await fetchBasisSpreadHistory(
        spreadQuery.venue,
        spreadQuery.baseAsset,
        spreadQuery.quoteAsset,
        range,
        cached?.etag ?? null,
        controller.signal,
        spreadQuery.compareVenue,
      );
      if (result.status === "updated") {
        spreadCache.current.set(key, {
          history: result.history,
          etag: result.etag,
          loadedAt: Date.now(),
        });
        return result.history;
      }
      if (cached) {
        spreadCache.current.set(key, {
          ...cached,
          etag: result.etag,
          loadedAt: Date.now(),
        });
        return cached.history;
      }
      return null;
    };
    const load = async () => {
      try {
        if (yieldMode === "cross") {
          const result = await fetchFundingRates(fundingCache.current.etag, controller.signal);
          if (!active) return;
          if (result.status === "updated") {
            fundingCache.current = { items: result.snapshot.data, etag: result.etag };
          }
          const yield_ = crossExchangeWindowYield(
            matchFundingOpportunity(fundingCache.current.items, legAInstrument),
            matchFundingOpportunity(fundingCache.current.items, legBInstrument),
          );
          if (active) {
            setYieldMetrics(yield_ ?? { value24h: null, value7d: null });
          }
          return;
        }
        const [history24h, history7d] = await Promise.all([
          loadCachedRange("24h"),
          loadCachedRange("7d"),
        ]);
        if (!active) return;
        setYieldMetrics({
          value24h:
            history24h?.availability === "available"
              ? annualizeBasisAvgBps(history24h.summary.avgBps, "24h")
              : null,
          value7d:
            history7d?.availability === "available"
              ? annualizeBasisAvgBps(history7d.summary.avgBps, "7d")
              : null,
        });
      } catch {
        if (!controller.signal.aborted && active) {
          setYieldMetrics({ value24h: null, value7d: null });
        }
      }
    };
    void load();
    const timer = window.setInterval(() => void load(), SPREAD_REFRESH_MS);
    return () => {
      active = false;
      controller.abort();
      window.clearInterval(timer);
    };
  }, [spreadQuery, yieldMode, legAInstrument, legBInstrument, spreadRetry]);

  const targetValue = Number(targetNotional);
  const orderValue = Number(orderNotional);
  const maxDeltaValue = Number(maxDeltaNotional);
  const notionalConstraintError =
    orderValue > 0 && maxDeltaValue > orderValue
      ? "最大未对冲敞口不能大于单笔订单金额"
      : orderValue > 0 && targetValue > 0 && orderValue > targetValue
        ? "单笔订单金额不能大于目标仓位"
        : "";
  const parametersValid = [targetValue, orderValue, maxDeltaValue].every((value) => value > 0) &&
    orderValue <= targetValue &&
    maxDeltaValue <= orderValue &&
    [askThreshold, bidThreshold].every((value) => Number.isFinite(Number(value)));
  const canCreate =
    !busy &&
    !pairError &&
    parametersValid &&
    legAAccountID !== null &&
    legBAccountID !== null &&
    legAAccountID !== legBAccountID;

  function chooseExchange(
    value: string,
    setExchange: (next: string) => void,
    setAccountID: (next: number | null) => void,
    setInstrumentID: (next: number | null) => void,
  ) {
    setExchange(value);
    setAccountID(productAccounts.find((item) => item.exchangeSlug === value)?.id ?? null);
    setInstrumentID(null);
  }

  function chooseProduct(value: string) {
    const nextAccounts = products.find((item) => item.productName === value)?.accounts ?? [];
    const first = nextAccounts[0];
    const second =
      nextAccounts.find((item) => item.exchangeSlug !== first?.exchangeSlug) ??
      nextAccounts[1] ??
      first;
    setProductName(value);
    setLegAExchange(first?.exchangeSlug ?? "");
    setLegAAccountID(first?.id ?? null);
    setLegBExchange(second?.exchangeSlug ?? "");
    setLegBAccountID(second?.id ?? null);
    setLegAInstrumentID(null);
    setLegBInstrumentID(null);
  }

  async function submitCreate() {
    if (
      !canCreate ||
      legAAccountID === null ||
      legBAccountID === null ||
      visibleLegAInstrumentID === null ||
      visibleLegBInstrumentID === null
    ) return;
    setBusy(true);
    setMessage("");
    try {
      const created = await createArbitrageCombination({
        productName,
        legAAccountId: legAAccountID,
        legAInstrumentId: visibleLegAInstrumentID,
        legBAccountId: legBAccountID,
        legBInstrumentId: visibleLegBInstrumentID,
        askThresholdBps: askThreshold.trim(),
        bidThresholdBps: bidThreshold.trim(),
        targetNotional: targetNotional.trim(),
        orderNotional: orderNotional.trim(),
        maxDeltaNotional: maxDeltaNotional.trim(),
        preferredLeg,
        executionMode,
      });
      setMessage(`套利组合 ${created.id} 已创建`);
      setCombinationView("running");
      await refreshCombinations();
    } catch (error) {
      setMessage(error instanceof Error ? error.message : "套利组合创建失败");
    } finally {
      setBusy(false);
    }
  }

  async function toggleDetail(id: string) {
    if (detailID === id) {
      setDetailID(null);
      setDetail(null);
      return;
    }
    setDetailID(id);
    setDetail(null);
    setDetailLoading(true);
    try {
      const next = await fetchArbitrageCombination(id);
      setDetail(next);
    } catch (error) {
      setMessage(error instanceof Error ? error.message : "套利组合详情加载失败");
    } finally {
      setDetailLoading(false);
    }
  }

  async function closeCombination(id: string) {
    if (!window.confirm("确认关闭此套利组合？系统将停止新触发，并异步撤单及对冲剩余敞口。")) {
      return;
    }
    setClosingID(id);
    try {
      await closeArbitrageCombination(id);
      setMessage(`套利组合 ${id} 正在关闭`);
      await refreshCombinations();
    } catch (error) {
      setMessage(error instanceof Error ? error.message : "关闭套利组合失败");
    } finally {
      setClosingID(null);
    }
  }

  return (
    <div className="min-w-0 space-y-3 p-3 lg:p-4">
      <section className="flex flex-wrap items-center gap-3 rounded-xl border bg-muted/15 px-4 py-3">
        <div className="flex size-9 items-center justify-center rounded-lg bg-primary/10 text-primary">
          <ArrowLeftRight className="size-4" />
        </div>
        <div className="mr-auto">
          <div className="flex items-center gap-2">
            <h2 className="text-sm font-semibold">跨所资金费套利</h2>
          </div>
          <p className="mt-0.5 text-[11px] text-muted-foreground">
            配置双腿执行关系、价差触发线与仓位预算
          </p>
        </div>
        <label className="flex min-w-52 items-center gap-3">
          <span className="shrink-0 text-xs text-muted-foreground">产品</span>
          <select
            value={productName}
            onChange={(event) => chooseProduct(event.target.value)}
            className={selectClassName}
          >
            {products.map((product) => (
              <option key={product.productName} value={product.productName}>
                {product.productName}
              </option>
            ))}
          </select>
        </label>
      </section>

      {accountsError ? (
        <div className="rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs text-destructive">
          {accountsError}
        </div>
      ) : null}

      <section className="grid gap-3 xl:grid-cols-[1fr_48px_1fr]">
        <LegCard
          label="LEG A"
          accent="positive"
          exchanges={exchanges}
          accounts={legAAccounts}
          instruments={legAInstruments}
          loading={legALoading}
          exchange={legAExchange}
          accountID={legAAccountID}
          contractType={legAContractType}
          instrumentID={visibleLegAInstrumentID}
          onExchangeChange={(value) =>
            chooseExchange(value, setLegAExchange, setLegAAccountID, setLegAInstrumentID)
          }
          onAccountChange={(value) => {
            setLegAAccountID(value);
            setLegAInstrumentID(null);
          }}
          onContractTypeChange={(value) => {
            setLegAContractType(value);
            setLegAInstrumentID(null);
          }}
          onInstrumentChange={setLegAInstrumentID}
        />
        <div className="hidden items-center justify-center xl:flex">
          <div className="flex size-9 items-center justify-center rounded-full border bg-background text-muted-foreground">
            <ArrowLeftRight className="size-4" />
          </div>
        </div>
        <LegCard
          label="LEG B"
          accent="negative"
          exchanges={exchanges}
          accounts={legBAccounts}
          instruments={legBInstruments}
          loading={legBLoading}
          exchange={legBExchange}
          accountID={legBAccountID}
          contractType={legBContractType}
          instrumentID={visibleLegBInstrumentID}
          onExchangeChange={(value) =>
            chooseExchange(value, setLegBExchange, setLegBAccountID, setLegBInstrumentID)
          }
          onAccountChange={(value) => {
            setLegBAccountID(value);
            setLegBInstrumentID(null);
          }}
          onContractTypeChange={(value) => {
            setLegBContractType(value);
            setLegBInstrumentID(null);
          }}
          onInstrumentChange={setLegBInstrumentID}
        />
      </section>

      <div
        className={cn(
          "flex items-center gap-2 rounded-lg border px-3 py-2 text-xs",
          pairError
            ? "border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-300"
            : "border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300",
        )}
      >
        {pairError ? <CircleAlert className="size-3.5" /> : <CheckCircle2 className="size-3.5" />}
        {pairError ||
          `${legAInstrument?.baseAsset}/${legAInstrument?.quoteAsset} 配对有效 · Ask: A 多 / B 空 · Bid: A 空 / B 多`}
      </div>

      <section className="grid items-start gap-3 xl:grid-cols-[minmax(0,1fr)_240px]">
        <div className="min-w-0 space-y-3">
          <div className="min-w-0 rounded-xl border bg-card">
            <div className="space-y-3 border-b px-4 py-3">
              <div className="flex flex-wrap items-center gap-3">
                <div className="mr-auto min-w-0">
                  <div className="flex items-center gap-2 text-sm font-medium">
                    <TrendingUp className="size-4 text-primary" />
                    价差走势
                  </div>
                  <p className="mt-0.5 truncate text-[11px] text-muted-foreground">
                    {spreadQuery?.formula ?? "Leg A / Leg B 配对后展示 Best Ask 价差"}
                  </p>
                </div>
                <div className="flex shrink-0 rounded-lg border bg-muted/30 p-0.5">
                  {periods.map((item) => (
                    <button
                      key={item}
                      type="button"
                      onClick={() => setPeriod(item)}
                      className={cn(
                        "rounded-md px-2.5 py-1 text-[11px] font-medium transition-colors",
                        period === item
                          ? "bg-background text-foreground shadow-sm"
                          : "text-muted-foreground hover:text-foreground",
                      )}
                    >
                      {item}
                    </button>
                  ))}
                </div>
              </div>
              <div
                className={cn(
                  "grid w-full min-w-0 divide-x rounded-lg border bg-muted/20",
                  yieldMode ? "grid-cols-5" : "grid-cols-3",
                )}
              >
                <HeaderMetric
                  label="当前"
                  value={spreadSummary ? formatBps(spreadSummary.currentBps) : "—"}
                />
                <HeaderMetric
                  label="周期均值"
                  value={spreadSummary ? formatBps(spreadSummary.avgBps) : "—"}
                />
                <HeaderMetric
                  label="覆盖率"
                  value={spreadSummary ? formatCoverage(spreadSummary.coverage) : "—"}
                />
                {yieldMode ? (
                  <>
                    <HeaderMetric
                      label={yieldMode === "basis" ? "24H 差值年化" : "24H 窗口年化"}
                      value={
                        yieldMetrics.value24h == null
                          ? "—"
                          : formatPercent(yieldMetrics.value24h, 1)
                      }
                      valueClassName={rateColor(yieldMetrics.value24h)}
                    />
                    <HeaderMetric
                      label={yieldMode === "basis" ? "7D 差值年化" : "7D 窗口年化"}
                      value={
                        yieldMetrics.value7d == null
                          ? "—"
                          : formatPercent(yieldMetrics.value7d, 1)
                      }
                      valueClassName={rateColor(yieldMetrics.value7d)}
                    />
                  </>
                ) : null}
              </div>
            </div>
            {!spreadQuery ? (
              <SpreadChartState>
                {pairError
                  ? "Leg A / Leg B 的 Symbol 配对有效后展示价差走势"
                  : "当前组合类型暂无 Best Ask 价差历史"}
              </SpreadChartState>
            ) : spreadLoading && !visibleSpreadHistory ? (
              <SpreadChartState>
                <RefreshCw className="size-4 animate-spin" />
                正在加载价差走势…
              </SpreadChartState>
            ) : spreadError && !visibleSpreadHistory ? (
              <SpreadChartState>
                <span>{spreadError}</span>
                <button
                  type="button"
                  className="inline-flex items-center gap-1 text-foreground"
                  onClick={() => {
                    spreadCache.current.delete(spreadCacheKey(spreadQuery, period));
                    setSpreadRetry((value) => value + 1);
                  }}
                >
                  <RefreshCw className="size-3" />
                  重试
                </button>
              </SpreadChartState>
            ) : visibleSpreadHistory?.availability === "unavailable" ? (
              <SpreadChartState>当前组合暂无对应 Best Ask BBO 数据</SpreadChartState>
            ) : visibleSpreadHistory && visibleSpreadHistory.points.length === 0 ? (
              <SpreadChartState>当前周期暂无配对价差</SpreadChartState>
            ) : visibleSpreadHistory ? (
              <SpreadChart
                values={visibleSpreadHistory.points.map((point) => point.spreadBps)}
                askThreshold={Number(askThreshold)}
                bidThreshold={Number(bidThreshold)}
              />
            ) : null}
          </div>
          <ArbitrageCombinationList
            view={combinationView}
            onViewChange={(next) => {
              setCombinationView(next);
              setDetailID(null);
              setDetail(null);
            }}
            rows={pages[combinationView]}
            counts={counts}
            loading={listLoading}
            error={listError}
            detailID={detailID}
            detail={detail}
            detailLoading={detailLoading}
            onDetailChange={toggleDetail}
            closingID={closingID}
            onClose={closeCombination}
          />
        </div>

        <div className="min-w-0 rounded-xl border bg-card xl:min-w-[240px] xl:max-w-[240px]">
          <div className="flex items-center gap-2 border-b px-3 py-3">
            <Gauge className="size-4 shrink-0 text-primary" />
            <div className="min-w-0">
              <h3 className="text-sm font-medium">组合执行参数</h3>
              <p className="text-[11px] text-muted-foreground">USDT 名义仓位与触发规则</p>
            </div>
          </div>
          <div className="grid gap-3 p-3">
            <Field label="目标仓位">
              <UnitInput value={targetNotional} onChange={setTargetNotional} unit="USDT" />
            </Field>
            <Field label="单笔订单金额">
              <UnitInput value={orderNotional} onChange={setOrderNotional} unit="USDT" />
            </Field>
            <Field label="最大未对冲敞口">
              <UnitInput value={maxDeltaNotional} onChange={setMaxDeltaNotional} unit="USDT" />
            </Field>
            <Field label="Ask Threshold">
              <UnitInput value={askThreshold} onChange={setAskThreshold} unit="bps" />
            </Field>
            <Field label="Bid Threshold">
              <UnitInput value={bidThreshold} onChange={setBidThreshold} unit="bps" />
            </Field>
            <Field label="执行优先级">
              <select
                value={preferredLeg}
                onChange={(event) => setPreferredLeg(event.target.value as ArbitragePreferredLeg)}
                disabled={executionMode === "simultaneous_market"}
                className={selectClassName}
              >
                <option value="a">Leg A 先执行</option>
                <option value="b">Leg B 先执行</option>
              </select>
            </Field>
            <Field label="执行模式">
              <select
                value={executionMode}
                onChange={(event) =>
                  setExecutionMode(event.target.value as ArbitrageExecutionMode)
                }
                className={selectClassName}
              >
                <option value="maker_then_hedge">Maker 成交后对冲</option>
                <option value="simultaneous_market">双腿并发市价</option>
              </select>
            </Field>
          </div>
          <div className="border-t p-3">
            <div className="mb-3 flex items-start gap-2 rounded-lg bg-muted/35 px-3 py-2 text-[11px] text-muted-foreground">
              <ShieldCheck className="mt-0.5 size-3.5 shrink-0 text-primary" />
              <span>创建前将再次校验合约、方向、Quote Asset 与最大订单金额。</span>
            </div>
            <Button className="w-full" disabled={!canCreate} onClick={() => void submitCreate()}>
              <Play data-icon="inline-start" />
              {busy ? "正在创建…" : "创建套利组合"}
            </Button>
            {notionalConstraintError ? (
              <p className="mt-2 text-center text-[11px] text-destructive">
                {notionalConstraintError}
              </p>
            ) : message ? (
              <p className={cn(
                "mt-2 text-center text-[11px]",
                message.includes("失败") || message.includes("invalid")
                  ? "text-destructive"
                  : "text-positive",
              )}
              >
                {message}
              </p>
            ) : null}
          </div>
        </div>
      </section>

    </div>
  );
}

function LegCard({
  label,
  accent,
  exchanges,
  accounts,
  instruments,
  loading,
  exchange,
  accountID,
  contractType,
  instrumentID,
  onExchangeChange,
  onAccountChange,
  onContractTypeChange,
  onInstrumentChange,
}: {
  label: string;
  accent: "positive" | "negative";
  exchanges: Array<[string, TradingAccount["exchange"]]>;
  accounts: TradingAccount[];
  instruments: TraderInstrument[];
  loading: boolean;
  exchange: string;
  accountID: number | null;
  contractType: TraderContractType;
  instrumentID: number | null;
  onExchangeChange: (value: string) => void;
  onAccountChange: (value: number | null) => void;
  onContractTypeChange: (value: TraderContractType) => void;
  onInstrumentChange: (value: number | null) => void;
}) {
  return (
    <div className="rounded-xl border bg-card">
      <div className="flex items-center gap-2 border-b px-4 py-2.5">
        <span
          className={cn(
            "flex size-7 items-center justify-center rounded-md font-mono text-[10px] font-bold",
            accent === "positive"
              ? "bg-positive-soft text-positive"
              : "bg-negative-soft text-negative",
          )}
        >
          {label.at(-1)}
        </span>
        <div>
          <div className="font-mono text-xs font-semibold">{label}</div>
          <div className="text-[10px] text-muted-foreground">
            {contractType === "perpetual" ? "PERPETUAL" : "SPOT"}
          </div>
        </div>
        <Activity className="ml-auto size-3.5 text-muted-foreground" />
      </div>
      <div className="grid gap-3 p-3 sm:grid-cols-2 2xl:grid-cols-4">
        <Field label="交易所">
          <select
            value={exchange}
            onChange={(event) => onExchangeChange(event.target.value)}
            className={selectClassName}
          >
            {exchanges.map(([slug, name]) => (
              <option key={slug} value={slug}>{name}</option>
            ))}
          </select>
        </Field>
        <Field label="账户">
          <select
            value={accountID ?? ""}
            onChange={(event) => onAccountChange(Number(event.target.value) || null)}
            className={selectClassName}
          >
            {accounts.map((account) => (
              <option key={account.id} value={account.id}>{account.accountName}</option>
            ))}
          </select>
        </Field>
        <Field label="产品类型">
          <select
            value={contractType}
            onChange={(event) =>
              onContractTypeChange(event.target.value as TraderContractType)
            }
            className={selectClassName}
          >
            <option value="perpetual">永续合约</option>
            <option value="spot">现货</option>
          </select>
        </Field>
        <Field label="Symbol">
          <div className="relative">
            <select
              value={instrumentID ?? ""}
              onChange={(event) => onInstrumentChange(Number(event.target.value) || null)}
              className={cn(selectClassName, "pr-7 font-mono")}
            >
              {instruments.map((instrument) => (
                <option key={instrument.id} value={instrument.id}>
                  {instrument.baseAsset}/{instrument.quoteAsset}
                </option>
              ))}
            </select>
            {loading ? (
              <RefreshCw className="absolute top-2 right-2 size-3 animate-spin text-muted-foreground" />
            ) : null}
          </div>
        </Field>
      </div>
    </div>
  );
}

function ArbitrageCombinationList({
  view,
  onViewChange,
  rows,
  counts,
  loading,
  error,
  detailID,
  detail,
  detailLoading,
  onDetailChange,
  closingID,
  onClose,
}: {
  view: ArbitrageView;
  onViewChange: (view: ArbitrageView) => void;
  rows: ArbitrageCombination[];
  counts: Record<ArbitrageView, number>;
  loading: boolean;
  error: string;
  detailID: string | null;
  detail: ArbitrageCombinationDetail | null;
  detailLoading: boolean;
  onDetailChange: (id: string) => void;
  closingID: string | null;
  onClose: (id: string) => void;
}) {
  return (
    <section className="overflow-hidden rounded-xl border bg-card">
      <div className="flex flex-wrap items-center gap-3 border-b px-4 py-3">
        <div className="flex size-8 items-center justify-center rounded-lg bg-primary/10 text-primary">
          <ListFilter className="size-4" />
        </div>
        <div className="mr-auto">
          <h3 className="text-sm font-medium">套利组合</h3>
          <p className="text-[11px] text-muted-foreground">
            监控实时双边价差、目标仓位与执行进度
          </p>
        </div>
        <div className="flex rounded-lg border bg-muted/30 p-0.5">
          {(["running", "closed"] as ArbitrageView[]).map((item) => (
            <button
              key={item}
              type="button"
              onClick={() => onViewChange(item)}
              className={cn(
                "rounded-md px-3 py-1.5 text-xs font-medium transition-colors",
                view === item
                  ? "bg-background text-foreground shadow-sm"
                  : "text-muted-foreground hover:text-foreground",
              )}
            >
              {item === "running" ? `运行中 ${counts.running}` : `已关闭 ${counts.closed}`}
            </button>
          ))}
        </div>
      </div>

      <div className="min-w-0 overflow-x-auto">
        <table className="w-full min-w-[980px] text-left text-xs">
          <thead className="border-b bg-muted/20 text-[10px] tracking-wide text-muted-foreground uppercase">
            <tr>
              <th className="px-4 py-2.5 font-medium">交易所</th>
              <th className="px-3 py-2.5 font-medium">币对</th>
              <th className="px-3 py-2.5 text-right font-medium">实时 Bid Spread</th>
              <th className="px-3 py-2.5 text-right font-medium">实时 Ask Spread</th>
              <th className="px-3 py-2.5 text-right font-medium">目标仓位</th>
              <th className="px-3 py-2.5 text-right font-medium">当前仓位</th>
              <th className="px-3 py-2.5 font-medium">运行状态</th>
              <th className="px-4 py-2.5 text-right font-medium">操作</th>
            </tr>
          </thead>
          <tbody className="divide-y">
            {rows.map((item) => {
              const progress = Math.min(
                100,
                Math.max(0, (Math.abs(Number(item.positionNotional)) / Number(item.targetNotional)) * 100),
              );
              const expanded = detailID === item.id;
              return (
                <React.Fragment key={item.id}>
                  <tr
                    className={cn(
                      "cursor-pointer transition-colors hover:bg-muted/20",
                      expanded && "bg-primary/[0.04]",
                    )}
                    onClick={() => void onDetailChange(item.id)}
                  >
                    <td className="px-4 py-3">
                      <div className="font-medium">{item.legA.exchange} ↔ {item.legB.exchange}</div>
                      <div className="mt-0.5 font-mono text-[10px] text-muted-foreground">{item.id}</div>
                    </td>
                    <td className="px-3 py-3 font-mono font-medium">
                      {item.legA.baseAsset} / {item.legA.quoteAsset}
                    </td>
                    <td className="px-3 py-3 text-right font-mono text-negative">
                      {item.marketDataStale ? "行情过期" : formatSpread(item.bidSpreadBps)}
                    </td>
                    <td className="px-3 py-3 text-right font-mono text-positive">
                      {item.marketDataStale ? "行情过期" : formatSpread(item.askSpreadBps)}
                    </td>
                    <td className="px-3 py-3 text-right font-mono">
                      {formatDecimal(item.targetNotional, " USDT")}
                    </td>
                    <td className="px-3 py-3 text-right">
                      <div className="font-mono">{formatSignedNotional(item.positionNotional)}</div>
                      <div className="mt-1 h-1 overflow-hidden rounded-full bg-muted">
                        <div
                          className="h-full rounded-full bg-primary"
                          style={{ width: `${Number.isFinite(progress) ? progress : 0}%` }}
                        />
                      </div>
                    </td>
                    <td className="px-3 py-3"><CombinationStatus item={item} /></td>
                    <td className="px-4 py-3 text-right">
                      <div className="flex justify-end gap-1">
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={(event) => {
                            event.stopPropagation();
                            void onDetailChange(item.id);
                          }}
                        >
                          <Eye data-icon="inline-start" />
                          {expanded ? "收起" : "详情"}
                        </Button>
                        {view === "running" ? (
                          <Button
                            variant="outline"
                            size="sm"
                            disabled={item.status === "closing" || closingID === item.id}
                            onClick={(event) => {
                              event.stopPropagation();
                              void onClose(item.id);
                            }}
                          >
                            {item.status === "closing" || closingID === item.id ? "关闭中" : "关闭"}
                          </Button>
                        ) : null}
                      </div>
                    </td>
                  </tr>
                  {expanded ? (
                    <tr className="bg-muted/10">
                      <td colSpan={8} className="px-4 py-3">
                        {detailLoading && detail?.id !== item.id ? (
                          <div className="text-xs text-muted-foreground">正在加载详情…</div>
                        ) : (
                          <CombinationDetail detail={detail?.id === item.id ? detail : item} />
                        )}
                      </td>
                    </tr>
                  ) : null}
                </React.Fragment>
              );
            })}
            {!loading && rows.length === 0 ? (
              <tr><td colSpan={8} className="px-4 py-8 text-center text-muted-foreground">暂无套利组合</td></tr>
            ) : null}
            {loading ? (
              <tr><td colSpan={8} className="px-4 py-8 text-center text-muted-foreground">正在加载套利组合…</td></tr>
            ) : null}
          </tbody>
        </table>
      </div>
      {error ? <div className="border-t px-4 py-2 text-xs text-destructive">{error}</div> : null}
    </section>
  );
}

function CombinationStatus({ item }: { item: ArbitrageCombination }) {
  const label = combinationRuntimeLabel(item);
  return (
    <Badge
      variant="outline"
      className={cn(
        item.status === "running" &&
          "border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300",
        item.status === "closing" &&
          "border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-300",
        (item.status === "failed" || item.positionUncertain) &&
          "border-destructive/30 bg-destructive/10 text-destructive",
      )}
    >
      {label}
    </Badge>
  );
}

function combinationRuntimeLabel(item: ArbitrageCombination): string {
  if (item.status === "closing") return "关闭中";
  if (item.status === "closed") return "已关闭";
  if (item.status === "failed") return "失败";
  if (item.positionUncertain) return "仓位待核对";
  if (item.nextRetryAt && Date.parse(item.nextRetryAt) > Date.now()) return "退避重试";
  const position = Number(item.positionNotional);
  const target = Number(item.targetNotional);
  if (Number.isFinite(position) && Number.isFinite(target) && target > 0) {
    if (position >= target) return "等待 Bid 平仓";
    if (position <= -target) return "等待 Ask 平仓";
    if (position > 0) return "持有正套";
    if (position < 0) return "持有反套";
  }
  return "运行中";
}

function formatSignedNotional(value: string): string {
  const amount = Number(value);
  if (!Number.isFinite(amount)) return `${value} USDT`;
  const prefix = amount > 0 ? "+" : "";
  return `${prefix}${formatDecimal(value, " USDT")}`;
}

function CombinationDetail({
  detail,
}: {
  detail: ArbitrageCombination | ArbitrageCombinationDetail;
}) {
  const complete = "recentExecutions" in detail;
  return (
    <div>
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <span className="font-mono text-xs font-semibold">{detail.id}</span>
        <span className="text-[11px] text-muted-foreground">创建于 {formatTime(detail.createdAt)}</span>
        {detail.marketDataStale ? <Badge variant="outline">行情过期</Badge> : null}
      </div>
      <div className="grid gap-x-6 gap-y-3 sm:grid-cols-2 lg:grid-cols-4">
        <DetailItem
          label="Leg A"
          value={`${detail.legA.exchange} · ${detail.legA.accountName} · ${detail.legA.exchangeSymbol}`}
        />
        <DetailItem
          label="Leg B"
          value={`${detail.legB.exchange} · ${detail.legB.accountName} · ${detail.legB.exchangeSymbol}`}
        />
        <DetailItem label="Ask 方向" value="买 Leg A / 卖 Leg B" />
        <DetailItem label="Bid 方向" value="卖 Leg A / 买 Leg B" />
        <DetailItem
          label="目标 / 当前仓位"
          value={`${detail.targetNotional} / ${detail.positionNotional} USDT`}
        />
        <DetailItem label="累计成交" value={`${detail.cumulativeTurnoverNotional} USDT`} />
        <DetailItem label="Ask / Bid 阈值" value={`${detail.askThresholdBps} / ${detail.bidThresholdBps} bps`} />
        <DetailItem label="单笔 / 最大敞口" value={`${detail.orderNotional} / ${detail.maxDeltaNotional} USDT`} />
        <DetailItem
          label="执行方式"
          value={detail.executionMode === "simultaneous_market"
            ? "双腿并发市价"
            : `Leg ${detail.preferredLeg.toUpperCase()} Maker 后对冲`}
        />
      </div>
      {detail.errorMessage ? (
        <div className="mt-3 rounded-md bg-destructive/10 px-3 py-2 text-xs text-destructive">
          {detail.errorMessage}
        </div>
      ) : null}
      {complete && detail.recentExecutions.length > 0 ? (
        <div className="mt-3 border-t pt-3">
          <div className="mb-2 text-[10px] font-medium tracking-wide text-muted-foreground uppercase">
            最近执行
          </div>
          <div className="space-y-1">
            {detail.recentExecutions.map((execution) => (
              <div key={execution.id} className="flex flex-wrap gap-x-4 text-[11px]">
                <span>{execution.direction === "ask" ? "Ask" : "Bid"}</span>
                <span>{execution.status}</span>
                <span className="font-mono">触发 {execution.triggerSpreadBps} bps</span>
                <span className="font-mono">Delta {execution.deltaNotional} USDT</span>
              </div>
            ))}
          </div>
        </div>
      ) : null}
    </div>
  );
}

function DetailItem({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <div className="text-[10px] tracking-wide text-muted-foreground uppercase">{label}</div>
      <div className="mt-1 text-xs font-medium">{value}</div>
    </div>
  );
}

function SpreadChartState({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex h-[232px] items-center justify-center gap-2 px-4 text-xs text-muted-foreground">
      {children}
    </div>
  );
}

function SpreadChart({
  values,
  askThreshold,
  bidThreshold,
}: {
  values: number[];
  askThreshold: number;
  bidThreshold: number;
}) {
  const width = 760;
  const height = 220;
  const padding = 28;
  const validThresholds = [askThreshold, bidThreshold].filter(Number.isFinite);
  const min = Math.min(...values, ...validThresholds, -10) - 3;
  const max = Math.max(...values, ...validThresholds, 20) + 3;
  const x = (index: number) =>
    padding + (index / Math.max(values.length - 1, 1)) * (width - padding * 2);
  const y = (value: number) =>
    padding + ((max - value) / (max - min)) * (height - padding * 2);
  const path = values
    .map((value, index) => `${index === 0 ? "M" : "L"} ${x(index)} ${y(value)}`)
    .join(" ");
  const area = `${path} L ${x(values.length - 1)} ${height - padding} L ${padding} ${height - padding} Z`;

  return (
    <div className="px-2 py-3">
      <svg viewBox={`0 0 ${width} ${height}`} className="h-52 w-full" role="img" aria-label="Best Ask 价差走势">
        <defs>
          <linearGradient id="spread-area" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="var(--primary)" stopOpacity="0.28" />
            <stop offset="100%" stopColor="var(--primary)" stopOpacity="0.01" />
          </linearGradient>
        </defs>
        {[0, 0.25, 0.5, 0.75, 1].map((ratio) => {
          const lineY = padding + ratio * (height - padding * 2);
          return (
            <line
              key={ratio}
              x1={padding}
              x2={width - padding}
              y1={lineY}
              y2={lineY}
              stroke="var(--border)"
              strokeWidth="1"
            />
          );
        })}
        {Number.isFinite(askThreshold) ? (
          <ThresholdLine y={y(askThreshold)} label={`ASK ${askThreshold} bps`} tone="positive" />
        ) : null}
        {Number.isFinite(bidThreshold) ? (
          <ThresholdLine y={y(bidThreshold)} label={`BID ${bidThreshold} bps`} tone="negative" />
        ) : null}
        <path d={area} fill="url(#spread-area)" />
        <path d={path} fill="none" stroke="var(--primary)" strokeWidth="2.5" strokeLinejoin="round" />
        <circle
          cx={x(values.length - 1)}
          cy={y(values.at(-1) ?? 0)}
          r="4"
          fill="var(--primary)"
          stroke="var(--background)"
          strokeWidth="2"
        />
        <text x={padding} y={height - 7} fill="var(--muted-foreground)" fontSize="10">过去</text>
        <text x={width - padding} y={height - 7} textAnchor="end" fill="var(--muted-foreground)" fontSize="10">现在</text>
      </svg>
    </div>
  );
}

function ThresholdLine({
  y,
  label,
  tone,
}: {
  y: number;
  label: string;
  tone: "positive" | "negative";
}) {
  const color = tone === "positive" ? "var(--positive)" : "var(--negative)";
  return (
    <>
      <line x1="28" x2="732" y1={y} y2={y} stroke={color} strokeWidth="1" strokeDasharray="5 5" />
      <text x="728" y={y - 5} textAnchor="end" fill={color} fontSize="10">{label}</text>
    </>
  );
}

function HeaderMetric({
  label,
  value,
  valueClassName,
}: {
  label: string;
  value: string;
  valueClassName?: string;
}) {
  return (
    <div className="min-w-0 px-2 py-1.5 sm:px-3">
      <div className="truncate text-[9px] font-medium tracking-wide text-muted-foreground uppercase">
        {label}
      </div>
      <div className={cn("mt-0.5 truncate font-mono text-sm font-semibold", valueClassName)}>
        {value}
      </div>
    </div>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="grid gap-1.5">
      <span className="text-[10px] font-medium tracking-wide text-muted-foreground uppercase">{label}</span>
      {children}
    </label>
  );
}

function UnitInput({
  value,
  onChange,
  unit,
}: {
  value: string;
  onChange: (value: string) => void;
  unit: string;
}) {
  return (
    <div className="relative">
      <Input
        value={value}
        onChange={(event) => onChange(event.target.value)}
        inputMode="decimal"
        className="pr-14 font-mono"
      />
      <span className="pointer-events-none absolute top-1/2 right-2.5 -translate-y-1/2 text-[10px] text-muted-foreground">
        {unit}
      </span>
    </div>
  );
}
