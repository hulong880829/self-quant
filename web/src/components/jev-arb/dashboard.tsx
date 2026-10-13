"use client";

import * as React from "react";
import Link from "next/link";
import { ArrowLeft, LoaderCircle, RefreshCw } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import {
  fetchJevDecisions,
  fetchJevEquity,
  fetchJevPositions,
  fetchJevSnapshot,
  fetchJevTrades,
  type JevDecision,
  type JevEquityCurve,
  type JevPosition,
  type JevSnapshot,
  type JevTrade,
} from "@/lib/api/jev-arb";
import { cn } from "@/lib/utils";

function formatNumber(value: string): string {
  const parsed = Number(value);
  if (!Number.isFinite(parsed)) return value || "—";
  return parsed.toLocaleString("en-US", { maximumFractionDigits: 6 });
}

export function JevArbDashboard() {
  const [snapshot, setSnapshot] = React.useState<JevSnapshot | null>(null);
  const [positions, setPositions] = React.useState<JevPosition[]>([]);
  const [decisions, setDecisions] = React.useState<JevDecision[]>([]);
  const [trades, setTrades] = React.useState<JevTrade[]>([]);
  const [equity, setEquity] = React.useState<JevEquityCurve | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [selectedVersion, setSelectedVersion] = React.useState<string>("");
  const etags = React.useRef<Record<string, string | null>>({});

  const load = React.useCallback(async (signal?: AbortSignal) => {
    try {
      const version = selectedVersion || undefined;
      const [snap, pos, dec, trd, eq] = await Promise.all([
        fetchJevSnapshot(etags.current.snapshot, signal, version),
        fetchJevPositions(etags.current.positions, signal, version),
        fetchJevDecisions(etags.current.decisions, signal, version),
        fetchJevTrades(etags.current.trades, signal, version),
        fetchJevEquity("24h", etags.current.equity, signal, version),
      ]);
      if (snap.status === "updated") {
        setSnapshot(snap.data);
        etags.current.snapshot = snap.etag;
      }
      if (pos.status === "updated") {
        setPositions(pos.data);
        etags.current.positions = pos.etag;
      }
      if (dec.status === "updated") {
        setDecisions(dec.data);
        etags.current.decisions = dec.etag;
      }
      if (trd.status === "updated") {
        setTrades(trd.data);
        etags.current.trades = trd.etag;
      }
      if (eq.status === "updated") {
        setEquity(eq.data);
        etags.current.equity = eq.etag;
      }
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "JEV 数据不可用");
    } finally {
      setLoading(false);
    }
  }, [selectedVersion]);

  React.useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    const timer = window.setInterval(() => {
      void load();
    }, 15000);
    return () => {
      controller.abort();
      window.clearInterval(timer);
    };
  }, [load]);

  return (
    <TooltipProvider>
    <div className="flex h-full min-h-0 flex-col overflow-auto">
      <div className="flex items-center justify-between gap-3 border-b px-4 py-3">
        <div>
          <Link
            href="/funding"
            aria-label="返回套利机会"
            className="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
          >
            <ArrowLeft className="size-4" />
            返回套利机会
          </Link>
          <div className="mt-2 flex items-center gap-2 text-sm text-muted-foreground">
            <Link href="/funding" className="hover:text-foreground">
              套利机会
            </Link>
            <span>/</span>
            <span className="text-foreground">JEV 纸面套利</span>
          </div>
          <h1 className="mt-1 text-lg font-semibold">JEV 纸面套利</h1>
        </div>
        <Button type="button" variant="outline" size="sm" onClick={() => void load()}>
          {loading ? <LoaderCircle className="size-4 animate-spin" /> : <RefreshCw className="size-4" />}
          刷新
        </Button>
      </div>
      {error ? (
        <div className="m-4 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive">
          {error}
        </div>
      ) : null}
      <div className="grid gap-3 p-4 md:grid-cols-5">
        <Metric label="已实现价差" value={formatNumber(snapshot?.realizedSpreadPnl ?? "")} hint="已平仓价差盈亏" />
        <Metric label="未实现价差" value={formatNumber(snapshot?.unrealizedSpreadPnl ?? "")} hint="持仓未实现价差" />
        <Metric label="资金费" value={formatNumber(snapshot?.fundingPnl ?? "")} hint="已计入的资金费盈亏" />
        <Metric label="成本" value={formatNumber(snapshot?.cumulativeCost ?? "")} hint="已计入的纸面往返成本" />
        <Metric label="当前版本净收益" value={formatNumber(snapshot?.netPnl ?? "")} hint="当前策略版本净盈亏" />
      </div>
      <div className="flex flex-wrap items-center gap-2 px-4 pb-3">
        <StatusTooltip label={cycleStatusLabel(snapshot?.status)} hint="本轮引擎周期状态">
          <Badge variant={snapshot?.status === "running" ? "default" : "secondary"}>
            {cycleStatusLabel(snapshot?.status)}
          </Badge>
        </StatusTooltip>
        {snapshot?.simulated ? (
          <StatusTooltip label="纸面模式" hint="只做纸面成交，不下真实订单">
            <Badge variant="outline">纸面模式</Badge>
          </StatusTooltip>
        ) : null}
        <StatusTooltip
          label={modelBadgeLabel(snapshot)}
          hint={snapshot?.modelStatus === "not_called" ? "本轮没有调用模型" : "本轮模型成功次数 / 请求次数"}
        >
          <Badge variant={modelBadgeVariant(snapshot?.modelStatus)}>
            {modelBadgeLabel(snapshot)}
          </Badge>
        </StatusTooltip>
        <span className="text-xs text-muted-foreground">
          上次成功 {formatTimestamp(snapshot?.lastSuccessfulAt)} · 模型 {formatTimestamp(snapshot?.lastModelCallAt)}
        </span>
        <span className="text-xs text-muted-foreground">
          {snapshot?.strategyVersion || "JEVStrategyV6"} · {snapshot?.model || snapshot?.modelRequested || "—"} · {snapshot?.promptVersion ?? "—"} · 往返成本{" "}
          {snapshot?.paperRoundTripCostBps ?? "—"} bps
        </span>
        {(snapshot?.availableStrategyVersions?.length ?? 0) > 1 ? (
          <label className="flex items-center gap-1 text-xs text-muted-foreground">
            版本
            <select
              className="rounded border bg-background px-1 py-0.5 text-foreground"
              value={selectedVersion || snapshot?.strategyVersion || ""}
              onChange={(event) => {
                etags.current = {};
                setSelectedVersion(event.target.value);
              }}
            >
              {(snapshot?.availableStrategyVersions ?? []).map((version) => (
                <option key={version} value={version}>
                  {version}
                </option>
              ))}
            </select>
          </label>
        ) : null}
        {snapshot?.degradedReasons?.map((reason) => (
          <Badge key={reason} variant="destructive">
            {reason}
          </Badge>
        ))}
      </div>
      <EquitySparkline points={equity?.points ?? []} />
      <Section title="本轮筛选">
        <FunnelSummary snapshot={snapshot} />
      </Section>
      <Section title="持仓">
        <Table
          headers={["标的", "方向", "价差", "资金费", "成本", "可平仓净收益", "版本", "状态"]}
          rows={positions.map((item) => [
            `${item.baseAsset} ${item.kind}`,
            `${item.longExchange}/${item.shortExchange}`,
            formatNumber(item.realizedSpreadPnl),
            formatNumber(item.fundingPnl),
            formatNumber(item.costPaid),
            formatNumber(item.netIfExit ?? ""),
            item.strategyVersion || "—",
            item.exitPendingReason || item.degradedReason || item.status,
          ])}
        />
      </Section>
      <Section title="决策">
        <Table
          headers={[
            "时间",
            "类型/标的",
            "24h净收益",
            "24h净收益折算年化",
            "资金费24h",
            "当前mid基差",
            "基差预测24h",
            "样本",
            "决策/概率",
            "执行状态",
          ]}
          emptyLabel={decisionEmptyCopy(snapshot, decisions)}
          rows={foldPositionHolds(orderJevDecisions(decisions)).map((item) => [
            formatTimestamp(item.decisionAt),
            formatCandidateLabel(item),
            formatPercentReturn(item.expectedNetReturn24h),
            formatAnnualizedReturn(item.expectedNetReturn24h),
            formatPercentReturn(item.fundingReturn24h),
            formatOptionalMetric(item.currentMidBasisBps),
            formatPercentReturn(item.expectedBasisReturn24h),
            formatOptionalCount(item.basisSampleCount),
            <DecisionProbCell key={`${item.id}-decision`} item={item} />,
            <ExecutionStatusCell key={`${item.id}-status`} item={item} />,
          ])}
        />
      </Section>
      <Section title="成交">
        <Table
          headers={["标的", "多头", "空头", "成交", "成本", "时间"]}
          rows={trades.map((item) => [
            `${item.baseAsset || "—"} ${item.kind || ""}`.trim(),
            formatTradeLeg(item.longExchange, item.longSymbol, item.longProduct, item.longFillPrice),
            formatTradeLeg(item.shortExchange, item.shortSymbol, item.shortProduct, item.shortFillPrice),
            `${item.side} ${item.sharedBaseQty} / ${formatNumber(item.notional)}`,
            formatNumber(item.cost),
            item.filledAt,
          ])}
        />
      </Section>
    </div>
    </TooltipProvider>
  );
}

function Metric({ label, value, hint }: { label: string; value: string; hint: string }) {
  return (
    <div className="rounded-lg border bg-card px-3 py-2">
      <Tooltip>
        <TooltipTrigger className="text-xs text-muted-foreground">{label}</TooltipTrigger>
        <TooltipContent>{hint}</TooltipContent>
      </Tooltip>
      <div className="mt-1 font-mono text-sm">{value || "—"}</div>
    </div>
  );
}

function StatusTooltip({
  label,
  hint,
  children,
}: {
  label: string;
  hint: string;
  children: React.ReactNode;
}) {
  return (
    <Tooltip>
      <TooltipTrigger aria-label={label} className="inline-flex">
        {children}
      </TooltipTrigger>
      <TooltipContent>{hint}</TooltipContent>
    </Tooltip>
  );
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="px-4 pb-4">
      <h2 className="mb-2 text-sm font-medium">{title}</h2>
      {children}
    </section>
  );
}

function Table({
  headers,
  rows,
  emptyLabel = "暂无数据",
}: {
  headers: string[];
  rows: React.ReactNode[][];
  emptyLabel?: string;
}) {
  return (
    <div className="overflow-x-auto rounded-lg border">
      <table className="w-full text-left text-xs">
        <thead className="bg-muted/40 text-muted-foreground">
          <tr>
            {headers.map((header) => (
              <th key={header} className="px-3 py-2 font-medium">
                {header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 ? (
            <tr>
              <td className="px-3 py-4 text-muted-foreground" colSpan={headers.length}>
                {emptyLabel}
              </td>
            </tr>
          ) : (
            rows.map((row, index) => (
              <tr key={index} className="border-t">
                {row.map((cell, cellIndex) => (
                  <td
                    key={cellIndex}
                    className={cn("px-3 py-2 font-mono", cellIndex === 0 && "font-sans")}
                  >
                    {cell ?? "—"}
                  </td>
                ))}
              </tr>
            ))
          )}
        </tbody>
      </table>
    </div>
  );
}

function FunnelSummary({ snapshot }: { snapshot: JevSnapshot | null }) {
  const single = snapshot?.singleFilter;
  const cross = snapshot?.crossFilter;
  return (
    <div className="space-y-1 text-sm text-muted-foreground">
      <p>
        单所：{single?.input ?? 0} 输入 → {single?.contractPassed ?? 0} 普通永续 → {single?.final ?? 0} 最终候选
      </p>
      <p>
        跨所：{cross?.input ?? 0} 输入 → {cross?.contractPassed ?? 0} 普通永续组合 → {cross?.final ?? 0} 最终候选
      </p>
    </div>
  );
}

function cycleStatusLabel(status: string | undefined): string {
  switch (status) {
    case "running":
      return "周期正常";
    case "degraded":
      return "周期降级";
    case "disabled":
      return "周期停用";
    case "stale":
      return "周期过期";
    default:
      return status || "unavailable";
  }
}

function modelBadgeLabel(snapshot: JevSnapshot | null): string {
  if (!snapshot || snapshot.modelStatus === "not_called") {
    return "模型未调用";
  }
  return `模型 ${snapshot.typesafeSuccess}/${snapshot.typesafeRequests}`;
}

function modelBadgeVariant(status: string | undefined): "default" | "secondary" | "destructive" | "outline" {
  if (status === "success") return "default";
  if (status === "error" || status === "unauthorized") return "destructive";
  return "outline";
}

function formatTimestamp(value: string | undefined): string {
  if (!value) return "—";
  return value;
}

export function decisionEmptyCopy(
  snapshot: JevSnapshot | null,
  decisions: JevDecision[],
): string {
  if (decisions.length > 0) {
    return "暂无数据";
  }
  const candidates = snapshot?.entryCandidateCount ?? 0;
  const status = snapshot?.modelStatus ?? "not_called";
  if (candidates <= 0) {
    return "本轮没有通过行情规则的候选";
  }
  if (status === "error" || status === "unauthorized") {
    return "模型调用失败";
  }
  if (status === "not_called") {
    return "发现候选，模型尚未调用";
  }
  return "模型已完成决策，暂无建仓信号";
}

export function formatCandidateLabel(item: Pick<JevDecision, "kind" | "candidateKey">): string {
  const parts = item.candidateKey.split(":");
  if (item.kind === "single" && parts.length === 4) {
    return `单所 ${parts[1]} ${parts[3]}`;
  }
  if (item.kind === "cross" && parts.length === 5) {
    return `跨所 ${parts[1]} ${parts[2]} / ${parts[3]} ${parts[4]}`;
  }
  return item.candidateKey;
}

export function formatPercentReturn(value: string | null | undefined): string {
  if (value === null || value === undefined || value === "") return "--";
  const parsed = Number(value);
  if (!Number.isFinite(parsed)) return "--";
  return `${(parsed * 100).toLocaleString("en-US", { maximumFractionDigits: 4 })}%`;
}

export function formatAnnualizedReturn(value: string | null | undefined): string {
  if (value === null || value === undefined || value === "") return "--";
  const parsed = Number(value);
  if (!Number.isFinite(parsed)) return "--";
  return `${(parsed * 365 * 100).toLocaleString("en-US", { maximumFractionDigits: 2 })}%`;
}

export function formatOptionalMetric(value: string | null | undefined): string {
  if (value === null || value === undefined || value === "") return "--";
  const parsed = Number(value);
  if (!Number.isFinite(parsed)) return "--";
  return parsed.toLocaleString("en-US", { maximumFractionDigits: 4 });
}

export function formatOptionalCount(value: number | null | undefined): string {
  if (value === null || value === undefined) return "--";
  return String(value);
}

export function executionStatusLabel(status: string): string {
  switch (status) {
    case "pending":
      return "待成交";
    case "filled":
      return "已成交";
    case "expired":
      return "已过期";
    case "fill_failed":
      return "成交失败";
    case "portfolio_rejected":
      return "仓位已满";
    case "not_applicable":
      return "无需执行/继续持有";
    default:
      return status || "—";
  }
}

export function foldPositionHolds(items: JevDecision[]): JevDecision[] {
  const seen = new Set<string>();
  const out: JevDecision[] = [];
  for (const item of items) {
    if (item.decision === "hold" && item.positionId) {
      if (seen.has(item.positionId)) {
        continue;
      }
      seen.add(item.positionId);
    }
    out.push(item);
  }
  return out;
}

export function formatTradeLeg(
  exchange: string | null | undefined,
  symbol: string | null | undefined,
  product: string | null | undefined,
  price: string,
): string {
  const venue = [exchange, symbol, product].filter(Boolean).join(" ");
  if (!venue) return formatNumber(price);
  return `${venue} @ ${formatNumber(price)}`;
}

export function orderJevDecisions(items: JevDecision[]): JevDecision[] {
  const groups = new Map<string, JevDecision[]>();
  for (const item of items) {
    const key = item.decisionAt || "";
    const list = groups.get(key) ?? [];
    list.push(item);
    groups.set(key, list);
  }
  const keys = [...groups.keys()].sort((left, right) => right.localeCompare(left));
  const out: JevDecision[] = [];
  for (const key of keys) {
    const group = groups.get(key) ?? [];
    group.sort((left, right) => {
      const net = Number(right.expectedNetReturn24h) - Number(left.expectedNetReturn24h);
      if (net !== 0 && Number.isFinite(net)) return net;
      return left.candidateKey.localeCompare(right.candidateKey);
    });
    out.push(...group);
  }
  return out;
}

function decisionProbabilityLabel(item: JevDecision): string {
  if (item.decision === "enter") {
    return `enter ${formatPercentReturn(item.enterProbability)}`;
  }
  if (item.decision === "exit") {
    return `exit ${formatPercentReturn(item.exitProbability)}`;
  }
  return item.decision || "hold";
}

function DecisionProbCell({ item }: { item: JevDecision }) {
  return (
    <Tooltip>
      <TooltipTrigger className="font-mono">{decisionProbabilityLabel(item)}</TooltipTrigger>
      <TooltipContent>{item.errorMessage || item.modelActual || item.modelRequested || item.promptVersion || "模型版本未知"}</TooltipContent>
    </Tooltip>
  );
}

function ExecutionStatusCell({ item }: { item: JevDecision }) {
  return (
    <Tooltip>
      <TooltipTrigger className="font-mono">{executionStatusLabel(item.executionStatus)}</TooltipTrigger>
      <TooltipContent>{item.errorMessage || item.modelActual || executionStatusLabel(item.executionStatus)}</TooltipContent>
    </Tooltip>
  );
}

function EquitySparkline({
  points,
}: {
  points: Array<{ ts: string; equity: string }>;
}) {
  const values = points
    .map((point) => Number(point.equity))
    .filter((value) => Number.isFinite(value));
  const distinct = new Set(values);
  if (values.length < 2 || distinct.size < 2) {
    return <div className="px-4 pb-4 text-xs text-muted-foreground">暂无权益变化</div>;
  }
  const min = Math.min(...values);
  const max = Math.max(...values);
  const span = max - min || 1;
  const d = values
    .map((value, index) => {
      const x = (index / (values.length - 1)) * 320;
      const y = 64 - ((value - min) / span) * 56 - 4;
      return `${index === 0 ? "M" : "L"}${x.toFixed(1)} ${y.toFixed(1)}`;
    })
    .join(" ");
  return (
    <div className="px-4 pb-4">
      <svg viewBox="0 0 320 64" className="h-16 w-full text-primary">
        <path d={d} fill="none" stroke="currentColor" strokeWidth="1.5" />
      </svg>
    </div>
  );
}
