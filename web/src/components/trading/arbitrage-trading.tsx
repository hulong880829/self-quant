"use client";

import * as React from "react";
import {
  Activity,
  ArrowLeftRight,
  Check,
  CheckCircle2,
  CircleAlert,
  Eye,
  Gauge,
  ListFilter,
  Pencil,
  Play,
  RefreshCw,
  TrendingUp,
  X,
} from "lucide-react";

import {
  accountSnapshotNeedsLiveRefresh,
  applyTradingReadiness,
  fetchCachedTradingAccountSnapshot,
  fetchLiveTradingAccountSnapshot,
  fetchTradingAccounts,
  groupAccountsByProduct,
  inspectTradingReadiness,
  isWalletDexExchange,
  tradingAccountStatusLabel,
  type ProductGroup,
  type TradingAccount,
  type TradingAccountSnapshot,
} from "@/lib/api/accounts";
import {
  ArbitrageCreateFailure,
  annualizedPercentToRatio,
  closeArbitrageCombination,
  createArbitrageCombination,
  fetchArbitrageCombination,
  fetchArbitrageCombinations,
  formatAnnualizedRatioAsPercent,
  formatSignedAnnualizedRatioAsPercent,
  signedAnnualizedPercentToRatio,
  updateArbitrageCombination,
  type ArbitrageCombination,
  type ArbitrageCombinationDetail,
  type ArbitrageExecutionMode,
  type ArbitrageExitPolicy,
  type ArbitragePreferredLeg,
  type ArbitrageRunMode,
  type ArbitrageView,
} from "@/lib/api/arbitrage";
import {
  ARBITRAGE_EXCHANGES,
  fetchTraderInstruments,
  type TraderContractType,
  type TraderInstrument,
} from "@/lib/api/trader";
import {
  FUNDING_RATES_LOOKUP_MAX_KEYS,
  fetchFundingRatesLookup,
  fundingRequestKey,
  type FundingRateLookupKey,
} from "@/lib/api/funding";
import {
  arbitragePrefillSignature,
  findPrefillProductGroup,
  findPrefillInstrument,
  parseArbitragePrefill,
  type ArbitragePrefill,
} from "@/lib/trading-prefill";
import type { FundingOpportunity } from "@/types/market";
import {
  fetchBasisSpreadHistory,
  type BasisSpreadHistory,
  type BasisSpreadRange,
} from "@/lib/api/spread";
import { isAbortError } from "@/lib/abort";
import { bboCanonicalSymbol } from "@/lib/funding-coverage";
import {
  annualize24h,
  annualize7d,
  formatCurrency,
  formatPercent,
  rateColor,
} from "@/lib/market-format";
import { spreadYDomain } from "@/components/funding/basis-spread-chart";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

const selectClassName =
  "h-8 w-full rounded-lg border border-input bg-background px-2.5 text-sm outline-none transition-colors focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50";

const EXIT_AFTER_OPTIONS = [
  { value: 3600, label: "1 小时" },
  { value: 14400, label: "4 小时" },
  { value: 28800, label: "8 小时" },
  { value: 86400, label: "24 小时" },
  { value: 604800, label: "7 天" },
] as const;

function formatHoldDuration(seconds: number): string {
  const option = EXIT_AFTER_OPTIONS.find((item) => item.value === seconds);
  if (option) {
    return `持仓 ${option.label}`;
  }
  return `持仓 ${seconds} 秒`;
}

function defaultLegLeverage(contractType: TraderContractType): string {
  return contractType === "spot" ? "1" : "4";
}

function defaultArbitrageLegs(groups: ProductGroup[]) {
  const firstProduct = groups[0];
  const first = firstProduct?.accounts[0];
  const second =
    firstProduct?.accounts.find(
      (item) => item.exchangeSlug !== first?.exchangeSlug,
    ) ??
    firstProduct?.accounts[1] ??
    first;
  return {
    productName: firstProduct?.productName ?? "",
    legAExchange: first?.exchangeSlug ?? "",
    legAAccountID: first?.id ?? null,
    legBExchange: second?.exchangeSlug ?? "",
    legBAccountID: second?.id ?? null,
  };
}

function prefillArbitrageLegs(prefill: ArbitragePrefill, groups: ProductGroup[]) {
  const product = findPrefillProductGroup(groups, prefill);
  const productAccounts = product?.accounts ?? [];
  const legAAccount = productAccounts.find(
    (item) => item.exchangeSlug === prefill.legA.exchange,
  );
  const legBAccount = productAccounts.find(
    (item) => item.exchangeSlug === prefill.legB.exchange,
  );
  const legAContract = isWalletDexExchange(prefill.legA.exchange)
    ? "perpetual"
    : prefill.legA.contract;
  const legBContract = isWalletDexExchange(prefill.legB.exchange)
    ? "perpetual"
    : prefill.legB.contract;
  return {
    productName: product?.productName ?? "",
    legAExchange: prefill.legA.exchange,
    legBExchange: prefill.legB.exchange,
    legAContractType: legAContract,
    legBContractType: legBContract,
    legALeverage: defaultLegLeverage(legAContract),
    legBLeverage: defaultLegLeverage(legBContract),
    legAAccountID: legAAccount?.id ?? null,
    legBAccountID: legBAccount?.id ?? null,
  };
}

const periods: BasisSpreadRange[] = ["1h", "4h", "8h", "24h", "7d"];
const COMBINATION_POLL_MS = 4_000;
const COMBINATION_CLOSING_POLL_MS = 1_000;
const SPREAD_REFRESH_MS = 30_000;
const SPREAD_CACHE_TTL_MS = 30_000;
const FUNDING_LOOKUP_REFRESH_MS = 30_000;
const FUNDING_LOOKUP_BACKOFF_MS = [1_000, 2_000, 5_000] as const;
const beijingChartTimeFormatter = new Intl.DateTimeFormat("zh-CN", {
  timeZone: "Asia/Shanghai",
  hour: "2-digit",
  minute: "2-digit",
  hour12: false,
});

type BasisSpreadQuery = {
  venue: string;
  compareVenue?: string;
  baseAsset: string;
  quoteAsset: string;
  venueSymbol?: string;
  compareVenueSymbol?: string;
  formula: string;
};

type SpreadLeg = Pick<
  TraderInstrument,
  "exchange" | "contractType" | "exchangeSymbol" | "baseAsset" | "quoteAsset"
>;

type SpreadCacheEntry = {
  history: BasisSpreadHistory;
  etag: string | null;
  loadedAt: number;
};

type FundingCacheEntry = {
  status: "hit" | "missing";
  item?: FundingOpportunity;
  snapshotVersion: string;
  loadedAt: number;
  fresh: boolean;
};

export type FundingLegResolver = (
  instrument: Pick<TraderInstrument, "exchange" | "exchangeSymbol">,
) => FundingOpportunity | undefined;

export type FundingLegLookupState = {
  item?: FundingOpportunity;
  missing: boolean;
  fresh: boolean;
};

function isTradingAccountReady(account: TradingAccount | null): boolean {
  if (!account) return false;
  return isWalletDexExchange(account.exchangeSlug)
    ? account.tradingReady === true
    : account.tradingReady !== false;
}

function isStableQuotePair(legA: SpreadLeg, legB: SpreadLeg): boolean {
  return (
    legA.contractType === "perpetual" &&
    legB.contractType === "perpetual" &&
    new Set([legA.quoteAsset, legB.quoteAsset]).size === 2 &&
    new Set([legA.quoteAsset, legB.quoteAsset]).has("USDC") &&
    new Set([legA.quoteAsset, legB.quoteAsset]).has("USDT")
  );
}

export function resolveBasisSpreadQuery(
  legA: SpreadLeg | null,
  legB: SpreadLeg | null,
): BasisSpreadQuery | null {
  if (!legA || !legB || legA.baseAsset !== legB.baseAsset) {
    return null;
  }
  const venueA = legA.exchange.toLowerCase();
  const venueB = legB.exchange.toLowerCase();
  const mixedStableQuotePair = isStableQuotePair(legA, legB);
  if (
    legA.quoteAsset !== legB.quoteAsset &&
    !mixedStableQuotePair
  ) {
    return null;
  }
  const quoteAsset = mixedStableQuotePair
    ? "USDT"
    : legA.quoteAsset;
  const symbol = `${legA.baseAsset}/${quoteAsset}`;
  if (
    venueA === venueB &&
    new Set([legA.contractType, legB.contractType]).size === 2
  ) {
    return {
      venue: venueA,
      baseAsset: legA.baseAsset,
      quoteAsset,
      formula: `Perpetual Ask / Spot Ask - 1 · ${symbol}`,
    };
  }
  if (venueA !== venueB) {
    return {
      venue: venueB,
      compareVenue: venueA,
      baseAsset: legA.baseAsset,
      quoteAsset,
      venueSymbol: bboCanonicalSymbol({
        exchange: legB.exchange,
        exchangeSymbol: legB.exchangeSymbol,
        quoteAsset: legB.quoteAsset,
        globalSymbol: `${legB.baseAsset}${legB.quoteAsset}`,
      }),
      compareVenueSymbol: bboCanonicalSymbol({
        exchange: legA.exchange,
        exchangeSymbol: legA.exchangeSymbol,
        quoteAsset: legA.quoteAsset,
        globalSymbol: `${legA.baseAsset}${legA.quoteAsset}`,
      }),
      formula: `${venueB.toUpperCase()} Ask / ${venueA.toUpperCase()} Ask - 1 · ${symbol}`,
    };
  }
  return null;
}

export type ArbitrageYieldMode = "basis" | "cross";

export function resolveArbitrageYieldMode(
  legA: SpreadLeg | null,
  legB: SpreadLeg | null,
): ArbitrageYieldMode | null {
  const query = resolveBasisSpreadQuery(legA, legB);
  if (!query) return null;
  return query.compareVenue ? "cross" : "basis";
}

export function annualizeBasisAvgBps(
  avgBps: number,
  range: "24h" | "7d",
): number {
  const days = range === "24h" ? 1 : 7;
  return (avgBps / 10_000) * (365 / days) * 100;
}

export function matchFundingOpportunity(
  items: FundingOpportunity[],
  instrument: Pick<
    TraderInstrument,
    "exchange" | "exchangeSymbol" | "baseAsset" | "quoteAsset"
  >,
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

export function fundingResolverFromItems(
  items: FundingOpportunity[],
): FundingLegResolver {
  const byKey = new Map<string, FundingOpportunity>();
  for (const item of items) {
    byKey.set(fundingRequestKey(item), item);
  }
  return (leg) => byKey.get(fundingRequestKey(leg));
}

function collectPerpetualFundingLookupKeys(
  legs: Array<SpreadLeg | null | undefined>,
): FundingRateLookupKey[] {
  const keys = new Map<string, FundingRateLookupKey>();
  for (const leg of legs) {
    if (!leg || leg.contractType !== "perpetual") continue;
    const key = fundingRequestKey(leg);
    if (!keys.has(key)) {
      keys.set(key, {
        exchange: leg.exchange,
        exchangeSymbol: leg.exchangeSymbol,
        baseAsset: leg.baseAsset,
        quoteAsset: leg.quoteAsset,
      });
    }
  }
  return [...keys.values()].sort((left, right) =>
    fundingRequestKey(left).localeCompare(fundingRequestKey(right)),
  );
}

function fundingLookupSignature(keys: FundingRateLookupKey[]): string {
  return keys.map((key) => fundingRequestKey(key)).join("\n");
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

export function combinationFundingAnnualized24h(
  item: Pick<ArbitrageCombination, "legA" | "legB">,
  resolve: FundingLegResolver,
): { value: number; complete: boolean } | null {
  const perpA = item.legA.contractType === "perpetual";
  const perpB = item.legB.contractType === "perpetual";
  if (!perpA && !perpB) return null;
  const fundingA = perpA ? resolve(item.legA) : undefined;
  const fundingB = perpB ? resolve(item.legB) : undefined;
  if (perpA && perpB) {
    if (!fundingA || !fundingB) return null;
    return {
      value: annualize24h(fundingB.cumulative24h - fundingA.cumulative24h),
      complete:
        fundingA.history24hComplete !== false &&
        fundingB.history24hComplete !== false,
    };
  }
  const funding = perpA ? fundingA : fundingB;
  if (!funding) return null;
  return {
    value: annualize24h(funding.cumulative24h),
    complete: funding.history24hComplete !== false,
  };
}

type LiquidityMetricStatus = "live" | "stale" | "missing" | "not_applicable";

type LiquidityMetric = {
  value: number | null;
  status: LiquidityMetricStatus;
};

export type ChartLiquidityMetrics = {
  positionNotional: LiquidityMetric;
  dailyVolume: LiquidityMetric;
  settlementA: LiquidityMetric;
  settlementB: LiquidityMetric;
  positionLabel: string;
  volumeLabel: string;
  description: string;
  detail: string;
};

function unavailableMetricStatus(
  metrics: LiquidityMetric[],
): LiquidityMetricStatus {
  return metrics.some((metric) => metric.status === "stale")
    ? "stale"
    : "missing";
}

export function resolveChartLiquidityMetrics(
  resolve: (instrument: Pick<TraderInstrument, "exchange" | "exchangeSymbol">) =>
    | FundingLegLookupState
    | undefined,
  legA: SpreadLeg | null,
  legB: SpreadLeg | null,
): ChartLiquidityMetrics {
  if (!legA || !legB) {
    const missing = { value: null, status: "missing" as const };
    return {
      positionNotional: missing,
      dailyVolume: missing,
      settlementA: missing,
      settlementB: missing,
      positionLabel: "持仓量（OI）",
      volumeLabel: "24H 成交额",
      description: "选择双腿后显示流动性与资金费周期",
      detail: "尚未选择有效的双腿标的",
    };
  }
  const resolveLeg = (leg: SpreadLeg | null): {
    positionNotional: LiquidityMetric;
    dailyVolume: LiquidityMetric;
    settlement: LiquidityMetric;
    source?: FundingOpportunity;
  } => {
    if (!leg || leg.contractType !== "perpetual") {
      const unavailable = { value: null, status: "not_applicable" as const };
      return {
        positionNotional: unavailable,
        dailyVolume: unavailable,
        settlement: unavailable,
      };
    }
    const state = resolve(leg);
    const match = state?.missing ? undefined : state?.item;
    const status: LiquidityMetricStatus = !match
      ? "missing"
      : !state?.fresh || match.stale
        ? "stale"
        : "live";
    return {
      positionNotional: {
        value: status === "live" ? match!.positionNotional : null,
        status,
      },
      dailyVolume: {
        value: status === "live" ? match!.dailyVolume : null,
        status,
      },
      settlement: {
        value: status === "live" ? match!.settlementIntervalHours : null,
        status,
      },
      source: match,
    };
  };

  const a = resolveLeg(legA);
  const b = resolveLeg(legB);
  const perpetualLegs = [
    legA?.contractType === "perpetual" ? a : null,
    legB?.contractType === "perpetual" ? b : null,
  ].filter((leg): leg is NonNullable<typeof leg> => leg !== null);
  const spotCount = [legA, legB].filter(
    (leg) => leg?.contractType === "spot",
  ).length;
  const describeLeg = (
    label: "A" | "B",
    leg: SpreadLeg,
    metrics: typeof a,
  ): string => {
    if (leg.contractType === "spot") {
      return `${label} ${leg.exchange.toUpperCase()} ${leg.exchangeSymbol}：Spot 无资金费流动性指标`;
    }
    if (!metrics.source) {
      return `${label} ${leg.exchange.toUpperCase()} ${leg.exchangeSymbol}：暂无 funding 快照`;
    }
    const stale =
      metrics.positionNotional.status === "stale" ? " · 已过期" : "";
    return `${label} ${leg.exchange.toUpperCase()} ${leg.exchangeSymbol}：OI ${formatCurrency(metrics.source.positionNotional, true)} · 24H ${formatCurrency(metrics.source.dailyVolume, true)} · ${metrics.source.settlementIntervalHours}h · ${formatTime(metrics.source.updatedAt)}${stale}`;
  };
  const detail = [
    describeLeg("A", legA, a),
    describeLeg("B", legB, b),
  ].join("\n");

  if (perpetualLegs.length === 0) {
    const unavailable = { value: null, status: "not_applicable" as const };
    return {
      positionNotional: unavailable,
      dailyVolume: unavailable,
      settlementA: a.settlement,
      settlementB: b.settlement,
      positionLabel: "持仓量（OI）",
      volumeLabel: "24H 成交额",
      description: "Spot / Spot 无资金费流动性指标",
      detail,
    };
  }

  const aggregate = (
    key: "positionNotional" | "dailyVolume",
  ): LiquidityMetric => {
    const metrics = perpetualLegs.map((leg) => leg[key]);
    if (metrics.every((metric) => metric.status === "live")) {
      return {
        value: Math.min(...metrics.map((metric) => metric.value!)),
        status: "live",
      };
    }
    return { value: null, status: unavailableMetricStatus(metrics) };
  };

  return {
    positionNotional: aggregate("positionNotional"),
    dailyVolume: aggregate("dailyVolume"),
    settlementA: a.settlement,
    settlementB: b.settlement,
    positionLabel: spotCount === 1 ? "永续腿 OI" : "较小腿 OI",
    volumeLabel:
      spotCount === 1 ? "永续腿 24H 成交额" : "较小腿 24H 成交额",
    description:
      spotCount === 1
        ? "仅展示永续腿；Spot 无资金费流动性指标"
        : "双腿取较小流动性值",
    detail,
  };
}

function spreadCacheKey(
  query: BasisSpreadQuery,
  range: BasisSpreadRange,
): string {
  return [
    query.venue,
    query.compareVenue ?? "",
    query.baseAsset,
    query.quoteAsset,
    query.venueSymbol ?? "",
    query.compareVenueSymbol ?? "",
    range,
  ].join("|");
}

export function sortArbitrageCombinationsByCreatedAt(
  items: ArbitrageCombination[],
): ArbitrageCombination[] {
  return [...items].sort((left, right) => {
    const leftTime = Date.parse(left.createdAt);
    const rightTime = Date.parse(right.createdAt);
    if (
      Number.isFinite(leftTime) &&
      Number.isFinite(rightTime) &&
      leftTime !== rightTime
    ) {
      return leftTime - rightTime;
    }
    const createdOrder = left.createdAt.localeCompare(right.createdAt);
    return createdOrder || left.id.localeCompare(right.id);
  });
}

function mergePolledCombinations(
  incoming: ArbitrageCombination[],
  current: ArbitrageCombination[],
): ArbitrageCombination[] {
  const currentByID = new Map(current.map((item) => [item.id, item]));
  return sortArbitrageCombinationsByCreatedAt(
    incoming.map((item) => {
      const existing = currentByID.get(item.id);
      if (!existing) return item;
      const existingUpdatedAt = Date.parse(existing.updatedAt);
      const incomingUpdatedAt = Date.parse(item.updatedAt);
      return Number.isFinite(existingUpdatedAt) &&
        Number.isFinite(incomingUpdatedAt) &&
        existingUpdatedAt > incomingUpdatedAt
        ? existing
        : item;
    }),
  );
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

function formatSpreadValue(value: string | null): string {
  if (value == null) return "—";
  const parsed = Number(value);
  return Number.isFinite(parsed)
    ? `${parsed > 0 ? "+" : ""}${parsed.toFixed(2)}`
    : "—";
}

export function formatSpread(value: string | null): string {
  const formatted = formatSpreadValue(value);
  return formatted === "—" ? "—" : `${formatted} bps`;
}

function LiveBidAskSpread({
  bidSpreadBps,
  askSpreadBps,
  marketDataStale,
}: {
  bidSpreadBps: string | null;
  askSpreadBps: string | null;
  marketDataStale: boolean;
}) {
  if (marketDataStale) {
    return <>行情过期</>;
  }
  return (
    <>
      <span className="text-negative">{formatSpreadValue(bidSpreadBps)}</span>
      <span className="text-muted-foreground"> / </span>
      <span className="text-positive">{formatSpreadValue(askSpreadBps)}</span>
      <span className="text-muted-foreground"> bps</span>
    </>
  );
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

type CachedAccountFunds = {
  accountId: number;
  availableFundsUsd: string;
  sourceUpdatedAt: string;
  stale: boolean;
  status: "idle" | "loading" | "ready" | "unavailable";
};

const idleAccountFunds: CachedAccountFunds = {
  accountId: 0,
  availableFundsUsd: "",
  sourceUpdatedAt: "",
  stale: false,
  status: "idle",
};

function formatUsdAmount(value: string): string {
  const amount = Number(value);
  if (!Number.isFinite(amount)) {
    return `${value} USD`;
  }
  return `${amount.toLocaleString("en-US", {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  })} USD`;
}

function formatAvailableFundsLabel(
  accountId: number | null,
  funds: CachedAccountFunds,
): string {
  if (accountId == null) {
    return "—";
  }
  if (funds.availableFundsUsd) {
    return formatUsdAmount(funds.availableFundsUsd);
  }
  switch (funds.status) {
    case "idle":
    case "loading":
      return "…";
    case "unavailable":
      return "暂不可用";
    default:
      return "…";
  }
}

function fundsFromSnapshot(snapshot: TradingAccountSnapshot): CachedAccountFunds {
  return {
    accountId: snapshot.tradingAccountId,
    availableFundsUsd: snapshot.availableFundsUsd,
    sourceUpdatedAt: snapshot.sourceUpdatedAt,
    stale: snapshot.stale,
    status: "ready",
  };
}

const liveAccountFundsInflight = new Map<
  number,
  Promise<TradingAccountSnapshot>
>();

function loadLiveAccountFunds(id: number): Promise<TradingAccountSnapshot> {
  let pending = liveAccountFundsInflight.get(id);
  if (!pending) {
    pending = fetchLiveTradingAccountSnapshot(id).finally(() => {
      if (liveAccountFundsInflight.get(id) === pending) {
        liveAccountFundsInflight.delete(id);
      }
    });
    liveAccountFundsInflight.set(id, pending);
  }
  return pending;
}

export function resetLiveAccountFundsInflight() {
  liveAccountFundsInflight.clear();
}

function useCachedAccountFunds(accountId: number | null): CachedAccountFunds {
  const [funds, setFunds] = React.useState<CachedAccountFunds>(idleAccountFunds);
  const liveRequestedFor = React.useRef<number | null>(null);
  React.useEffect(() => {
    if (accountId == null) {
      liveRequestedFor.current = null;
      setFunds(idleAccountFunds);
      return;
    }
    const requestedId = accountId;
    liveRequestedFor.current = null;
    const controller = new AbortController();
    setFunds({
      accountId: requestedId,
      availableFundsUsd: "",
      sourceUpdatedAt: "",
      stale: false,
      status: "loading",
    });

    const applySnapshot = (snapshot: TradingAccountSnapshot) => {
      if (controller.signal.aborted) {
        return;
      }
      if (snapshot.tradingAccountId !== requestedId) {
        return;
      }
      setFunds(fundsFromSnapshot(snapshot));
    };

    const requestLive = () => {
      if (liveRequestedFor.current === requestedId) {
        return;
      }
      liveRequestedFor.current = requestedId;
      void loadLiveAccountFunds(requestedId)
        .then(applySnapshot)
        .catch((error: unknown) => {
          if (controller.signal.aborted || isAbortError(error)) {
            return;
          }
          setFunds((current) => {
            if (current.accountId !== requestedId) {
              return current;
            }
            if (current.availableFundsUsd) {
              return current;
            }
            return {
              accountId: requestedId,
              availableFundsUsd: "",
              sourceUpdatedAt: "",
              stale: false,
              status: "unavailable",
            };
          });
        });
    };

    void fetchCachedTradingAccountSnapshot(requestedId, controller.signal)
      .then((result) => {
        if (controller.signal.aborted) {
          return;
        }
        if (result.status === "missing") {
          requestLive();
          return;
        }
        applySnapshot(result.snapshot);
        if (accountSnapshotNeedsLiveRefresh(result.snapshot)) {
          requestLive();
        }
      })
      .catch((error: unknown) => {
        if (controller.signal.aborted || isAbortError(error)) {
          return;
        }
        setFunds({
          accountId: requestedId,
          availableFundsUsd: "",
          sourceUpdatedAt: "",
          stale: false,
          status: "unavailable",
        });
      });
    return () => controller.abort();
  }, [accountId]);
  return funds;
}

export function ArbitrageTradingView({
  searchParams,
}: {
  searchParams?: { get(name: string): string | null } | null;
} = {}) {
  const prefill = React.useMemo(
    () => parseArbitragePrefill(searchParams ?? null),
    [searchParams],
  );
  const prefillKey = arbitragePrefillSignature(prefill);
  const prefillRef = React.useRef(prefill);
  prefillRef.current = prefill;
  const lastAppliedPrefillKeyRef = React.useRef("");
  const pinnedPrefillRef = React.useRef<ArbitragePrefill | null>(null);
  const formSeededRef = React.useRef(false);
  const inspectedIdsRef = React.useRef(new Set<number>());
  const readinessByIdRef = React.useRef(new Map<number, TradingAccount>());
  const walletInspectStartedRef = React.useRef(false);
  const [accountsReady, setAccountsReady] = React.useState(false);
  const [accounts, setAccounts] = React.useState<TradingAccount[]>([]);
  const [accountsError, setAccountsError] = React.useState("");
  const [productName, setProductName] = React.useState("");
  const [legAExchange, setLegAExchange] = React.useState("");
  const [legBExchange, setLegBExchange] = React.useState("");
  const [legAAccountID, setLegAAccountID] = React.useState<number | null>(null);
  const [legBAccountID, setLegBAccountID] = React.useState<number | null>(null);
  const [legAInstrumentID, setLegAInstrumentID] = React.useState<number | null>(
    null,
  );
  const [legBInstrumentID, setLegBInstrumentID] = React.useState<number | null>(
    null,
  );
  const [legAContractType, setLegAContractType] =
    React.useState<TraderContractType>("perpetual");
  const [legBContractType, setLegBContractType] =
    React.useState<TraderContractType>("perpetual");
  const [period, setPeriod] = React.useState<BasisSpreadRange>("24h");
  const [targetNotional, setTargetNotional] = React.useState("10000");
  const [preferredLeg, setPreferredLeg] =
    React.useState<ArbitragePreferredLeg>("a");
  const [executionMode, setExecutionMode] =
    React.useState<ArbitrageExecutionMode>("maker_then_hedge");
  const [askThreshold, setAskThreshold] = React.useState("12");
  const [bidThreshold, setBidThreshold] = React.useState("-8");
  const [runMode, setRunMode] = React.useState<ArbitrageRunMode>("one_shot");
  const [legALeverage, setLegALeverage] = React.useState("4");
  const [legBLeverage, setLegBLeverage] = React.useState("4");
  const [exitPolicy, setExitPolicy] =
    React.useState<ArbitrageExitPolicy>("annualized");
  const [exitAnnualizedRate, setExitAnnualizedRate] = React.useState("15");
  const [exitAfterSeconds, setExitAfterSeconds] = React.useState(86400);
  const [earlyExitFunding8hEnabled, setEarlyExitFunding8hEnabled] =
    React.useState(false);
  const [earlyExitFunding8hPercent, setEarlyExitFunding8hPercent] =
    React.useState("");
  const [createLegErrors, setCreateLegErrors] = React.useState<{
    a?: string;
    b?: string;
  }>({});
  const [toast, setToast] = React.useState<{
    id: number;
    message: string;
    type: "success" | "error";
  } | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [combinationView, setCombinationView] =
    React.useState<ArbitrageView>("running");
  const [pages, setPages] = React.useState<
    Record<ArbitrageView, ArbitrageCombination[]>
  >({
    running: [],
    closed: [],
  });
  const [counts, setCounts] = React.useState<Record<ArbitrageView, number>>({
    running: 0,
    closed: 0,
  });
  const [listLoading, setListLoading] = React.useState(true);
  const [detailID, setDetailID] = React.useState<string | null>(null);
  const [selectedChartCombinationID, setSelectedChartCombinationID] =
    React.useState<string | null>(null);
  const [detail, setDetail] = React.useState<ArbitrageCombinationDetail | null>(
    null,
  );
  const [detailLoading, setDetailLoading] = React.useState(false);
  const [closingID, setClosingID] = React.useState<string | null>(null);
  const requestFastCombinationPollRef = React.useRef<() => void>(() => {});
  const [spreadHistory, setSpreadHistory] =
    React.useState<BasisSpreadHistory | null>(null);
  const [spreadLoading, setSpreadLoading] = React.useState(false);
  const [spreadError, setSpreadError] = React.useState("");
  const [spreadRetry, setSpreadRetry] = React.useState(0);
  const [yieldMetrics, setYieldMetrics] = React.useState<{
    value24h: number | null;
    value7d: number | null;
  }>({ value24h: null, value7d: null });
  const [fundingTick, setFundingTick] = React.useState(0);
  const spreadCache = React.useRef(new Map<string, SpreadCacheEntry>());
  const toastTimer = React.useRef<number | null>(null);
  const toastSequence = React.useRef(0);
  const fundingByKeyRef = React.useRef(new Map<string, FundingCacheEntry>());
  const visibleFundingKeysRef = React.useRef<FundingRateLookupKey[]>([]);

  const arbitrageAccounts = React.useMemo(
    () => accounts.filter((item) => ARBITRAGE_EXCHANGES.has(item.exchangeSlug)),
    [accounts],
  );
  const products = React.useMemo(
    () => groupAccountsByProduct(arbitrageAccounts),
    [arbitrageAccounts],
  );
  const productAccounts = React.useMemo(
    () =>
      products.find((item) => item.productName === productName)?.accounts ?? [],
    [productName, products],
  );
  const exchanges = React.useMemo(
    () =>
      Array.from(
        new Map(
          productAccounts.map((item) => [item.exchangeSlug, item.exchange]),
        ),
      ),
    [productAccounts],
  );
  const legAAccounts = productAccounts.filter(
    (item) => item.exchangeSlug === legAExchange,
  );
  const legBAccounts = productAccounts.filter(
    (item) => item.exchangeSlug === legBExchange,
  );
  const selectedLegAAccount =
    legAAccounts.find((item) => item.id === legAAccountID) ?? null;
  const selectedLegBAccount =
    legBAccounts.find((item) => item.id === legBAccountID) ?? null;
  const legAFunds = useCachedAccountFunds(legAAccountID);
  const legBFunds = useCachedAccountFunds(legBAccountID);
  const { items: legAInstruments, loading: legALoading } =
    useAccountInstruments(legAAccountID, legAContractType);
  const { items: legBInstruments, loading: legBLoading } =
    useAccountInstruments(legBAccountID, legBContractType);
  const pinnedPrefill = pinnedPrefillRef.current;
  const prefillLegAInstrument = pinnedPrefill
    ? findPrefillInstrument(legAInstruments, pinnedPrefill.legA)
    : undefined;
  const prefillLegBInstrument = pinnedPrefill
    ? findPrefillInstrument(legBInstruments, pinnedPrefill.legB)
    : undefined;
  const legAInstrument =
    legAInstruments.find((item) => item.id === legAInstrumentID) ??
    prefillLegAInstrument ??
    (!pinnedPrefill ? legAInstruments[0] : null) ??
    null;
  const legBInstrument =
    legBInstruments.find((item) => item.id === legBInstrumentID) ??
    prefillLegBInstrument ??
    (!pinnedPrefill ? legBInstruments[0] : null) ??
    null;
  const visibleLegAInstrumentID = legAInstrument?.id ?? null;
  const visibleLegBInstrumentID = legBInstrument?.id ?? null;
  const selectedChartCombination = React.useMemo(
    () =>
      (detail?.id === selectedChartCombinationID ? detail : null) ??
      [...pages.running, ...pages.closed].find(
        (item) => item.id === selectedChartCombinationID,
      ) ?? null,
    [detail, pages, selectedChartCombinationID],
  );
  const chartLegA = selectedChartCombination?.legA ?? legAInstrument;
  const chartLegB = selectedChartCombination?.legB ?? legBInstrument;
  const spreadQuery = React.useMemo(
    () => resolveBasisSpreadQuery(chartLegA, chartLegB),
    [chartLegA, chartLegB],
  );
  const yieldMode = React.useMemo(
    () => resolveArbitrageYieldMode(chartLegA, chartLegB),
    [chartLegA, chartLegB],
  );
  const chartUsesOneShotExit =
    selectedChartCombination != null
      ? selectedChartCombination.runMode === "one_shot"
      : runMode === "one_shot";
  const chartAskThreshold = chartUsesOneShotExit
    ? Number.NaN
    : Number(
        selectedChartCombination?.askThresholdBps ?? askThreshold,
      );
  const chartBidThreshold = chartUsesOneShotExit
    ? Number.NaN
    : Number(
        selectedChartCombination?.bidThresholdBps ?? bidThreshold,
      );
  const visibleFundingKeys = React.useMemo(() => {
    const legs: Array<SpreadLeg | null | undefined> = [];
    for (const row of pages[combinationView]) {
      legs.push(row.legA, row.legB);
    }
    legs.push(chartLegA, chartLegB);
    return collectPerpetualFundingLookupKeys(legs);
  }, [pages, combinationView, chartLegA, chartLegB]);
  const visibleFundingSignature = React.useMemo(
    () => fundingLookupSignature(visibleFundingKeys),
    [visibleFundingKeys],
  );
  visibleFundingKeysRef.current = visibleFundingKeys;

  const resolveFundingOpportunity = React.useCallback<FundingLegResolver>(
    (instrument) => {
      const entry = fundingByKeyRef.current.get(fundingRequestKey(instrument));
      if (!entry || entry.status !== "hit") return undefined;
      return entry.item;
    },
    [fundingTick],
  );
  const resolveFundingLegState = React.useCallback(
    (
      instrument: Pick<TraderInstrument, "exchange" | "exchangeSymbol">,
    ): FundingLegLookupState | undefined => {
      const entry = fundingByKeyRef.current.get(fundingRequestKey(instrument));
      if (!entry) return undefined;
      return {
        item: entry.status === "hit" ? entry.item : undefined,
        missing: entry.status === "missing",
        fresh: entry.fresh,
      };
    },
    [fundingTick],
  );

  React.useEffect(() => {
    const visibleFundingKeys = visibleFundingKeysRef.current;
    if (visibleFundingKeys.length === 0) {
      return;
    }
    let cancelled = false;
    let intervalTimer: number | undefined;
    let retryTimer: number | undefined;
    let failures = 0;
    const controller = new AbortController();

    const applySuccess = (
      keys: FundingRateLookupKey[],
      rows: Array<{
        key: string;
        status: "hit" | "missing";
        item?: FundingOpportunity;
      }>,
      snapshotVersion: string,
    ) => {
      const loadedAt = Date.now();
      for (let index = 0; index < keys.length; index += 1) {
        const requestKey = fundingRequestKey(keys[index]!);
        const row = rows[index];
        if (row?.status === "hit" && row.item) {
          fundingByKeyRef.current.set(requestKey, {
            status: "hit",
            item: row.item,
            snapshotVersion,
            loadedAt,
            fresh: true,
          });
        } else {
          fundingByKeyRef.current.set(requestKey, {
            status: "missing",
            snapshotVersion,
            loadedAt,
            fresh: true,
          });
        }
      }
      setFundingTick((value) => value + 1);
    };

    const applyNetworkError = (keys: FundingRateLookupKey[]) => {
      for (const key of keys) {
        const requestKey = fundingRequestKey(key);
        const existing = fundingByKeyRef.current.get(requestKey);
        if (existing) {
          fundingByKeyRef.current.set(requestKey, {
            ...existing,
            fresh: false,
          });
        }
      }
      setFundingTick((value) => value + 1);
    };

    const lookupChunks = async (keys: FundingRateLookupKey[]) => {
      const rows: Array<{
        key: string;
        status: "hit" | "missing";
        item?: FundingOpportunity;
      }> = [];
      let snapshotVersion = "";
      for (
        let offset = 0;
        offset < keys.length;
        offset += FUNDING_RATES_LOOKUP_MAX_KEYS
      ) {
        const chunk = keys.slice(offset, offset + FUNDING_RATES_LOOKUP_MAX_KEYS);
        const result = await fetchFundingRatesLookup(chunk, controller.signal);
        snapshotVersion = result.snapshotVersion;
        rows.push(...result.results);
      }
      return { rows, snapshotVersion };
    };

    const runLookup = async (keys: FundingRateLookupKey[]) => {
      if (keys.length === 0) return;
      try {
        const { rows, snapshotVersion } = await lookupChunks(keys);
        if (cancelled) return;
        applySuccess(keys, rows, snapshotVersion);
        failures = 0;
        window.clearTimeout(retryTimer);
        window.clearInterval(intervalTimer);
        intervalTimer = window.setInterval(() => {
          void runLookup(visibleFundingKeysRef.current);
        }, FUNDING_LOOKUP_REFRESH_MS);
      } catch {
        if (cancelled || controller.signal.aborted) return;
        applyNetworkError(keys);
        failures += 1;
        window.clearInterval(intervalTimer);
        const delay =
          failures <= FUNDING_LOOKUP_BACKOFF_MS.length
            ? FUNDING_LOOKUP_BACKOFF_MS[failures - 1]!
            : FUNDING_LOOKUP_REFRESH_MS;
        window.clearTimeout(retryTimer);
        retryTimer = window.setTimeout(() => {
          void runLookup(visibleFundingKeysRef.current);
        }, delay);
      }
    };

    const missing = visibleFundingKeys.filter(
      (key) => !fundingByKeyRef.current.has(fundingRequestKey(key)),
    );
    if (missing.length > 0) {
      void runLookup(missing);
    } else {
      intervalTimer = window.setInterval(() => {
        void runLookup(visibleFundingKeysRef.current);
      }, FUNDING_LOOKUP_REFRESH_MS);
    }

    return () => {
      cancelled = true;
      controller.abort();
      window.clearInterval(intervalTimer);
      window.clearTimeout(retryTimer);
    };
  }, [visibleFundingSignature]);

  const showToast = React.useCallback(
    (message: string, type: "success" | "error" = "error") => {
      if (toastTimer.current !== null) {
        window.clearTimeout(toastTimer.current);
      }
      const id = ++toastSequence.current;
      setToast({ id, message, type });
      toastTimer.current = window.setTimeout(() => {
        setToast((current) => (current?.id === id ? null : current));
        toastTimer.current = null;
      }, 1000);
    },
    [],
  );

  React.useEffect(
    () => () => {
      if (toastTimer.current !== null) {
        window.clearTimeout(toastTimer.current);
      }
    },
    [],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    let cancelled = false;
    void (async () => {
      try {
        const items = await fetchTradingAccounts(controller.signal);
        if (cancelled) return;
        const filtered = items.filter((item) =>
          ARBITRAGE_EXCHANGES.has(item.exchangeSlug),
        );
        const checkingAccounts = filtered.map((item) => {
          if (!isWalletDexExchange(item.exchangeSlug)) return item;
          const cached = readinessByIdRef.current.get(item.id);
          if (cached) return applyTradingReadiness(item, cached);
          return applyTradingReadiness(item, {
            tradingReady: false,
            tradingStatus: "checking",
            tradingUnavailableCode: "",
            tradingUnavailableReason: "",
          });
        });
        const groups = groupAccountsByProduct(checkingAccounts);
        setAccounts(checkingAccounts);
        setAccountsReady(true);
        if (!formSeededRef.current) {
          const current = prefillRef.current;
          const key = arbitragePrefillSignature(current);
          const pending = Boolean(
            current &&
              key &&
              lastAppliedPrefillKeyRef.current !== key,
          );
          if (!pending) {
            formSeededRef.current = true;
            const defaults = defaultArbitrageLegs(groups);
            setProductName(defaults.productName);
            setLegAExchange(defaults.legAExchange);
            setLegAAccountID(defaults.legAAccountID);
            setLegBExchange(defaults.legBExchange);
            setLegBAccountID(defaults.legBAccountID);
            setLegAContractType("perpetual");
            setLegBContractType("perpetual");
            setLegALeverage("4");
            setLegBLeverage("4");
          }
        }
        const toInspect = checkingAccounts.filter(
          (item) =>
            isWalletDexExchange(item.exchangeSlug) &&
            !inspectedIdsRef.current.has(item.id),
        );
        if (walletInspectStartedRef.current || toInspect.length === 0) {
          return;
        }
        walletInspectStartedRef.current = true;
        const inspectedById = new Map<number, TradingAccount>();
        await Promise.all(
          toInspect.map(async (item) => {
            inspectedIdsRef.current.add(item.id);
            try {
              const next = applyTradingReadiness(
                item,
                await inspectTradingReadiness(item.id, controller.signal),
              );
              inspectedById.set(item.id, next);
            } catch (error) {
              if (cancelled || controller.signal.aborted || isAbortError(error)) {
                throw error;
              }
              inspectedById.set(
                item.id,
                applyTradingReadiness(item, {
                  tradingReady: false,
                  tradingStatus: "venue_unavailable",
                  tradingUnavailableCode: "venue_unavailable",
                  tradingUnavailableReason:
                    error instanceof Error ? error.message : "交易能力检查失败",
                }),
              );
            }
          }),
        );
        if (cancelled || controller.signal.aborted) return;
        for (const [id, account] of inspectedById) {
          readinessByIdRef.current.set(id, account);
        }
        setAccounts((current) =>
          current.map((item) => inspectedById.get(item.id) ?? item),
        );
      } catch (error: unknown) {
        if (cancelled || controller.signal.aborted || isAbortError(error)) {
          return;
        }
        setAccountsError(
          error instanceof Error ? error.message : "账户加载失败",
        );
      }
    })();
    return () => {
      cancelled = true;
      controller.abort();
    };
  }, []);

  React.useEffect(() => {
    if (!accountsReady) return;
    if (!prefill || !prefillKey) {
      lastAppliedPrefillKeyRef.current = "";
      return;
    }
    if (lastAppliedPrefillKeyRef.current === prefillKey) return;
    const groups = groupAccountsByProduct(
      accounts.filter((item) => ARBITRAGE_EXCHANGES.has(item.exchangeSlug)),
    );
    const next = prefillArbitrageLegs(prefill, groups);
    formSeededRef.current = true;
    pinnedPrefillRef.current = prefill;
    lastAppliedPrefillKeyRef.current = prefillKey;
    setProductName(next.productName);
    setLegAExchange(next.legAExchange);
    setLegBExchange(next.legBExchange);
    setLegAContractType(next.legAContractType);
    setLegBContractType(next.legBContractType);
    setLegALeverage(next.legALeverage);
    setLegBLeverage(next.legBLeverage);
    setLegAAccountID(next.legAAccountID);
    setLegBAccountID(next.legBAccountID);
    setLegAInstrumentID(null);
    setLegBInstrumentID(null);
    setSelectedChartCombinationID(null);
  }, [accounts, accountsReady, prefill, prefillKey]);

  const refreshCombinations = React.useCallback(
    async (signal?: AbortSignal) => {
      const [running, closed] = await Promise.all([
        fetchArbitrageCombinations("running", { limit: 50, signal }),
        fetchArbitrageCombinations("closed", { limit: 50, signal }),
      ]);
      const hasClosing = running.items.some((item) => item.status === "closing");
      setPages((current) => ({
        running: mergePolledCombinations(running.items, current.running),
        closed: mergePolledCombinations(closed.items, current.closed),
      }));
      setCounts({ running: running.total, closed: closed.total });
      setListLoading(false);
      return hasClosing;
    },
    [],
  );

  React.useEffect(() => {
    let stopped = false;
    let timer = 0;
    let inFlight: AbortController | null = null;
    let pendingFastPoll = false;
    const schedule = (delayMs: number) => {
      if (stopped) return;
      timer = window.setTimeout(() => void poll(), delayMs);
    };
    const poll = async () => {
      if (stopped || document.visibilityState === "hidden") return;
      window.clearTimeout(timer);
      inFlight?.abort();
      const request = new AbortController();
      inFlight = request;
      let hasClosing = false;
      let failed = false;
      try {
        hasClosing = await refreshCombinations(request.signal);
      } catch (error) {
        if (
          !stopped &&
          !isAbortError(error) &&
          !request.signal.aborted
        ) {
          failed = true;
          showToast(
            error instanceof Error ? error.message : "套利组合加载失败",
          );
          setListLoading(false);
        }
      } finally {
        if (inFlight === request) inFlight = null;
      }
      if (
        !stopped &&
        document.visibilityState === "visible" &&
        !request.signal.aborted
      ) {
        if (pendingFastPoll) {
          pendingFastPoll = false;
          schedule(COMBINATION_CLOSING_POLL_MS);
        } else if (failed) {
          schedule(COMBINATION_POLL_MS);
        } else {
          schedule(
            hasClosing ? COMBINATION_CLOSING_POLL_MS : COMBINATION_POLL_MS,
          );
        }
      }
    };
    const requestFastCombinationPoll = () => {
      if (stopped) return;
      if (inFlight) {
        pendingFastPoll = true;
        return;
      }
      window.clearTimeout(timer);
      schedule(COMBINATION_CLOSING_POLL_MS);
    };
    requestFastCombinationPollRef.current = requestFastCombinationPoll;
    const onVisibility = () => {
      if (document.visibilityState === "visible") {
        window.clearTimeout(timer);
        void poll();
      }
    };
    void poll();
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      stopped = true;
      window.clearTimeout(timer);
      inFlight?.abort();
      requestFastCombinationPollRef.current = () => {};
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [refreshCombinations, showToast]);

  const pairError = React.useMemo(() => {
    if (!legAInstrument || !legBInstrument) return "请选择两侧合约";
    if (legAInstrument.id === legBInstrument.id) {
      return "Leg A 与 Leg B 不能使用同一个合约";
    }
    if (legAInstrument.baseAsset !== legBInstrument.baseAsset) {
      return "Leg A 与 Leg B 的基础资产必须一致";
    }
    if (
      legAInstrument.quoteAsset !== legBInstrument.quoteAsset &&
      !isStableQuotePair(legAInstrument, legBInstrument)
    ) {
      return "Leg A 与 Leg B 的 Quote Asset 必须一致";
    }
    if (
      legAInstrument.exchange === legBInstrument.exchange &&
      legAInstrument.contractType === legBInstrument.contractType
    ) {
      return "单所套利必须配对一个现货与一个永续合约";
    }
    return "";
  }, [legAInstrument, legBInstrument]);
  const visibleSpreadHistory =
    spreadHistory &&
    spreadQuery &&
    spreadHistory.venue.toLowerCase() === spreadQuery.venue &&
    (spreadHistory.compareVenue?.toLowerCase() ?? "") ===
      (spreadQuery.compareVenue ?? "") &&
    spreadHistory.baseAsset === spreadQuery.baseAsset &&
    spreadHistory.quoteAsset === spreadQuery.quoteAsset &&
    spreadHistory.range === period
      ? spreadHistory
      : null;
  const spreadSummary =
    visibleSpreadHistory?.availability === "available"
      ? visibleSpreadHistory.summary
      : null;
  const chartLiquidityMetrics = React.useMemo(
    () =>
      resolveChartLiquidityMetrics(
        resolveFundingLegState,
        chartLegA,
        chartLegB,
      ),
    [chartLegA, chartLegB, resolveFundingLegState],
  );

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
      if (
        !force &&
        cached &&
        Date.now() - cached.loadedAt < SPREAD_CACHE_TTL_MS
      ) {
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
          spreadQuery.venueSymbol,
          spreadQuery.compareVenueSymbol,
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
          setSpreadError(
            error instanceof Error ? error.message : "价差走势加载失败",
          );
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
    if (!spreadQuery || !chartLegA || !chartLegB || !yieldMode) {
      const timer = window.setTimeout(
        () => setYieldMetrics({ value24h: null, value7d: null }),
        0,
      );
      return () => window.clearTimeout(timer);
    }
    if (yieldMode === "cross") {
      const fundingA = resolveFundingOpportunity(chartLegA);
      const fundingB = resolveFundingOpportunity(chartLegB);
      const yield_ = crossExchangeWindowYield(
        fundingA && !fundingA.stale ? fundingA : undefined,
        fundingB && !fundingB.stale ? fundingB : undefined,
      );
      const timer = window.setTimeout(
        () => setYieldMetrics(yield_ ?? { value24h: null, value7d: null }),
        0,
      );
      return () => window.clearTimeout(timer);
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
        spreadQuery.venueSymbol,
        spreadQuery.compareVenueSymbol,
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
  }, [
    spreadQuery,
    yieldMode,
    chartLegA,
    chartLegB,
    spreadRetry,
    resolveFundingOpportunity,
  ]);

  const targetValue = Number(targetNotional);
  const leverageAValue = Number(legALeverage);
  const leverageBValue = Number(legBLeverage);
  const leverageValid =
    (legAContractType === "spot"
      ? leverageAValue === 1
      : leverageAValue >= 1) &&
    (legBContractType === "spot"
      ? leverageBValue === 1
      : leverageBValue >= 1);
  const oneShotValid =
    runMode !== "one_shot" ||
    (exitPolicy === "annualized"
      ? annualizedPercentToRatio(exitAnnualizedRate) != null
      : exitPolicy === "time" &&
        [3600, 14400, 28800, 86400, 604800].includes(exitAfterSeconds));
  const funding8hFloorRatio = signedAnnualizedPercentToRatio(
    earlyExitFunding8hPercent,
  );
  const funding8hFloorValid =
    runMode !== "one_shot" ||
    !earlyExitFunding8hEnabled ||
    funding8hFloorRatio != null;
  const spreadThresholdsValid =
    runMode !== "spread" ||
    [askThreshold, bidThreshold].every((value) => {
      const trimmed = value.trim();
      return trimmed !== "" && Number.isFinite(Number(trimmed));
    });
  const parametersValid =
    targetValue > 0 &&
    leverageValid &&
    oneShotValid &&
    funding8hFloorValid &&
    spreadThresholdsValid;
  const canCreate =
    !busy &&
    !pairError &&
    parametersValid &&
    legAAccountID !== null &&
    legBAccountID !== null &&
    isTradingAccountReady(selectedLegAAccount) &&
    isTradingAccountReady(selectedLegBAccount);

  function clearSelectedChartCombination() {
    setSelectedChartCombinationID(null);
  }

  function chooseExchange(
    value: string,
    setExchange: (next: string) => void,
    setAccountID: (next: number | null) => void,
    setInstrumentID: (next: number | null) => void,
    setContractType: (next: TraderContractType) => void,
    setLeverage: (next: string) => void,
  ) {
    clearSelectedChartCombination();
    setExchange(value);
    setAccountID(
      productAccounts.find((item) => item.exchangeSlug === value)?.id ?? null,
    );
    if (isWalletDexExchange(value)) {
      setContractType("perpetual");
      setLeverage("4");
    }
    setInstrumentID(null);
  }

  function chooseProduct(value: string) {
    clearSelectedChartCombination();
    const nextAccounts =
      products.find((item) => item.productName === value)?.accounts ?? [];
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
    if (first && isWalletDexExchange(first.exchangeSlug)) {
      setLegAContractType("perpetual");
      setLegALeverage("4");
    }
    if (second && isWalletDexExchange(second.exchangeSlug)) {
      setLegBContractType("perpetual");
      setLegBLeverage("4");
    }
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
    )
      return;
    setBusy(true);
    setCreateLegErrors({});
    try {
      const selectedAccounts = [selectedLegAAccount, selectedLegBAccount].filter(
        (account): account is TradingAccount =>
          Boolean(account && isWalletDexExchange(account.exchangeSlug)),
      );
      const readiness = await Promise.all(
        Array.from(
          new Map(selectedAccounts.map((account) => [account.id, account])).values(),
        ).map(async (account) => ({
          account,
          ready: await inspectTradingReadiness(account.id),
        })),
      );
      if (readiness.length > 0) {
        setAccounts((current) =>
          current.map((account) => {
            const result = readiness.find(
              (item) => item.account.id === account.id,
            );
            return result
              ? applyTradingReadiness(account, result.ready)
              : account;
          }),
        );
        const unavailable = readiness.find((item) => !item.ready.tradingReady);
        if (unavailable) {
          throw new Error(
            unavailable.ready.tradingUnavailableReason || "当前账户不可交易",
          );
        }
      }
      const created = await createArbitrageCombination({
        productName,
        legAAccountId: legAAccountID,
        legAInstrumentId: visibleLegAInstrumentID,
        legBAccountId: legBAccountID,
        legBInstrumentId: visibleLegBInstrumentID,
        askThresholdBps: runMode === "one_shot" ? "" : askThreshold.trim(),
        bidThresholdBps: runMode === "one_shot" ? "" : bidThreshold.trim(),
        targetNotional: targetNotional.trim(),
        preferredLeg,
        executionMode,
        runMode,
        entryDirection: runMode === "one_shot" ? "ask" : "",
        legALeverage: legALeverage.trim(),
        legBLeverage: legBLeverage.trim(),
        exitPolicy: runMode === "one_shot" ? exitPolicy : "",
        exitAnnualizedRate:
          runMode === "one_shot" && exitPolicy === "annualized"
            ? (annualizedPercentToRatio(exitAnnualizedRate) ?? "")
            : "",
        exitAfterSeconds:
          runMode === "one_shot" && exitPolicy === "time"
            ? exitAfterSeconds
            : 0,
        earlyExitFunding8hAnnualizedFloor:
          runMode === "one_shot" && earlyExitFunding8hEnabled
            ? (funding8hFloorRatio ?? "")
            : "",
      });
      showToast(`套利组合 ${created.id} 已创建`, "success");
      setCombinationView("running");
      await refreshCombinations();
    } catch (error) {
      if (error instanceof ArbitrageCreateFailure) {
        const venueError =
          error.code === "leverage_apply_failed"
            ? error.details.error?.trim()
            : "";
        const displayMessage =
          venueError && !error.message.includes(venueError)
            ? `${error.message}：${venueError}`
            : error.message;
        if (error.leg === "a" || error.leg === "b") {
          setCreateLegErrors({ [error.leg]: displayMessage });
        }
        const applied = error.details.appliedLegs;
        const hasAppliedLegs = Boolean(applied) && applied !== "[]";
        const uncertain = error.details.uncertain === "true";
        if (uncertain) {
          showToast(
            hasAppliedLegs
              ? `${displayMessage}（可能已修改杠杆的腿：${applied}）`
              : displayMessage,
          );
        } else {
          showToast(
            hasAppliedLegs
              ? `${displayMessage}（已修改杠杆的腿：${applied}）`
              : displayMessage,
          );
        }
      } else {
        showToast(error instanceof Error ? error.message : "套利组合创建失败");
      }
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
      showToast(
        error instanceof Error ? error.message : "套利组合详情加载失败",
      );
    } finally {
      setDetailLoading(false);
    }
  }

  function applyCombinationUpdate(updated: ArbitrageCombination) {
    setPages((current) => ({
      running: current.running.map((item) =>
        item.id === updated.id ? updated : item,
      ),
      closed: current.closed.map((item) =>
        item.id === updated.id ? updated : item,
      ),
    }));
    setDetail((current) =>
      current?.id === updated.id ? { ...current, ...updated } : current,
    );
  }

  async function closeCombination(id: string) {
    if (
      !window.confirm(
        "确认关闭此套利组合？系统将立即停止新交易、撤销活动挂单，并开始分批平掉该组合产生的已配对仓位。全部可执行配对仓位平完后，组合才会关闭；未配平、无法成交或状态无法确认的敞口可能需要人工处理。",
      )
    ) {
      return;
    }
    setClosingID(id);
    try {
      const updated = await closeArbitrageCombination(id);
      applyCombinationUpdate(updated);
      showToast(`套利组合 ${id} 正在关闭`, "success");
      requestFastCombinationPollRef.current();
    } catch (error) {
      showToast(error instanceof Error ? error.message : "关闭套利组合失败");
    } finally {
      setClosingID(null);
    }
  }

  return (
    <div className="min-w-0 space-y-3 p-3 lg:p-4">
      {toast ? (
        <div
          role={toast.type === "error" ? "alert" : "status"}
          className={cn(
            "fixed top-4 right-4 z-50 max-w-sm rounded-lg border bg-background px-4 py-2 text-sm shadow-lg",
            toast.type === "error" ? "text-destructive" : "text-positive",
          )}
        >
          {toast.message}
        </div>
      ) : null}
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

      <section className="grid gap-3 xl:grid-cols-[minmax(0,2fr)_minmax(420px,0.95fr)]">
        <div className="grid min-w-0 grid-cols-1 items-stretch gap-3 md:grid-cols-[minmax(0,1fr)_40px_minmax(0,1fr)]">
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
          leverage={legALeverage}
          availableFundsLabel={formatAvailableFundsLabel(
            legAAccountID,
            legAFunds,
          )}
          error={createLegErrors.a}
          onExchangeChange={(value) =>
            chooseExchange(
              value,
              setLegAExchange,
              setLegAAccountID,
              setLegAInstrumentID,
              setLegAContractType,
              setLegALeverage,
            )
          }
          onAccountChange={(value) => {
            clearSelectedChartCombination();
            setLegAAccountID(value);
            setLegAInstrumentID(null);
          }}
          onContractTypeChange={(value) => {
            clearSelectedChartCombination();
            setLegAContractType(value);
            setLegALeverage(defaultLegLeverage(value));
            setLegAInstrumentID(null);
          }}
          onLeverageChange={setLegALeverage}
          onInstrumentChange={(value) => {
            clearSelectedChartCombination();
            setLegAInstrumentID(value);
          }}
        />
        <div className="hidden items-center justify-center md:flex">
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
          leverage={legBLeverage}
          availableFundsLabel={formatAvailableFundsLabel(
            legBAccountID,
            legBFunds,
          )}
          error={createLegErrors.b}
          onExchangeChange={(value) =>
            chooseExchange(
              value,
              setLegBExchange,
              setLegBAccountID,
              setLegBInstrumentID,
              setLegBContractType,
              setLegBLeverage,
            )
          }
          onAccountChange={(value) => {
            clearSelectedChartCombination();
            setLegBAccountID(value);
            setLegBInstrumentID(null);
          }}
          onContractTypeChange={(value) => {
            clearSelectedChartCombination();
            setLegBContractType(value);
            setLegBLeverage(defaultLegLeverage(value));
            setLegBInstrumentID(null);
          }}
          onLeverageChange={setLegBLeverage}
          onInstrumentChange={(value) => {
            clearSelectedChartCombination();
            setLegBInstrumentID(value);
          }}
        />
        </div>
        <div className="min-w-0 rounded-xl border bg-card">
          <div className="flex items-center gap-2 border-b px-3 py-2">
            <Gauge className="size-4 shrink-0 text-primary" />
            <div className="min-w-0">
              <h3 className="text-sm font-medium">组合执行参数</h3>
              <p className="text-[11px] text-muted-foreground">
                运行模式、目标仓位与触发规则
              </p>
            </div>
          </div>
          <div className="grid gap-2 p-2.5 sm:grid-cols-2">
            <div className="grid gap-1">
              <Field label="运行模式">
                <select
                  value={runMode}
                  onChange={(event) =>
                    setRunMode(event.target.value as ArbitrageRunMode)
                  }
                  className={selectClassName}
                >
                  <option value="spread">价差触发</option>
                  <option value="one_shot">一次性建仓</option>
                </select>
              </Field>
              {runMode === "one_shot" ? (
                <p className="text-[11px] leading-4 text-muted-foreground">
                  固定买 Leg A / 卖 Leg B
                </p>
              ) : null}
            </div>
            <Field label="目标仓位">
              <UnitInput
                value={targetNotional}
                onChange={setTargetNotional}
                unit="USDT"
              />
            </Field>
            {runMode === "spread" ? (
              <>
                <Field label="开仓阈值">
                  <UnitInput
                    value={askThreshold}
                    onChange={setAskThreshold}
                    unit="bps"
                  />
                </Field>
                <Field label="平仓阈值">
                  <UnitInput
                    value={bidThreshold}
                    onChange={setBidThreshold}
                    unit="bps"
                  />
                </Field>
              </>
            ) : (
              <>
                <Field label="退出方式">
                  <select
                    value={exitPolicy}
                    onChange={(event) =>
                      setExitPolicy(event.target.value as ArbitrageExitPolicy)
                    }
                    className={selectClassName}
                  >
                    <option value="annualized">年化达标退出</option>
                    <option value="time">固定持仓时间</option>
                  </select>
                </Field>
                {exitPolicy === "annualized" ? (
                  <Field label="退出年化">
                    <UnitInput
                      value={exitAnnualizedRate}
                      onChange={setExitAnnualizedRate}
                      unit="%"
                      placeholder="15"
                    />
                  </Field>
                ) : (
                  <Field label="持仓时间">
                    <select
                      value={exitAfterSeconds}
                      onChange={(event) =>
                        setExitAfterSeconds(Number(event.target.value))
                      }
                      className={selectClassName}
                    >
                      {EXIT_AFTER_OPTIONS.map((item) => (
                        <option key={item.value} value={item.value}>
                          {item.label}
                        </option>
                      ))}
                    </select>
                  </Field>
                )}
                <label className="flex flex-wrap items-center gap-2 sm:col-span-2">
                  <input
                    type="checkbox"
                    checked={earlyExitFunding8hEnabled}
                    onChange={(event) =>
                      setEarlyExitFunding8hEnabled(event.target.checked)
                    }
                    aria-label="启用8h资金费年化提前退出"
                  />
                  {earlyExitFunding8hEnabled ? (
                    <>
                      <span className="text-sm">8h资金费年化&lt;</span>
                      <input
                        value={earlyExitFunding8hPercent}
                        onChange={(event) =>
                          setEarlyExitFunding8hPercent(event.target.value)
                        }
                        aria-label="8h资金费年化阈值"
                        inputMode="decimal"
                        className="h-8 w-20 rounded-md border bg-background px-2 font-mono text-sm"
                        placeholder="xx"
                      />
                      <span className="text-sm">%</span>
                      {!funding8hFloorValid ? (
                        <span className="text-xs text-destructive">
                          请填写有效年化阈值
                        </span>
                      ) : null}
                    </>
                  ) : (
                    <span className="text-sm">8h资金费年化提前退出</span>
                  )}
                </label>
              </>
            )}
            <Field label="执行优先级">
              <select
                value={preferredLeg}
                onChange={(event) =>
                  setPreferredLeg(event.target.value as ArbitragePreferredLeg)
                }
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
            <Button
              className="w-full"
              disabled={!canCreate}
              onClick={() => void submitCreate()}
            >
              <Play data-icon="inline-start" />
              {busy ? "正在检查并创建…" : "创建套利组合"}
            </Button>
          </div>
        </div>
      </section>

      <div
        className={cn(
          "flex items-center gap-2 rounded-lg border px-3 py-2 text-xs",
          pairError
            ? "border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-300"
            : "border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300",
        )}
      >
        {pairError ? (
          <CircleAlert className="size-3.5" />
        ) : (
          <CheckCircle2 className="size-3.5" />
        )}
        {pairError ||
          `${spreadQuery?.baseAsset}/${spreadQuery?.quoteAsset} 配对有效 · Ask: A 多 / B 空 · Bid: A 空 / B 多`}
      </div>

      <section className="min-w-0 space-y-3">
          <div className="min-w-0 rounded-xl border bg-card">
            <div className="space-y-3 border-b px-4 py-3">
              <div className="flex flex-wrap items-center gap-3">
                <div className="mr-auto min-w-0">
                  <div className="flex items-center gap-2 text-sm font-medium">
                    <TrendingUp className="size-4 text-primary" />
                    价差走势
                  </div>
                  <p className="mt-0.5 truncate text-[11px] text-muted-foreground">
                    {spreadQuery?.formula ??
                      "Leg A / Leg B 配对后展示 Best Ask 价差"}
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
                  value={
                    spreadSummary ? formatBps(spreadSummary.currentBps) : "—"
                  }
                />
                <HeaderMetric
                  label="周期均值"
                  value={spreadSummary ? formatBps(spreadSummary.avgBps) : "—"}
                />
                <HeaderMetric
                  label="覆盖率"
                  value={
                    spreadSummary ? formatCoverage(spreadSummary.coverage) : "—"
                  }
                />
                {yieldMode ? (
                  <>
                    <HeaderMetric
                      label={
                        yieldMode === "basis" ? "24H 差值年化" : "24H 窗口年化"
                      }
                      value={
                        yieldMetrics.value24h == null
                          ? "—"
                          : formatPercent(yieldMetrics.value24h, 1)
                      }
                      valueClassName={rateColor(yieldMetrics.value24h)}
                    />
                    <HeaderMetric
                      label={
                        yieldMode === "basis" ? "7D 差值年化" : "7D 窗口年化"
                      }
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
              <ChartLiquidityMetricRow metrics={chartLiquidityMetrics} />
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
                    spreadCache.current.delete(
                      spreadCacheKey(spreadQuery, period),
                    );
                    setSpreadRetry((value) => value + 1);
                  }}
                >
                  <RefreshCw className="size-3" />
                  重试
                </button>
              </SpreadChartState>
            ) : visibleSpreadHistory?.availability === "unavailable" ? (
              <SpreadChartState>
                当前组合暂无对应 Best Ask BBO 数据
              </SpreadChartState>
            ) : visibleSpreadHistory &&
              visibleSpreadHistory.points.length === 0 ? (
              <SpreadChartState>当前周期暂无配对价差</SpreadChartState>
            ) : visibleSpreadHistory ? (
              <SpreadChart
                history={visibleSpreadHistory}
                askThreshold={Number(chartAskThreshold)}
                bidThreshold={Number(chartBidThreshold)}
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
            resolveFunding={resolveFundingOpportunity}
            counts={counts}
            loading={listLoading}
            detailID={detailID}
            selectedChartID={selectedChartCombinationID}
            detail={detail}
            detailLoading={detailLoading}
            onDetailChange={toggleDetail}
            onChartSelect={(item) => setSelectedChartCombinationID(item.id)}
            onUpdated={applyCombinationUpdate}
            closingID={closingID}
            onClose={closeCombination}
          />
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
  leverage,
  availableFundsLabel,
  error,
  onExchangeChange,
  onAccountChange,
  onContractTypeChange,
  onLeverageChange,
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
  leverage: string;
  availableFundsLabel: string;
  error?: string;
  onExchangeChange: (value: string) => void;
  onAccountChange: (value: number | null) => void;
  onContractTypeChange: (value: TraderContractType) => void;
  onLeverageChange: (value: string) => void;
  onInstrumentChange: (value: number | null) => void;
}) {
  const dex = isWalletDexExchange(exchange);
  const selectedAccount =
    accounts.find((account) => account.id === accountID) ?? null;
  return (
    <div className="flex h-full flex-col rounded-xl border bg-card">
      <div className="flex items-center gap-2 border-b px-3 py-2">
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
      <div className="flex min-h-0 flex-1 flex-col p-2.5">
        <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
          <Field label="交易所">
            <select
              value={exchange}
              onChange={(event) => onExchangeChange(event.target.value)}
              className={selectClassName}
            >
              {exchanges.map(([slug, name]) => (
                <option key={slug} value={slug}>
                  {name}
                </option>
              ))}
            </select>
          </Field>
          <Field label="账户">
            <select
              value={accountID ?? ""}
              onChange={(event) =>
                onAccountChange(Number(event.target.value) || null)
              }
              className={selectClassName}
            >
              <option value="" disabled>
                请选择可交易账户
              </option>
              {accounts.map((account) => (
                <option
                  key={account.id}
                  value={account.id}
                  disabled={!isTradingAccountReady(account)}
                >
                  {isTradingAccountReady(account)
                    ? account.accountName
                    : tradingAccountStatusLabel(account)}
                </option>
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
              disabled={dex}
            >
              <option value="perpetual">永续合约</option>
              {!dex ? <option value="spot">现货</option> : null}
            </select>
          </Field>
          <Field label="杠杆">
            <UnitInput
              value={leverage}
              onChange={onLeverageChange}
              unit="x"
              disabled={contractType === "spot"}
            />
          </Field>
          <Field label="Symbol">
            <div className="relative">
              <select
                value={instrumentID ?? ""}
                onChange={(event) =>
                  onInstrumentChange(Number(event.target.value) || null)
                }
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
        {error ? (
          <p className="pt-2 text-xs text-destructive">{error}</p>
        ) : null}
        {selectedAccount && !isTradingAccountReady(selectedAccount) ? (
          <p className="pt-2 text-xs text-amber-700 dark:text-amber-300">
            {selectedAccount.tradingStatus === "checking" ||
            selectedAccount.tradingStatus === ""
              ? "交易能力检查中"
              : selectedAccount.tradingUnavailableReason || "当前账户不可交易"}
          </p>
        ) : null}
        <div className="mt-auto flex min-h-8 items-center justify-end gap-2 pt-2">
          <span className="text-[10px] font-medium tracking-wide text-muted-foreground uppercase">
            可用资金
          </span>
          <span className="font-mono text-sm tabular-nums text-muted-foreground">
            {availableFundsLabel}
          </span>
        </div>
      </div>
    </div>
  );
}

const COMBINATION_TABLE_COLUMNS = 11;

function ArbitrageCombinationList({
  view,
  onViewChange,
  rows,
  resolveFunding,
  counts,
  loading,
  detailID,
  selectedChartID,
  detail,
  detailLoading,
  onDetailChange,
  onChartSelect,
  onUpdated,
  closingID,
  onClose,
}: {
  view: ArbitrageView;
  onViewChange: (view: ArbitrageView) => void;
  rows: ArbitrageCombination[];
  resolveFunding: FundingLegResolver;
  counts: Record<ArbitrageView, number>;
  loading: boolean;
  detailID: string | null;
  selectedChartID: string | null;
  detail: ArbitrageCombinationDetail | null;
  detailLoading: boolean;
  onDetailChange: (id: string) => void;
  onChartSelect: (item: ArbitrageCombination) => void;
  onUpdated: (item: ArbitrageCombination) => void;
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
                "min-w-24 rounded-md px-3 py-1.5 text-center text-xs font-medium tabular-nums transition-colors",
                view === item
                  ? "bg-background text-foreground shadow-sm"
                  : "text-muted-foreground hover:text-foreground",
              )}
            >
              {item === "running"
                ? `运行中 ${counts.running}`
                : `已关闭 ${counts.closed}`}
            </button>
          ))}
        </div>
      </div>

      <div className="min-w-0 overflow-x-auto">
        <table className="w-full min-w-[1760px] table-fixed text-left text-xs">
          <colgroup>
            <col className="w-[180px]" />
            <col className="w-[110px]" />
            <col className="w-[180px]" />
            <col className="w-[200px]" />
            <col className="w-[130px]" />
            <col className="w-[120px]" />
            <col className="w-[160px]" />
            <col className="w-[150px]" />
            <col className="w-[260px]" />
            <col className="w-[150px]" />
            <col className="w-[120px]" />
          </colgroup>
          <thead className="border-b bg-muted/20 text-[10px] tracking-wide text-muted-foreground uppercase">
            <tr>
              <th className="px-4 py-2.5 font-medium">交易所</th>
              <th className="px-3 py-2.5 font-medium">币对</th>
              <th className="px-3 py-2.5 font-medium">运行模式</th>
              <th className="px-3 py-2.5 text-right font-medium">
                实时 Bid/Ask Spread
              </th>
              <th className="px-3 py-2.5 text-right font-medium">持仓均价差</th>
              <th
                className="px-3 py-2.5 text-right font-medium"
                title="跨所为双腿 24h 累计资金费差年化（B−A）；期现为永续腿 24h 资金费年化。与已实现净年化无关。"
              >
                24H 年化
              </th>
              <th className="px-3 py-2.5 text-right font-medium">
                累计资金费收入
              </th>
              <th
                className="px-3 py-2.5 text-right font-medium"
                title="已结算资金费 + 已实现价差 − 预估手续费；开仓价差作成本、平仓后才计入；未满一个资金费周期时按周期截断；非交易所账单口径。不含持仓浮盈。"
              >
                已实现净年化
              </th>
              <th className="px-3 py-2.5 text-right font-medium">
                目标仓位/当前仓位
              </th>
              <th className="px-3 py-2.5 font-medium">运行状态</th>
              <th className="px-4 py-2.5 text-right font-medium">操作</th>
            </tr>
          </thead>
          <tbody className="divide-y">
            {rows.map((item) => {
              const currentNotional = currentVenuePositionNotional(item);
              const currentNotionalValue =
                currentNotional === null
                  ? Number.NaN
                  : Number(currentNotional);
              const targetNotionalValue = Number(item.targetNotional);
              const progress =
                Number.isFinite(currentNotionalValue) &&
                Number.isFinite(targetNotionalValue) &&
                targetNotionalValue > 0
                  ? Math.min(
                      100,
                      (Math.abs(currentNotionalValue) / targetNotionalValue) *
                        100,
                    )
                  : 0;
              const expanded = detailID === item.id;
              const fundingAnnualized24h = combinationFundingAnnualized24h(
                item,
                resolveFunding,
              );
              return (
                <React.Fragment key={item.id}>
                  <tr
                    className={cn(
                      "h-16 cursor-pointer transition-colors hover:bg-muted/20",
                      expanded && "bg-primary/[0.04]",
                      selectedChartID === item.id &&
                        "ring-1 ring-inset ring-primary/30",
                    )}
                    onClick={() => {
                      onChartSelect(item);
                      void onDetailChange(item.id);
                    }}
                  >
                    <td className="overflow-hidden px-4 py-3">
                      <div className="truncate whitespace-nowrap font-medium">
                        {item.legA.exchange} ↔ {item.legB.exchange}
                      </div>
                      <div className="mt-0.5 truncate whitespace-nowrap font-mono text-[10px] text-muted-foreground">
                        {item.id}
                      </div>
                    </td>
                    <td className="whitespace-nowrap px-3 py-3 font-mono font-medium">
                      {item.legA.baseAsset} / {item.legA.quoteAsset}
                    </td>
                    <td className="whitespace-nowrap px-3 py-3">
                      <div className="font-medium">
                        {item.runMode === "one_shot" ? "一次性建仓" : "价差"}
                      </div>
                      <div className="mt-0.5 text-[10px] text-muted-foreground">
                        {item.legALeverage || "—"}x / {item.legBLeverage || "—"}x
                        {item.oneShotPhase
                          ? ` · ${oneShotPhaseLabel(item.oneShotPhase)}`
                          : ""}
                      </div>
                    </td>
                    <td className="whitespace-nowrap px-3 py-3 text-right font-mono tabular-nums">
                      <LiveBidAskSpread
                        bidSpreadBps={item.bidSpreadBps}
                        askSpreadBps={item.askSpreadBps}
                        marketDataStale={item.marketDataStale}
                      />
                    </td>
                    <td className="whitespace-nowrap px-3 py-3 text-right font-mono tabular-nums">
                      {formatSignedBps(item.averageEntrySpreadBps)}
                    </td>
                    <td
                      className={cn(
                        "whitespace-nowrap px-3 py-3 text-right font-mono tabular-nums",
                        rateColor(fundingAnnualized24h?.value ?? null),
                      )}
                      title={
                        fundingAnnualized24h?.complete === false
                          ? "24h 资金费历史覆盖不足，当前结果仅按已有 settled 数据估算"
                          : undefined
                      }
                    >
                      {formatFundingAnnualized24h(fundingAnnualized24h)}
                    </td>
                    <td
                      className="whitespace-nowrap px-3 py-3 text-right font-mono tabular-nums"
                      title={
                        item.fundingHistoryComplete
                          ? undefined
                          : "资金费历史覆盖不足，当前结果仅按已有 settled 数据估算"
                      }
                    >
                      {formatFundingIncome(
                        item.estimatedFundingPnl,
                        item.legA.quoteAsset,
                      )}
                    </td>
                    <td
                      className="whitespace-nowrap px-3 py-3 text-right font-mono tabular-nums"
                      title={
                        item.fundingHistoryComplete
                          ? undefined
                          : "资金费历史覆盖不足，当前结果仅按已有 settled 数据估算"
                      }
                    >
                      {formatAnnualized(item.combinedPositionAnnualized)}
                    </td>
                    <td className="px-3 py-3 text-right">
                      <div className="whitespace-nowrap font-mono tabular-nums">
                        {formatDecimal(item.targetNotional, " USDT")} /{" "}
                        {currentNotional === null
                          ? "--"
                          : formatDecimal(
                              Math.abs(Number(currentNotional)).toString(),
                              " USDT",
                            )}
                      </div>
                      <div className="mt-1 h-1 w-full overflow-hidden rounded-full bg-muted">
                        <div
                          className="h-full rounded-full bg-primary"
                          style={{
                            width: `${Number.isFinite(progress) ? progress : 0}%`,
                          }}
                        />
                      </div>
                    </td>
                    <td className="whitespace-nowrap px-3 py-3">
                      <CombinationStatus item={item} />
                    </td>
                    <td className="px-4 py-3 text-right">
                      <div className="flex justify-end gap-1">
                        <Button
                          variant="ghost"
                          size="sm"
                          className="w-[68px]"
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
                            className="w-[64px]"
                            disabled={
                              item.status === "closing" || closingID === item.id
                            }
                            onClick={(event) => {
                              event.stopPropagation();
                              void onClose(item.id);
                            }}
                          >
                            {item.status === "closing" || closingID === item.id
                              ? "关闭中"
                              : "关闭"}
                          </Button>
                        ) : null}
                      </div>
                    </td>
                  </tr>
                  {expanded ? (
                    <tr className="bg-muted/10">
                      <td colSpan={COMBINATION_TABLE_COLUMNS} className="min-h-48 px-4 py-3">
                        {detailLoading && detail?.id !== item.id ? (
                          <div
                            className="min-h-44 animate-pulse rounded-lg bg-muted/30"
                            aria-label="正在加载详情"
                          />
                        ) : (
                          <CombinationDetail
                            detail={
                              detail?.id === item.id
                                ? {
                                    ...detail,
                                    estimatedFundingPnl:
                                      item.estimatedFundingPnl,
                                    fundingHistoryComplete:
                                      item.fundingHistoryComplete,
                                  }
                                : item
                            }
                            onUpdated={onUpdated}
                          />
                        )}
                      </td>
                    </tr>
                  ) : null}
                </React.Fragment>
              );
            })}
            {!loading && rows.length === 0 ? (
              <tr>
                <td
                  colSpan={COMBINATION_TABLE_COLUMNS}
                  className="px-4 py-8 text-center text-muted-foreground"
                >
                  暂无套利组合
                </td>
              </tr>
            ) : null}
            {loading && rows.length === 0 ? (
              <tr>
                <td
                  colSpan={COMBINATION_TABLE_COLUMNS}
                  className="px-4 py-8 text-center text-muted-foreground"
                >
                  正在加载套利组合…
                </td>
              </tr>
            ) : null}
          </tbody>
        </table>
      </div>
      <div className="min-h-[33px] border-t px-4 py-2" />
    </section>
  );
}

function CombinationStatus({ item }: { item: ArbitrageCombination }) {
  const label = combinationRuntimeLabel(item);
  return (
    <Badge
      variant="outline"
      className={cn(
        "inline-flex min-w-[132px] justify-center whitespace-nowrap",
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
  if (item.status === "closed" && item.positionUncertain)
    return arbitrageRiskReason(item) === "position_drift"
      ? "已关闭（仓位待核对）"
      : "已关闭（订单状态待核对）";
  if (item.status === "closed") return "已关闭";
  if (item.status === "failed") return "失败";
  if (item.positionUncertain)
    return arbitrageRiskReason(item) === "order_state"
      ? "订单状态待核对"
      : "仓位待核对";
  if (item.runtimeState === "manual_intervention") return "需人工处理";
  if (item.runMode === "one_shot" && item.oneShotPhase === "exiting") {
    return "退出平仓中";
  }
  if (item.runMode === "one_shot" && item.oneShotPhase === "exited") {
    return "已退出 · 待关闭";
  }
  if (item.runMode === "one_shot" && item.oneShotPhase === "waiting_exit") {
    return "等待退出";
  }
  if (item.runMode === "one_shot" && item.oneShotPhase === "building_target") {
    return "一次性建仓";
  }
  switch (item.runtimeState) {
    case "maker_open":
      return "Maker 挂单中";
    case "maker_canceling":
      return "Maker 撤单确认中";
    case "repricing":
      return "重新报价";
    case "opportunity_gone":
      return "机会消失";
    case "hedging":
      return "终态对冲中";
    case "hedge_deferred_dust": {
      const exposure = combinationExpectedExposure(item);
      if (exposure.state === "baseline_reducing") return "底仓减仓中";
      if (exposure.state === "flat_after_baseline_close") return "已平至零";
      if (exposure.state === "ask") return "持有可平仓位";
      if (exposure.state === "reverse") return "持有反套";
      return "残差累计中";
    }
    case "reconciling":
      return "残差待追平";
    case "backoff":
      return "退避重试";
    case "position_uncertain":
      return "仓位待核对";
  }
  if (item.nextRetryAt && Date.parse(item.nextRetryAt) > Date.now())
    return "退避重试";
  const exposure = combinationExpectedExposure(item);
  if (exposure.state === "baseline_reducing") return "底仓减仓中";
  if (exposure.state === "flat_after_baseline_close") return "已平至零";
  if (exposure.state === "ask") return "持有可平仓位";
  if (exposure.state === "reverse") {
    const target = Number(item.targetNotional);
    return Number.isFinite(target) &&
      target > 0 &&
      exposure.pairedNotional >= target
      ? "旧反向仓位待收敛"
      : "旧反向仓位";
  }
  return "运行中";
}

function oneShotPhaseLabel(
  phase: ArbitrageCombination["oneShotPhase"],
): string {
  switch (phase) {
    case "waiting_exit":
      return "等待退出";
    case "exiting":
      return "退出平仓中";
    case "exited":
      return "已退出 · 待关闭";
    default:
      return "建仓中";
  }
}

type CombinationExpectedExposure = {
  state:
    | "ask"
    | "baseline_reducing"
    | "flat_after_baseline_close"
    | "reverse"
    | "none";
  expectedA: number;
  expectedB: number;
  pairedNotional: number;
};

function combinationExpectedExposure(
  item: ArbitrageCombination,
): CombinationExpectedExposure {
  const comboA = Number(item.legABasePosition);
  const comboB = Number(item.legBBasePosition);
  if (!Number.isFinite(comboA) || !Number.isFinite(comboB)) {
    return { state: "none", expectedA: 0, expectedB: 0, pairedNotional: 0 };
  }

  const baselineA =
    item.legAVenueBaselineBasePosition === null
      ? Number.NaN
      : Number(item.legAVenueBaselineBasePosition);
  const baselineB =
    item.legBVenueBaselineBasePosition === null
      ? Number.NaN
      : Number(item.legBVenueBaselineBasePosition);
  const hasBaseline =
    item.venueBaselineCapturedAt !== "" &&
    Number.isFinite(baselineA) &&
    Number.isFinite(baselineB);
  const expectedA = hasBaseline ? baselineA + comboA : comboA;
  const expectedB = hasBaseline ? baselineB + comboB : comboB;
  const priceA = expectedValuationPrice(
    item.legAVenueValuationPrice,
    item.legAVenueNotional,
    item.legAVenueBasePosition,
  );
  const priceB = expectedValuationPrice(
    item.legBVenueValuationPrice,
    item.legBVenueNotional,
    item.legBVenueBasePosition,
  );
  const askNotional = pairedDirectionalNotional(
    expectedA * priceA,
    -expectedB * priceB,
  );
  const reverseNotional = pairedDirectionalNotional(
    -expectedA * priceA,
    expectedB * priceB,
  );
  const baselineAskNotional = hasBaseline
    ? pairedDirectionalNotional(baselineA * priceA, -baselineB * priceB)
    : 0;
  const reducingBaseline =
    baselineAskNotional > 0 && comboA < 0 && comboB > 0;

  if (reducingBaseline && askNotional > 0) {
    return {
      state: "baseline_reducing",
      expectedA,
      expectedB,
      pairedNotional: askNotional,
    };
  }
  if (reducingBaseline && askNotional === 0) {
    return {
      state: "flat_after_baseline_close",
      expectedA,
      expectedB,
      pairedNotional: 0,
    };
  }
  if (askNotional > 0) {
    return {
      state: "ask",
      expectedA,
      expectedB,
      pairedNotional: askNotional,
    };
  }
  if (reverseNotional > 0) {
    return {
      state: "reverse",
      expectedA,
      expectedB,
      pairedNotional: reverseNotional,
    };
  }
  return { state: "none", expectedA, expectedB, pairedNotional: 0 };
}

function pairedDirectionalNotional(legA: number, legB: number): number {
  return legA > 0 && legB > 0 ? Math.min(legA, legB) : 0;
}

function expectedValuationPrice(
  price: string | null,
  venueNotional: string | null,
  venueBasePosition: string,
): number {
  const direct = price === null ? Number.NaN : Number(price);
  if (Number.isFinite(direct) && direct > 0) return direct;
  const notional = venueNotional === null ? Number.NaN : Number(venueNotional);
  const base = Number(venueBasePosition);
  const inferred = Math.abs(notional / base);
  return Number.isFinite(inferred) && inferred > 0 ? inferred : 1;
}

function currentVenuePositionNotional(
  item: ArbitrageCombination,
): string | null {
  const legA = item.legAVenueNotional;
  const legB = item.legBVenueNotional;
  const valueA = legA === null ? Number.NaN : Number(legA);
  const valueB = legB === null ? Number.NaN : Number(legB);
  const validA = Number.isFinite(valueA);
  const validB = Number.isFinite(valueB);
  if (!validA) return validB ? legB : null;
  if (!validB) return legA;
  return Math.abs(valueA) >= Math.abs(valueB) ? legA : legB;
}

function formatSignedNotional(value: string): string {
  const amount = Number(value);
  if (!Number.isFinite(amount)) return `${value} USDT`;
  const prefix = amount > 0 ? "+" : "";
  return `${prefix}${formatDecimal(value, " USDT")}`;
}

function formatBeijingChartTime(timestamp: number): string {
  return beijingChartTimeFormatter.format(new Date(timestamp));
}

function formatVenueNotionalAmount(value: string | null): string {
  if (value === null) return "--";
  const amount = Number(value);
  if (!Number.isFinite(amount)) return "--";
  const prefix = amount > 0 ? "+" : "";
  return `${prefix}${amount.toFixed(2)}`;
}

function formatVenueValuationMeta(
  price: string | null,
  valuedAt: string,
): string | null {
  if (price === null || !valuedAt) return null;
  const amount = Number(price);
  if (!Number.isFinite(amount)) return null;
  return `@ ${formatDecimal(price)} · ${formatTime(valuedAt)}`;
}

function venueNotionalColor(value: string | null): string {
  const amount = value === null ? Number.NaN : Number(value);
  if (!Number.isFinite(amount) || amount === 0) return "text-muted-foreground";
  return amount > 0 ? "text-positive" : "text-negative";
}

function formatSignedBps(value: string | null): string {
  if (value === null) return "--";
  const amount = Number(value);
  if (!Number.isFinite(amount)) return "--";
  return `${amount > 0 ? "+" : ""}${amount.toFixed(2)} bps`;
}

function formatAnnualized(value: string | null): string {
  if (value === null) return "--";
  const amount = Number(value) * 100;
  if (!Number.isFinite(amount)) return "--";
  return `${amount > 0 ? "+" : ""}${amount.toFixed(2)}%`;
}

function formatFundingAnnualized24h(
  metric: { value: number; complete: boolean } | null,
): string {
  if (!metric) return "--";
  return formatPercent(metric.value, 2);
}

function formatSignedPnl(value: string | null): string {
  if (value === null) return "--";
  const amount = Number(value);
  if (!Number.isFinite(amount)) return "--";
  return `${amount > 0 ? "+" : ""}${amount.toFixed(4)}`;
}

function formatFundingIncome(value: string | null, quoteAsset: string): string {
  const amount = formatSignedPnl(value);
  if (amount === "--") return "--";
  return `${amount} ${quoteAsset}`;
}

type EditableArbitrageField =
  "targetNotional" | "askThresholdBps" | "bidThresholdBps";

function editableArbitrageValues(
  detail: ArbitrageCombination | ArbitrageCombinationDetail,
): Record<EditableArbitrageField, string> {
  return {
    targetNotional: detail.targetNotional,
    askThresholdBps: detail.askThresholdBps,
    bidThresholdBps: detail.bidThresholdBps,
  };
}

function CombinationDetail({
  detail,
  onUpdated,
}: {
  detail: ArbitrageCombination | ArbitrageCombinationDetail;
  onUpdated: (item: ArbitrageCombination) => void;
}) {
  const complete = "orders" in detail;
  const venueErrorOrder = complete
    ? detail.orders.find((order) => meaningfulOrderError(order) !== null)
    : undefined;
  const venueError = venueErrorOrder
    ? meaningfulOrderError(venueErrorOrder)
    : null;
  const riskReason = arbitrageRiskReason(detail);
  const hasVenueBaseline =
    detail.venueBaselineCapturedAt !== "" &&
    detail.legAVenueBaselineBasePosition !== null &&
    detail.legBVenueBaselineBasePosition !== null;
  const expectedExposure = combinationExpectedExposure(detail);
  const expectedLegAPosition =
    detail.legAExpectedBasePosition ??
    formatDecimal(String(expectedExposure.expectedA));
  const expectedLegBPosition =
    detail.legBExpectedBasePosition ??
    formatDecimal(String(expectedExposure.expectedB));
  const [editingField, setEditingField] =
    React.useState<EditableArbitrageField | null>(null);
  const [loadingField, setLoadingField] =
    React.useState<EditableArbitrageField | null>(null);
  const [drafts, setDrafts] = React.useState(() =>
    editableArbitrageValues(detail),
  );
  const [fieldErrors, setFieldErrors] = React.useState<
    Partial<Record<EditableArbitrageField, string>>
  >({});

  React.useEffect(() => {
    if (editingField === null) setDrafts(editableArbitrageValues(detail));
  }, [detail, editingField]);

  const beginEdit = (field: EditableArbitrageField) => {
    setDrafts((current) => ({ ...current, [field]: detail[field] }));
    setFieldErrors((current) => ({ ...current, [field]: undefined }));
    setEditingField(field);
  };
  const cancelEdit = () => {
    if (editingField) {
      setDrafts((current) => ({
        ...current,
        [editingField]: detail[editingField],
      }));
      setFieldErrors((current) => ({ ...current, [editingField]: undefined }));
    }
    setEditingField(null);
  };
  const saveField = async (field: EditableArbitrageField) => {
    if (loadingField !== null) return;
    setLoadingField(field);
    setFieldErrors((current) => ({ ...current, [field]: undefined }));
    try {
      const value = drafts[field];
      const input =
        field === "targetNotional"
          ? { targetNotional: value }
          : field === "askThresholdBps"
            ? { askThresholdBps: value }
            : { bidThresholdBps: value };
      const updated = await updateArbitrageCombination(detail.id, input);
      onUpdated(updated);
      setDrafts(editableArbitrageValues(updated));
      setEditingField(null);
    } catch (error) {
      setFieldErrors((current) => ({
        ...current,
        [field]: error instanceof Error ? error.message : "参数更新失败",
      }));
    } finally {
      setLoadingField(null);
    }
  };

  return (
    <div>
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <span className="font-mono text-xs font-semibold">{detail.id}</span>
        <span className="text-[11px] text-muted-foreground">
          创建于 {formatTime(detail.createdAt)}
        </span>
        {detail.marketDataStale ? (
          <Badge variant="outline">行情过期</Badge>
        ) : null}
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
          label="运行模式"
          value={detail.runMode === "one_shot" ? "一次性建仓" : "价差触发"}
        />
        {detail.runMode === "one_shot" ? (
          <>
            <DetailItem
              label="阶段"
              value={oneShotPhaseLabel(detail.oneShotPhase)}
            />
            <DetailItem
              label="退出条件"
              value={
                detail.exitPolicy === "annualized"
                  ? `年化达到 ≥ ${formatAnnualizedRatioAsPercent(detail.exitAnnualizedRate)}`
                  : detail.exitPolicy === "time"
                    ? formatHoldDuration(detail.exitAfterSeconds)
                    : "—"
              }
            />
            <DetailItem
              label="预计退出时间"
              value={
                detail.scheduledExitAt
                  ? formatTime(detail.scheduledExitAt)
                  : "—"
              }
            />
            <DetailItem
              label="8h资金费年化"
              value={
                detail.earlyExitFunding8hAnnualizedFloor
                  ? `8h资金费年化< ${formatSignedAnnualizedRatioAsPercent(detail.earlyExitFunding8hAnnualizedFloor)}`
                  : "未启用"
              }
            />
          </>
        ) : null}
        <DetailItem
          label="杠杆 Leg A / Leg B"
          value={`${detail.legALeverage || "—"}x / ${detail.legBLeverage || "—"}x`}
        />
        <EditableMetric
          label="目标仓位"
          value={detail.targetNotional}
          unit="USDT"
          secondary={`当前 ${detail.positionNotional} USDT`}
          editable={detail.status === "running" && detail.runMode !== "one_shot"}
          editing={editingField === "targetNotional"}
          loading={loadingField === "targetNotional"}
          draft={drafts.targetNotional}
          error={fieldErrors.targetNotional}
          onStart={() => beginEdit("targetNotional")}
          onDraft={(value) =>
            setDrafts((current) => ({ ...current, targetNotional: value }))
          }
          onSave={() => void saveField("targetNotional")}
          onCancel={cancelEdit}
        />
        {detail.runMode === "spread" ? (
          <>
            <EditableMetric
              label="开仓阈值"
              value={detail.askThresholdBps}
              unit="bps"
              editable={detail.status === "running"}
              editing={editingField === "askThresholdBps"}
              loading={loadingField === "askThresholdBps"}
              draft={drafts.askThresholdBps}
              error={fieldErrors.askThresholdBps}
              onStart={() => beginEdit("askThresholdBps")}
              onDraft={(value) =>
                setDrafts((current) => ({ ...current, askThresholdBps: value }))
              }
              onSave={() => void saveField("askThresholdBps")}
              onCancel={cancelEdit}
            />
            <EditableMetric
              label="平仓阈值"
              value={detail.bidThresholdBps}
              unit="bps"
              editable={detail.status === "running"}
              editing={editingField === "bidThresholdBps"}
              loading={loadingField === "bidThresholdBps"}
              draft={drafts.bidThresholdBps}
              error={fieldErrors.bidThresholdBps}
              onStart={() => beginEdit("bidThresholdBps")}
              onDraft={(value) =>
                setDrafts((current) => ({ ...current, bidThresholdBps: value }))
              }
              onSave={() => void saveField("bidThresholdBps")}
              onCancel={cancelEdit}
            />
          </>
        ) : null}
        <DetailItem
          label="组合累计成交"
          value={`${detail.cumulativeTurnoverNotional} USDT`}
        />
        <DetailItem
          label="双腿累计成交额"
          value={`${detail.grossTurnoverNotional} USDT`}
        />
        <PnlDetailItem
          legA={detail.legAUnrealizedPnl}
          legB={detail.legBUnrealizedPnl}
          quoteAsset={detail.legA.quoteAsset}
        />
        <DetailItem
          label="未配平 Base carry"
          value={`${detail.carryBaseQuantity} ${detail.legA.baseAsset}`}
        />
        <DetailItem
          label="创建底仓 Leg A / Leg B"
          value={
            hasVenueBaseline
              ? `${detail.legAVenueBaselineBasePosition} / ${detail.legBVenueBaselineBasePosition} ${detail.legA.baseAsset}`
              : "未记录创建底仓（旧组合审计）"
          }
        />
        <DetailItem
          label="组合成交净仓 Leg A / Leg B"
          value={`${detail.legABasePosition} / ${detail.legBBasePosition} ${detail.legA.baseAsset}`}
        />
        <DetailItem
          label="预期交易所仓位 Leg A / Leg B"
          value={`${expectedLegAPosition} / ${expectedLegBPosition} ${detail.legA.baseAsset}`}
        />
        <DetailItem
          label="交易所实际仓位 Leg A / Leg B"
          value={`${detail.legAVenueBasePosition} / ${detail.legBVenueBasePosition} ${detail.legA.baseAsset}`}
        />
        <DetailItem
          label="仓位漂移 Leg A / Leg B"
          value={`${detail.legAPositionDifference} / ${detail.legBPositionDifference} ${detail.legA.baseAsset}`}
        />
        <DetailItem
          label="最后仓位对账"
          value={
            detail.lastPositionReconciledAt
              ? formatTime(detail.lastPositionReconciledAt)
              : "尚未对账"
          }
        />
        <DetailItem
          label="执行方式"
          value={
            detail.executionMode === "simultaneous_market"
              ? "双腿并发市价"
              : `Leg ${detail.preferredLeg.toUpperCase()} Maker 后对冲`
          }
        />
        <VenueNotionalDetailItem
          quoteAsset={detail.legA.quoteAsset || "USDT"}
          legANotional={detail.legAVenueNotional}
          legBNotional={detail.legBVenueNotional}
          legAPrice={detail.legAVenueValuationPrice}
          legBPrice={detail.legBVenueValuationPrice}
          legAValuedAt={detail.legAVenueValuationAt}
          legBValuedAt={detail.legBVenueValuationAt}
        />
      </div>
      {detail.errorMessage ? (
        <div
          className={cn(
            "mt-3 rounded-md px-3 py-2 text-xs",
            detail.status === "closing"
              ? "bg-amber-500/10 text-amber-700 dark:text-amber-300"
              : "bg-destructive/10 text-destructive",
          )}
        >
          <div>
            {detail.status === "closing"
              ? "关闭未完成，系统将自动重试"
              : detail.status === "closed" && detail.positionUncertain
                ? riskReason === "position_drift"
                  ? "已关闭（仓位待核对）。策略已停止；交易所仓位与组合账本仍需人工核对。"
                  : "已关闭（订单状态待核对）。策略已停止，不会产生新执行；最终成交量需人工核对。"
                : riskReason === "order_state"
                  ? "订单状态待核对"
                  : riskReason === "position_drift"
                    ? "仓位待核对"
                    : detail.errorMessage}
          </div>
          {detail.status === "closing" && detail.nextRetryAt ? (
            <div className="mt-1">
              下次重试：{formatTime(detail.nextRetryAt)}
            </div>
          ) : null}
          {detail.status === "closing" ||
          (detail.positionUncertain && riskReason !== "other") ? (
            <div className="mt-1 font-mono">{detail.errorMessage}</div>
          ) : null}
          {venueErrorOrder ? (
            <div className="mt-1 font-mono">
              {venueErrorOrder.exchange}
              {venueError ? ` · ${venueError}` : ""}
            </div>
          ) : null}
        </div>
      ) : null}
      {complete ? (
        <div className="mt-3 border-t pt-3">
          <div className="mb-2 text-[10px] font-medium tracking-wide text-muted-foreground uppercase">
            最近订单
          </div>
          {detail.orders.length === 0 ? (
            <div className="rounded-md border border-dashed px-3 py-4 text-center text-[11px] text-muted-foreground">
              暂无已提交到交易所的订单
            </div>
          ) : (
            <div className="overflow-x-auto rounded-md border">
              <table className="w-full min-w-[920px] text-[11px]">
                <thead className="bg-muted/40 text-muted-foreground">
                  <tr>
                    <th className="px-2 py-1.5 text-left font-medium">更新时间</th>
                    <th className="px-2 py-1.5 text-left font-medium">交易所</th>
                    <th className="px-2 py-1.5 text-left font-medium">Leg</th>
                    <th className="px-2 py-1.5 text-left font-medium">方向</th>
                    <th className="px-2 py-1.5 text-right font-medium">委托量</th>
                    <th className="px-2 py-1.5 text-right font-medium">成交量</th>
                    <th className="px-2 py-1.5 text-left font-medium">状态</th>
                    <th className="px-2 py-1.5 text-left font-medium">错误信息</th>
                  </tr>
                </thead>
                <tbody>
                  {detail.orders.map((order) => {
                    const meaningfulError = meaningfulOrderError(order);
                    const expectedNoFill = expectedNoFillOrder(order);
                    return (
                    <tr key={order.id} className="border-t">
                      <td className="whitespace-nowrap px-2 py-1.5">
                        {formatTime(order.updatedAt)}
                      </td>
                      <td className="px-2 py-1.5">{order.exchange}</td>
                      <td className="px-2 py-1.5 uppercase">
                        {order.arbitrageLeg || "—"}
                      </td>
                      <td className="px-2 py-1.5">
                        {order.side === "buy" ? "买" : "卖"}
                      </td>
                      <td className="px-2 py-1.5 text-right font-mono">
                        {order.quantity}
                      </td>
                      <td className="px-2 py-1.5 text-right font-mono">
                        {order.filledQuantity}
                      </td>
                      <td className="px-2 py-1.5">{order.status}</td>
                      <td
                        className={cn(
                          "max-w-[260px] break-words px-2 py-1.5",
                          meaningfulError && "text-destructive",
                        )}
                      >
                        {expectedNoFill
                          ? "未即时成交（已取消）"
                          : meaningfulError ?? "—"}
                      </td>
                    </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </div>
      ) : null}
    </div>
  );
}

function meaningfulOrderError(
  order: ArbitrageCombinationDetail["orders"][number],
): string | null {
  if (expectedNoFillOrder(order)) return null;
  const benign = new Set([
    "EC_NOERROR",
    "UNKNOWN",
    "CANCEL_BY_UNKNOWN",
    "CANCELBYUNKNOWN",
    "CANCEL_BY_USER",
    "CANCELBYUSER",
    "NOTAVAILABLE",
  ]);
  const values = [order.errorCode, order.errorMessage]
    .map((value) => value.trim())
    .filter((value) => value !== "" && !benign.has(value.toUpperCase()));
  return values.length > 0 ? values.join(" · ") : null;
}

function expectedNoFillOrder(
  order: ArbitrageCombinationDetail["orders"][number],
): boolean {
  if (
    !["canceled", "rejected", "expired"].includes(order.status) ||
    Number(order.filledQuantity) !== 0
  ) {
    return false;
  }
  const metadata = `${order.errorCode} ${order.errorMessage}`.toUpperCase();
  return (
    metadata.includes("EC_NOIMMEDIATEQTYTOFILL") ||
    metadata.includes("EC_POSTONLYWILLTAKELIQUIDITY")
  );
}

type ArbitrageRiskReason = "order_state" | "position_drift" | "other";

function arbitrageRiskReason(
  item: ArbitrageCombination,
): ArbitrageRiskReason {
  const message = item.errorMessage.trim().toLowerCase();
  if (
    message.startsWith("account position differs from combination ledger:")
  ) {
    return "position_drift";
  }
  if (
    message === "order state remained uncertain after bounded reconciliation" ||
    (item.status === "closed" && item.positionUncertain)
  ) {
    return "order_state";
  }
  return "other";
}

function EditableMetric({
  label,
  value,
  unit,
  secondary,
  editable,
  editing,
  loading,
  draft,
  error,
  onStart,
  onDraft,
  onSave,
  onCancel,
}: {
  label: string;
  value: string;
  unit: string;
  secondary?: string;
  editable: boolean;
  editing: boolean;
  loading: boolean;
  draft: string;
  error?: string;
  onStart: () => void;
  onDraft: (value: string) => void;
  onSave: () => void;
  onCancel: () => void;
}) {
  return (
    <div>
      <div className="text-[10px] tracking-wide text-muted-foreground uppercase">
        {label}
      </div>
      {editing ? (
        <>
          <div className="mt-1 flex items-center gap-1">
            <Input
              autoFocus
              aria-label={`编辑${label}`}
              className="h-7 min-w-0 bg-background px-2 text-xs"
              inputMode="decimal"
              value={draft}
              disabled={loading}
              onChange={(event) => onDraft(event.target.value)}
              onKeyDown={(event) => {
                if (event.key === "Enter") {
                  event.preventDefault();
                  onSave();
                } else if (event.key === "Escape") {
                  event.preventDefault();
                  onCancel();
                }
              }}
            />
            <span className="shrink-0 text-[11px] text-muted-foreground">
              {unit}
            </span>
            <button
              type="button"
              aria-label={`保存${label}`}
              className="rounded p-1 text-positive hover:bg-muted disabled:opacity-50"
              disabled={loading}
              onClick={onSave}
            >
              <Check className="size-3.5" />
            </button>
            <button
              type="button"
              aria-label={`取消编辑${label}`}
              className="rounded p-1 text-muted-foreground hover:bg-muted disabled:opacity-50"
              disabled={loading}
              onClick={onCancel}
            >
              <X className="size-3.5" />
            </button>
          </div>
          {secondary ? (
            <div className="mt-0.5 text-[10px] text-muted-foreground">
              {secondary}
            </div>
          ) : null}
          {error ? (
            <div className="mt-1 text-[10px] leading-tight text-destructive">
              {error}
            </div>
          ) : null}
        </>
      ) : (
        <>
          <div className="mt-1 flex items-center gap-1 text-xs font-medium">
            <span>
              {value} {unit}
            </span>
            {editable ? (
              <button
                type="button"
                aria-label={`编辑${label}`}
                className="rounded p-0.5 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
                onClick={onStart}
              >
                <Pencil className="size-3" />
              </button>
            ) : null}
          </div>
          {secondary ? (
            <div className="mt-0.5 text-[10px] text-muted-foreground">
              {secondary}
            </div>
          ) : null}
        </>
      )}
    </div>
  );
}

function DetailItem({
  label,
  value,
  title,
}: {
  label: string;
  value: string;
  title?: string;
}) {
  return (
    <div title={title}>
      <div className="text-[10px] tracking-wide text-muted-foreground uppercase">
        {label}
      </div>
      <div className="mt-1 text-xs font-medium">{value}</div>
    </div>
  );
}

function VenueNotionalDetailItem({
  quoteAsset,
  legANotional,
  legBNotional,
  legAPrice,
  legBPrice,
  legAValuedAt,
  legBValuedAt,
}: {
  quoteAsset: string;
  legANotional: string | null;
  legBNotional: string | null;
  legAPrice: string | null;
  legBPrice: string | null;
  legAValuedAt: string;
  legBValuedAt: string;
}) {
  return (
    <div>
      <div className="flex items-center gap-1.5">
        <div className="text-[10px] tracking-wide text-muted-foreground uppercase">
          交易所实际市值
        </div>
        <div className="text-[10px] tracking-wide text-muted-foreground uppercase">
          {quoteAsset}
        </div>
      </div>
      <div className="mt-1 space-y-1">
        <VenueNotionalRow
          leg="A"
          notional={legANotional}
          price={legAPrice}
          valuedAt={legAValuedAt}
        />
        <VenueNotionalRow
          leg="B"
          notional={legBNotional}
          price={legBPrice}
          valuedAt={legBValuedAt}
        />
      </div>
    </div>
  );
}

function VenueNotionalRow({
  leg,
  notional,
  price,
  valuedAt,
}: {
  leg: "A" | "B";
  notional: string | null;
  price: string | null;
  valuedAt: string;
}) {
  const meta = formatVenueValuationMeta(price, valuedAt);
  return (
    <div className="flex min-w-0 items-baseline gap-2 text-xs">
      <span className="w-3 shrink-0 text-[10px] font-medium text-muted-foreground">
        {leg}
      </span>
      <span
        className={cn(
          "w-16 shrink-0 text-right font-medium tabular-nums",
          venueNotionalColor(notional),
        )}
      >
        {formatVenueNotionalAmount(notional)}
      </span>
      {meta ? (
        <span className="min-w-0 truncate text-[10px] text-muted-foreground">
          {meta}
        </span>
      ) : null}
    </div>
  );
}

function PnlDetailItem({
  legA,
  legB,
  quoteAsset,
}: {
  legA: string | null;
  legB: string | null;
  quoteAsset: string;
}) {
  const color = (value: string | null) => {
    const amount = value === null ? Number.NaN : Number(value);
    if (!Number.isFinite(amount) || amount === 0)
      return "text-muted-foreground";
    return amount > 0 ? "text-positive" : "text-negative";
  };
  return (
    <div>
      <div className="text-[10px] tracking-wide text-muted-foreground uppercase">
        Leg A / Leg B 浮动盈亏
      </div>
      <div className="mt-1 text-xs font-medium">
        <span className={color(legA)}>{formatSignedPnl(legA)}</span>
        <span className="text-muted-foreground"> / </span>
        <span className={color(legB)}>{formatSignedPnl(legB)}</span>
        <span>{` ${quoteAsset}`}</span>
      </div>
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
  history,
  askThreshold,
  bidThreshold,
}: {
  history: BasisSpreadHistory;
  askThreshold: number;
  bidThreshold: number;
}) {
  const width = 760;
  const height = 220;
  const margin = { top: 16, right: 28, bottom: 30, left: 52 };
  const values = history.points.map((point) => point.spreadBps);
  const timestamps = history.points.map((point) => Date.parse(point.ts));
  const xStart = Math.min(...timestamps);
  const xEnd = Math.max(...timestamps);
  const validThresholds = [askThreshold, bidThreshold].filter(Number.isFinite);
  const { min, max } = spreadYDomain([...values, ...validThresholds]);
  const x = (timestamp: number) =>
    margin.left +
    ((timestamp - xStart) / Math.max(xEnd - xStart, 1)) *
      (width - margin.left - margin.right);
  const y = (value: number) =>
    margin.top +
    ((max - value) / Math.max(max - min, 0.0001)) *
      (height - margin.top - margin.bottom);
  const gapMs = history.resolutionSeconds * 2.5 * 1000;
  const segments: string[] = [];
  let path = "";
  history.points.forEach((point, index) => {
    const timestamp = Date.parse(point.ts);
    const previous =
      index === 0 ? timestamp : Date.parse(history.points[index - 1]!.ts);
    if (index > 0 && timestamp - previous > gapMs && path) {
      segments.push(path);
      path = "";
    }
    path += `${path ? " L" : "M"} ${x(timestamp).toFixed(2)} ${y(point.spreadBps).toFixed(2)}`;
  });
  if (path) segments.push(path);
  const plotRight = width - margin.right;
  const lastPoint = history.points.at(-1);

  return (
    <div className="px-2 py-3">
      <svg
        viewBox={`0 0 ${width} ${height}`}
        className="h-52 w-full"
        role="img"
        aria-label="Best Ask 价差走势"
      >
        {[0, 0.25, 0.5, 0.75, 1].map((ratio) => {
          const value = max - ratio * (max - min);
          const lineY = y(value);
          return (
            <g key={ratio}>
              <line
                x1={margin.left}
                x2={plotRight}
                y1={lineY}
                y2={lineY}
                stroke="var(--border)"
                strokeDasharray="4 5"
              />
              <text
                x={margin.left - 8}
                y={lineY + 3}
                textAnchor="end"
                fill="var(--muted-foreground)"
                fontSize="10"
              >
                {value.toFixed(1)}
              </text>
            </g>
          );
        })}
        {min <= 0 && max >= 0 ? (
          <line
            x1={margin.left}
            x2={plotRight}
            y1={y(0)}
            y2={y(0)}
            stroke="var(--muted-foreground)"
            strokeOpacity="0.5"
          />
        ) : null}
        {Number.isFinite(askThreshold) ? (
          <ThresholdLine
            y={y(askThreshold)}
            x1={margin.left}
            x2={plotRight}
            label={`开仓 ${askThreshold} bps`}
            tone="positive"
          />
        ) : null}
        {Number.isFinite(bidThreshold) ? (
          <ThresholdLine
            y={y(bidThreshold)}
            x1={margin.left}
            x2={plotRight}
            label={`平仓 ${bidThreshold} bps`}
            tone="negative"
          />
        ) : null}
        {segments.map((segment, index) => (
          <path
            key={index}
            d={segment}
            fill="none"
            stroke="var(--primary)"
            strokeWidth="2.5"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        ))}
        {lastPoint ? (
          <circle
            cx={x(Date.parse(lastPoint.ts))}
            cy={y(lastPoint.spreadBps)}
            r="4"
            fill="var(--primary)"
            stroke="var(--background)"
            strokeWidth="2"
          />
        ) : null}
        {[0, 0.5, 1].map((ratio) => {
          const timestamp = xStart + ratio * (xEnd - xStart);
          return (
            <text
              key={ratio}
              x={x(timestamp)}
              y={height - 8}
              textAnchor={
                ratio === 0 ? "start" : ratio === 1 ? "end" : "middle"
              }
              fill="var(--muted-foreground)"
              fontSize="10"
            >
              {formatBeijingChartTime(timestamp)}
            </text>
          );
        })}
      </svg>
    </div>
  );
}

function ThresholdLine({
  y,
  x1,
  x2,
  label,
  tone,
}: {
  y: number;
  x1: number;
  x2: number;
  label: string;
  tone: "positive" | "negative";
}) {
  const color = tone === "positive" ? "var(--positive)" : "var(--negative)";
  return (
    <>
      <line
        x1={x1}
        x2={x2}
        y1={y}
        y2={y}
        stroke={color}
        strokeWidth="1"
        strokeDasharray="5 5"
      />
      <text x={x2 - 4} y={y - 5} textAnchor="end" fill={color} fontSize="10">
        {label}
      </text>
    </>
  );
}

function formatLiquidityMetric(
  metric: LiquidityMetric,
  formatter: (value: number) => string,
): string {
  if (
    metric.status === "live" &&
    metric.value !== null &&
    Number.isFinite(metric.value)
  ) {
    return formatter(metric.value);
  }
  if (metric.status === "stale") return "已过期";
  if (metric.status === "missing") return "暂无";
  return "—";
}

function ChartLiquidityMetricRow({
  metrics,
}: {
  metrics: ChartLiquidityMetrics;
}) {
  const settlement = (metric: LiquidityMetric) =>
    formatLiquidityMetric(metric, (value) => `${value}h`);
  return (
    <div
      className="overflow-hidden rounded-lg border bg-muted/10"
      aria-label="图表流动性指标"
      title={metrics.detail}
    >
      <div className="grid divide-y sm:grid-cols-3 sm:divide-x sm:divide-y-0">
        <HeaderMetric
          label={metrics.positionLabel}
          value={formatLiquidityMetric(metrics.positionNotional, (value) =>
            formatCurrency(value, true),
          )}
        />
        <HeaderMetric
          label={metrics.volumeLabel}
          value={formatLiquidityMetric(metrics.dailyVolume, (value) =>
            formatCurrency(value, true),
          )}
        />
        <HeaderMetric
          label="结算周期"
          value={`A ${settlement(metrics.settlementA)} / B ${settlement(metrics.settlementB)}`}
        />
      </div>
      <div className="border-t px-3 py-1 text-[9px] text-muted-foreground">
        {metrics.description}
      </div>
    </div>
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
      <div
        className={cn(
          "mt-0.5 truncate font-mono text-sm font-semibold",
          valueClassName,
        )}
      >
        {value}
      </div>
    </div>
  );
}

function Field({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <label className="grid gap-1">
      <span className="text-[10px] font-medium tracking-wide text-muted-foreground uppercase">
        {label}
      </span>
      {children}
    </label>
  );
}

function UnitInput({
  value,
  onChange,
  unit,
  disabled,
  placeholder,
}: {
  value: string;
  onChange: (value: string) => void;
  unit: string;
  disabled?: boolean;
  placeholder?: string;
}) {
  return (
    <div className="relative">
      <Input
        value={value}
        onChange={(event) => onChange(event.target.value)}
        inputMode="decimal"
        disabled={disabled}
        placeholder={placeholder}
        className="pr-14 font-mono"
      />
      <span className="pointer-events-none absolute top-1/2 right-2.5 -translate-y-1/2 text-[10px] text-muted-foreground">
        {unit}
      </span>
    </div>
  );
}
