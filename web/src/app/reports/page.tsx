"use client";

import * as React from "react";
import {
  ArrowDownRight,
  ArrowUpRight,
  BarChart3,
  CalendarDays,
  ChevronRight,
  CircleDollarSign,
  LoaderCircle,
  RefreshCw,
  WalletCards,
} from "lucide-react";

import { AuthGate } from "@/components/auth/auth-gate";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { PageFrame, WideTableScroll } from "@/components/layout/responsive";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import {
  createProductCashFlow,
  fetchProductCashFlows,
  fetchProductReport,
  fetchReportProducts,
  type CashFlowType,
  type ProductCashFlow,
  type ProductReport,
  type ReportProduct,
} from "@/lib/api/reports";
import { cn } from "@/lib/utils";

type FlowDraft = {
  amount: string;
  currency: string;
  occurredAt: string;
  note: string;
};

const emptyDraft: FlowDraft = {
  amount: "",
  currency: "USDT",
  occurredAt: "",
  note: "",
};

export default function ReportsPage() {
  return (
    <AuthGate
      redirectTo="/reports"
      title="需要登录"
      description="报表包含产品净值与资金流水，请先登录后再访问。"
    >
      <ReportsContent />
    </AuthGate>
  );
}

function ReportsContent() {
  const [products, setProducts] = React.useState<ReportProduct[]>([]);
  const [selectedId, setSelectedId] = React.useState<string | null>(null);
  const [productsLoading, setProductsLoading] = React.useState(true);
  const [productsError, setProductsError] = React.useState<string | null>(null);
  const [reloadKey, setReloadKey] = React.useState(0);

  const [report, setReport] = React.useState<ProductReport | null>(null);
  const [reportLoading, setReportLoading] = React.useState(false);
  const [reportError, setReportError] = React.useState<string | null>(null);
  const [cashFlows, setCashFlows] = React.useState<ProductCashFlow[]>([]);
  const [cashFlowsLoading, setCashFlowsLoading] = React.useState(false);
  const [cashFlowsError, setCashFlowsError] = React.useState<string | null>(null);

  const [flowOpen, setFlowOpen] = React.useState(false);
  const [flowType, setFlowType] = React.useState<CashFlowType>("subscription");
  const [draft, setDraft] = React.useState<FlowDraft>(emptyDraft);
  const [flowBusy, setFlowBusy] = React.useState(false);
  const [flowError, setFlowError] = React.useState<string | null>(null);
  const [recomputeNotice, setRecomputeNotice] = React.useState<string | null>(null);
  const reportRequest = React.useRef(0);

  React.useEffect(() => {
    const controller = new AbortController();
    fetchReportProducts(controller.signal)
      .then((next) => {
        setProducts(next);
        if (next.length === 0) {
          setReport(null);
          setCashFlows([]);
        }
        setSelectedId((current) =>
          current && next.some((item) => item.id === current)
            ? current
            : next[0]?.id ?? null,
        );
      })
      .catch((error: unknown) => {
        if (!controller.signal.aborted) {
          setProductsError(errorMessage(error, "加载产品失败"));
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) setProductsLoading(false);
      });
    return () => controller.abort();
  }, [reloadKey]);

  const refreshSelected = React.useCallback(async (productId: string) => {
    const requestId = ++reportRequest.current;
    await Promise.resolve();
    if (requestId !== reportRequest.current) return;
    setReportLoading(true);
    setCashFlowsLoading(true);
    setReportError(null);
    setCashFlowsError(null);

    const [reportResult, flowsResult] = await Promise.allSettled([
      fetchProductReport(productId),
      fetchProductCashFlows(productId),
    ]);
    if (requestId !== reportRequest.current) return;
    if (reportResult.status === "fulfilled") {
      setReport(reportResult.value);
      const stillRecomputing =
        reportResult.value.status === "recomputing" ||
        reportResult.value.rows.some((row) => row.status === "recomputing");
      if (!stillRecomputing) {
        setRecomputeNotice(null);
      }
    } else {
      setReport(null);
      setReportError(errorMessage(reportResult.reason, "加载报表失败"));
    }
    if (flowsResult.status === "fulfilled") {
      setCashFlows(flowsResult.value);
    } else {
      setCashFlows([]);
      setCashFlowsError(errorMessage(flowsResult.reason, "加载资金流水失败"));
    }
    setReportLoading(false);
    setCashFlowsLoading(false);
  }, []);

  React.useEffect(() => {
    if (!selectedId) return;
    const timer = window.setTimeout(() => void refreshSelected(selectedId), 0);
    return () => window.clearTimeout(timer);
  }, [refreshSelected, selectedId]);

  const reportRecomputing =
    report?.status === "recomputing" ||
    Boolean(report?.rows.some((row) => row.status === "recomputing"));

  React.useEffect(() => {
    if (!selectedId || !reportRecomputing) return;
    const timer = window.setInterval(() => {
      void refreshSelected(selectedId);
    }, 1500);
    return () => window.clearInterval(timer);
  }, [refreshSelected, reportRecomputing, selectedId]);

  function openNewFlow(type: "subscription" | "redemption") {
    const now = new Date();
    now.setMinutes(now.getMinutes() - now.getTimezoneOffset());
    setFlowType(type);
    setDraft({
      amount: "",
      currency: report?.product.currency || "USDT",
      occurredAt: now.toISOString().slice(0, 16),
      note: "",
    });
    setFlowError(null);
    setFlowOpen(true);
  }

  async function submitFlow(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!selectedId) return;
    const amount = draft.amount.trim();
    if (!/^(?:0|[1-9]\d*)(?:\.\d+)?$/.test(amount) || Number(amount) <= 0) {
      setFlowError("请输入大于 0 的十进制金额");
      return;
    }
    if (!draft.currency.trim() || !draft.occurredAt) {
      setFlowError("币种和实际发生时间不能为空");
      return;
    }
    const occurredAt = new Date(draft.occurredAt);
    if (!Number.isFinite(occurredAt.getTime())) {
      setFlowError("实际发生时间无效");
      return;
    }

    setFlowBusy(true);
    setFlowError(null);
    try {
      const fields = {
        amount,
        currency: draft.currency.trim().toUpperCase(),
        occurredAt: occurredAt.toISOString(),
        note: draft.note.trim(),
      };
      const created = await createProductCashFlow(selectedId, {
        ...fields,
        type: flowType,
        status: "confirmed",
      });
      setFlowOpen(false);
      if (created.recomputeStatus === "queued" && created.flowDate) {
        setRecomputeNotice(`正在重算 ${created.flowDate} 日报`);
      }
      await refreshSelected(selectedId);
    } catch (error) {
      setFlowError(errorMessage(error, "保存资金流水失败"));
    } finally {
      setFlowBusy(false);
    }
  }

  const selectedProduct =
    products.find((product) => product.id === selectedId) ?? null;

  return (
    <PageFrame>
      <div className="grid min-w-0 flex-1 gap-3 lg:grid-cols-[220px_minmax(0,1fr)]">
        <aside className="flex max-h-[min(24rem,var(--app-panel-height))] min-h-0 flex-col overflow-hidden rounded-xl border bg-card shadow-sm lg:max-h-[var(--app-panel-height)]">
          <div className="border-b px-4 py-3">
            <div className="text-sm font-medium">产品列表</div>
            <div className="mt-0.5 text-[11px] text-muted-foreground">
              admin · {products.length} 个产品
            </div>
          </div>
          <div className="min-h-0 flex-1 overflow-y-auto">
          <div className="grid gap-1.5 p-2">
            {productsLoading ? (
              <StateMessage loading>正在加载产品…</StateMessage>
            ) : productsError ? (
              <StateMessage>
                <span>{productsError}</span>
                <Button
                  size="xs"
                  variant="outline"
                  onClick={() => {
                    setProductsLoading(true);
                    setProductsError(null);
                    setReloadKey((value) => value + 1);
                  }}
                >
                  重试
                </Button>
              </StateMessage>
            ) : products.length === 0 ? (
              <StateMessage>暂无报表产品</StateMessage>
            ) : (
              products.map((product) => {
                const active = product.id === selectedId;
                return (
                  <button
                    key={product.id}
                    type="button"
                    onClick={() => setSelectedId(product.id)}
                    className={cn(
                      "group rounded-lg border border-transparent p-3 text-left transition-colors",
                      active
                        ? "border-primary/20 bg-primary/[0.08]"
                        : "hover:bg-muted/60",
                    )}
                  >
                    <div className="flex items-center justify-between gap-2">
                      <span className="truncate text-sm font-medium">
                        {product.displayName || product.name}
                      </span>
                      <ChevronRight
                        className={cn(
                          "size-3.5 text-muted-foreground transition-transform",
                          active && "translate-x-0.5 text-primary",
                        )}
                      />
                    </div>
                  </button>
                );
              })
            )}
          </div>
          </div>
        </aside>

        <main className="min-w-0 space-y-3">
          {!selectedProduct && !productsLoading ? (
            <section className="rounded-xl border bg-card p-8 text-center text-sm text-muted-foreground shadow-sm">
              {productsError ? "产品列表暂不可用" : "尚无可展示的产品日报"}
            </section>
          ) : reportLoading && !report ? (
            <section className="rounded-xl border bg-card p-10 shadow-sm">
              <StateMessage loading>正在加载产品日报…</StateMessage>
            </section>
          ) : reportError ? (
            <section className="rounded-xl border bg-card p-8 shadow-sm">
              <StateMessage>
                <span>{reportError}</span>
                {selectedId ? (
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => void refreshSelected(selectedId)}
                  >
                    <RefreshCw className="size-3.5" />
                    重试
                  </Button>
                ) : null}
              </StateMessage>
            </section>
          ) : report ? (
            <>
              {(recomputeNotice || reportRecomputing) && (
                <section className="rounded-xl border border-sky-500/30 bg-sky-500/[0.06] px-4 py-3 text-xs text-sky-800 dark:text-sky-300">
                  {recomputeNotice ?? "正在按资金流水重算日报，收益率暂不展示。"}
                </section>
              )}
              <ReportSummary
                report={report}
                refreshing={reportLoading}
                onSubscribe={() => openNewFlow("subscription")}
                onRedeem={() => openNewFlow("redemption")}
                onRefresh={() => selectedId && void refreshSelected(selectedId)}
              />

              <QualityNotice report={report} />

              {report.latest ? (
                <>
                  <AumChart report={report} />
                  <ReportTable report={report} />
                </>
              ) : (
                <section className="rounded-xl border bg-card p-8 text-center shadow-sm">
                  <CalendarDays className="mx-auto size-6 text-muted-foreground" />
                  <div className="mt-2 text-sm font-medium">首日报告尚未生成</div>
                  <p className="mt-1 text-xs text-muted-foreground">
                    首个完整的 08:55–09:00 采样窗口结束后会显示数据。
                  </p>
                </section>
              )}

              <CashFlowSection
                flows={cashFlows}
                loading={cashFlowsLoading}
                error={cashFlowsError}
              />
            </>
          ) : null}
        </main>
      </div>

      <CashFlowSheet
        open={flowOpen}
        onOpenChange={setFlowOpen}
        type={flowType}
        draft={draft}
        setDraft={setDraft}
        error={flowError}
        busy={flowBusy}
        onSubmit={submitFlow}
      />
    </PageFrame>
  );
}

function ReportSummary({
  report,
  refreshing,
  onSubscribe,
  onRedeem,
  onRefresh,
}: {
  report: ProductReport;
  refreshing: boolean;
  onSubscribe: () => void;
  onRedeem: () => void;
  onRefresh: () => void;
}) {
  const latest = report.latest;
  const series = report.aumSeries;
  const previousAum = series.at(-2)?.aum;
  const aumChange =
    latest && previousAum && previousAum !== 0
      ? (latest.aum - previousAum) / previousAum
      : null;

  return (
    <section className="rounded-xl border bg-card p-4 shadow-sm">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <div className="flex items-center gap-2">
            <h2 className="text-lg font-semibold">
              {report.product.displayName || report.product.name}
            </h2>
            {report.product.category ? (
              <Badge variant="outline">{report.product.category}</Badge>
            ) : null}
            {report.status && report.status !== "final" ? (
              <Badge variant="outline">{statusLabel(report.status)}</Badge>
            ) : null}
          </div>
          <p className="mt-1 text-xs text-muted-foreground">
            admin / {report.product.strategy || "未设置策略"}
          </p>
        </div>
        <div className="flex flex-wrap items-center justify-end gap-2">
          <Button size="sm" onClick={onSubscribe}>申购</Button>
          <Button size="sm" variant="outline" onClick={onRedeem}>赎回</Button>
          <Button
            size="icon-sm"
            variant="ghost"
            aria-label="刷新报表"
            disabled={refreshing}
            onClick={onRefresh}
          >
            <RefreshCw className={cn("size-3.5", refreshing && "animate-spin")} />
          </Button>
          <div className="flex items-center gap-2 rounded-lg border bg-background/60 px-3 py-2 text-xs text-muted-foreground">
            <CalendarDays className="size-3.5" />
            数据日期：{report.dataDate ?? "尚无"}
          </div>
        </div>
      </div>

      <div className="mt-5 grid gap-2 sm:grid-cols-2 xl:grid-cols-3">
        <MetricCard
          icon={CircleDollarSign}
          label="资金规模 AUM"
          value={latest ? formatMoney(latest.aum, report.product.currency) : "—"}
          detail={
            aumChange === null
              ? "暂无资金规模对比"
              : `${formatRatio(aumChange)} 资金规模变化（含申赎）`
          }
          positive={aumChange === null ? undefined : aumChange >= 0}
        />
        <MetricCard
          icon={ArrowUpRight}
          label="累计绝对收益"
          value={
            latest
              ? formatMoney(latest.absoluteReturn, report.product.currency)
              : "—"
          }
          detail={
            latest?.status === "recomputing"
              ? "该日正在重算，暂不展示收益率"
              : `日收益 ${formatNullableRatio(latest?.dailyReturn)} · 收益已排除申购、赎回等外部资金流影响`
          }
          positive={
            latest ? latest.absoluteReturn >= 0 : undefined
          }
        />
        <MetricCard
          icon={BarChart3}
          label="24h 成交量"
          value={
            latest?.volume24h == null
              ? "待同步"
              : formatMoney(latest.volume24h, report.product.currency)
          }
          detail={`资金利用率 ${formatNullableRatio(latest?.capitalUtilization)}`}
        />
      </div>
    </section>
  );
}

function QualityNotice({ report }: { report: ProductReport }) {
  if (report.status === "recomputing") {
    return null;
  }
  if (report.status === "final" && !report.partial && report.errors.length === 0) {
    return null;
  }
  return (
    <section className="rounded-xl border border-amber-500/30 bg-amber-500/[0.06] px-4 py-3 text-xs text-amber-800 dark:text-amber-300">
      <div className="font-medium">
        {report.status === "provisional"
          ? "当前为临时报表，采样完整后可能更新。"
          : report.status === "failed"
            ? "本期报表生成失败。"
            : "部分数据尚未同步完成。"}
      </div>
      {report.partial ? <div className="mt-1">数据不完整，请勿将当前值视为正式净值。</div> : null}
      {report.errors.map((error) => (
        <div key={error} className="mt-1">{error}</div>
      ))}
    </section>
  );
}

function AumChart({ report }: { report: ProductReport }) {
  const series = report.aumSeries;
  if (series.length === 0) {
    return (
      <section className="rounded-xl border bg-card p-6 text-center text-xs text-muted-foreground shadow-sm">
        暂无 AUM 历史走势
      </section>
    );
  }
  const values = series.map((point) => point.aum);
  const min = Math.min(...values) * 0.995;
  const max = Math.max(...values) * 1.005;
  const range = max - min || 1;
  const divisor = Math.max(series.length - 1, 1);
  const coordinates = values.map((value, index) => ({
    x: 18 + (index / divisor) * 664,
    y: 142 - ((value - min) / range) * 116,
  }));
  const points = coordinates
    .map(({ x, y }) => `${x.toFixed(1)},${y.toFixed(1)}`)
    .join(" ");
  const first = values[0];
  const last = values.at(-1) ?? first;
  const change = first === 0 ? null : (last - first) / first;

  return (
    <section className="rounded-xl border bg-card p-4 shadow-sm">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h3 className="text-sm font-medium">AUM 资金规模走势</h3>
          <p className="mt-0.5 text-[11px] text-muted-foreground">
            最近 {series.length} 个交易日
          </p>
        </div>
        <div className="flex gap-6 text-right">
          <div>
            <div className="text-[9px] uppercase tracking-wider text-muted-foreground">当前</div>
            <div className="mt-1 font-mono text-sm font-semibold">
              {formatMoney(last, report.product.currency)}
            </div>
          </div>
          <div>
            <div className="text-[9px] uppercase tracking-wider text-muted-foreground">
              资金规模变化（含申赎）
            </div>
            <div
              className={cn(
                "mt-1 flex items-center justify-end gap-1 font-mono text-sm font-semibold",
                change !== null && (change >= 0 ? "text-emerald-600" : "text-rose-600"),
              )}
            >
              {change === null ? null : change >= 0 ? (
                <ArrowUpRight className="size-3.5" />
              ) : (
                <ArrowDownRight className="size-3.5" />
              )}
              {change === null ? "—" : formatRatio(change)}
            </div>
          </div>
        </div>
      </div>

      <WideTableScroll className="mt-3">
        <svg
          viewBox="0 0 700 180"
          role="img"
          aria-label={`${report.product.name} AUM 走势图`}
          className="h-48 w-full min-w-[520px]"
        >
          {[26, 65, 104, 142].map((y) => (
            <line
              key={y}
              x1="18"
              y1={y}
              x2="682"
              y2={y}
              stroke="currentColor"
              className="text-border"
              strokeWidth="1"
              strokeDasharray="3 5"
            />
          ))}
          <polygon
            points={`${points} ${coordinates.at(-1)?.x ?? 18},151 18,151`}
            fill="currentColor"
            className="text-primary"
            opacity="0.08"
          />
          <polyline
            points={points}
            fill="none"
            stroke="currentColor"
            className="text-primary"
            strokeWidth="2.5"
            strokeLinejoin="round"
            strokeLinecap="round"
          />
          {series.map((point, index) => {
            const coordinate = coordinates[index];
            if (!coordinate) return null;
            return (
              <g key={point.date}>
                <circle
                  cx={coordinate.x}
                  cy={coordinate.y}
                  r={index === series.length - 1 ? 4 : 2.5}
                  fill="currentColor"
                  className="text-primary"
                />
                <text
                  x={coordinate.x}
                  y="172"
                  textAnchor="middle"
                  className="fill-muted-foreground text-[9px]"
                >
                  {formatChartDate(point.date)}
                </text>
                {index === series.length - 1 ? (
                  <text
                    x={coordinate.x - 4}
                    y={coordinate.y - 10}
                    textAnchor="end"
                    className="fill-foreground text-[9px] font-medium"
                  >
                    {formatNumber(point.aum)}
                  </text>
                ) : null}
              </g>
            );
          })}
        </svg>
      </WideTableScroll>
    </section>
  );
}

function ReportTable({ report }: { report: ProductReport }) {
  return (
    <section className="overflow-hidden rounded-xl border bg-card shadow-sm">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b px-4 py-3">
        <div>
          <h3 className="text-sm font-medium">绩效明细</h3>
          <p className="mt-0.5 text-[11px] text-muted-foreground">
            收益已排除申购、赎回等外部资金流影响
          </p>
        </div>
        <Badge variant="outline">单位：{report.product.currency}</Badge>
      </div>
      {report.rows.length === 0 ? (
        <div className="p-8 text-center text-xs text-muted-foreground">暂无绩效明细</div>
      ) : (
        <WideTableScroll>
          <table className="w-full min-w-[1420px] text-xs">
            <thead className="sticky top-0 z-10 bg-muted/35 text-[10px] text-muted-foreground">
              <tr className="border-b">
                <ReportHeader align="left">日期</ReportHeader>
                <ReportHeader>绝对收益<br />（{report.product.currency}）</ReportHeader>
                <ReportHeader>按日收益<br />（%）</ReportHeader>
                <ReportHeader>年化收益</ReportHeader>
                <ReportHeader>7日年化</ReportHeader>
                <ReportHeader>30日年化</ReportHeader>
                <ReportHeader>最大回撤</ReportHeader>
                <ReportHeader>资金利用率</ReportHeader>
                <ReportHeader>夏普率</ReportHeader>
                <ReportHeader>资金规模<br />（{report.product.currency}）</ReportHeader>
                <ReportHeader>24h成交量<br />（{report.product.currency}）</ReportHeader>
              </tr>
            </thead>
            <tbody>
              {report.rows.map((row, index) => (
                <tr
                  key={row.date}
                  className={cn(
                    "border-b transition-colors last:border-0 hover:bg-muted/25",
                    index === 0 && "bg-primary/[0.035]",
                  )}
                >
                  <td className="whitespace-nowrap px-4 py-3 text-left font-mono font-medium">
                    <div className="flex items-center gap-1.5">
                      {row.date}
                      {row.status === "recomputing" ? (
                        <span className="text-[9px] text-sky-600">重算中</span>
                      ) : row.status !== "final" || row.partial ? (
                        <span className="text-[9px] text-amber-600">
                          {row.status === "failed" ? "异常" : "临时"}
                        </span>
                      ) : null}
                    </div>
                  </td>
                  <MoneyReturnCell value={row.absoluteReturn} />
                  <td className="px-4 py-3 text-right font-mono tabular-nums">
                    {row.status === "recomputing"
                      ? "重算中"
                      : formatNullableRatio(row.dailyReturn)}
                  </td>
                  <RatioCell value={row.annualizedReturn} insufficient />
                  <RatioCell value={row.annualized7d} insufficient />
                  <RatioCell value={row.annualized30d} insufficient />
                  <RatioCell value={row.maxDrawdown} />
                  <td className="px-4 py-3 text-right font-mono tabular-nums">
                    {formatNullableRatio(row.capitalUtilization)}
                  </td>
                  <td className="px-4 py-3 text-right font-mono tabular-nums">
                    {row.sharpe == null ? "数据不足" : row.sharpe.toFixed(2)}
                  </td>
                  <td className="px-4 py-3 text-right font-mono font-medium tabular-nums">
                    {formatNumber(row.aum)}
                  </td>
                  <td className="px-4 py-3 text-right font-mono tabular-nums">
                    {row.volume24h == null ? "待同步" : formatNumber(row.volume24h)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </WideTableScroll>
      )}
    </section>
  );
}

function CashFlowSection({
  flows,
  loading,
  error,
}: {
  flows: ProductCashFlow[];
  loading: boolean;
  error: string | null;
}) {
  return (
    <section className="overflow-hidden rounded-xl border bg-card shadow-sm">
      <div className="border-b px-4 py-3">
        <div className="flex items-center gap-2">
          <WalletCards className="size-4" />
          <h3 className="text-sm font-medium">资金流水</h3>
        </div>
        <p className="mt-1 text-[11px] text-muted-foreground">
          仅用于报表收益修正，请按实际到账/转出时间登记；不会执行真实转账。
        </p>
      </div>
      {loading ? (
        <StateMessage loading>正在加载资金流水…</StateMessage>
      ) : error ? (
        <StateMessage>{error}</StateMessage>
      ) : flows.length === 0 ? (
        <StateMessage>暂无手工资金流水</StateMessage>
      ) : (
        <WideTableScroll>
          <table className="w-full min-w-[860px] text-xs">
            <thead className="sticky top-0 z-10 bg-muted/35 text-[10px] text-muted-foreground">
              <tr className="border-b">
                <ReportHeader align="left">实际发生时间</ReportHeader>
                <ReportHeader align="left">归属报表日</ReportHeader>
                <ReportHeader align="left">类型</ReportHeader>
                <ReportHeader>金额</ReportHeader>
                <ReportHeader align="left">备注</ReportHeader>
                <ReportHeader align="left">状态</ReportHeader>
              </tr>
            </thead>
            <tbody>
              {flows.map((flow) => (
                <tr key={flow.id} className="border-b last:border-0">
                  <td className="whitespace-nowrap px-4 py-3 font-mono">
                    {formatDateTime(flow.occurredAt)}
                  </td>
                  <td className="whitespace-nowrap px-4 py-3 font-mono">
                    {flow.flowDate || "—"}
                  </td>
                  <td className="px-4 py-3">{flowTypeLabel(flow.type)}</td>
                  <td className="px-4 py-3 text-right font-mono tabular-nums">
                    {flow.type === "redemption" || flow.type === "withdrawal" ? "−" : "+"}
                    {formatUnsignedAmount(flow.amount)} {flow.currency}
                  </td>
                  <td className="max-w-64 truncate px-4 py-3 text-muted-foreground">
                    {flow.note || "—"}
                  </td>
                  <td className="px-4 py-3">
                    <Badge variant="outline">{flowStatusLabel(flow.status)}</Badge>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </WideTableScroll>
      )}
    </section>
  );
}

function CashFlowSheet({
  open,
  onOpenChange,
  type,
  draft,
  setDraft,
  error,
  busy,
  onSubmit,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  type: CashFlowType;
  draft: FlowDraft;
  setDraft: React.Dispatch<React.SetStateAction<FlowDraft>>;
  error: string | null;
  busy: boolean;
  onSubmit: (event: React.FormEvent<HTMLFormElement>) => void;
}) {
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="sm:max-w-md">
        <SheetHeader>
          <SheetTitle>
            登记{flowTypeLabel(type)}
          </SheetTitle>
          <SheetDescription>
            提交后按实际发生时间归属报表日并重算该日收益，不会发起交易所转账。
          </SheetDescription>
        </SheetHeader>
        <form
          id="cash-flow-form"
          className="grid flex-1 content-start gap-4 px-4"
          onSubmit={onSubmit}
        >
          <FormField label="金额">
            <Input
              autoFocus
              inputMode="decimal"
              placeholder="0.00"
              value={draft.amount}
              onChange={(event) =>
                setDraft((current) => ({ ...current, amount: event.target.value }))
              }
            />
          </FormField>
          <FormField label="币种">
            <Input
              placeholder="USDT"
              value={draft.currency}
              onChange={(event) =>
                setDraft((current) => ({ ...current, currency: event.target.value }))
              }
            />
          </FormField>
          <FormField label="实际发生时间">
            <Input
              type="datetime-local"
              value={draft.occurredAt}
              onChange={(event) =>
                setDraft((current) => ({ ...current, occurredAt: event.target.value }))
              }
            />
          </FormField>
          <FormField label="备注（可选）">
            <textarea
              className="min-h-20 w-full resize-y rounded-lg border border-input bg-transparent px-2.5 py-2 text-sm outline-none placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
              placeholder="到账说明、外部流水号等"
              value={draft.note}
              onChange={(event) =>
                setDraft((current) => ({ ...current, note: event.target.value }))
              }
            />
          </FormField>
          {error ? <p className="text-xs text-destructive">{error}</p> : null}
        </form>
        <SheetFooter>
          <Button type="submit" form="cash-flow-form" disabled={busy}>
            {busy ? <LoaderCircle className="size-4 animate-spin" /> : null}
            确认登记
          </Button>
          <Button type="button" variant="outline" disabled={busy} onClick={() => onOpenChange(false)}>
            取消
          </Button>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  );
}

function FormField({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="grid gap-1.5 text-xs font-medium">
      {label}
      {children}
    </label>
  );
}

function StateMessage({
  children,
  loading = false,
}: {
  children: React.ReactNode;
  loading?: boolean;
}) {
  return (
    <div className="flex min-h-20 flex-col items-center justify-center gap-2 p-4 text-center text-xs text-muted-foreground">
      {loading ? <LoaderCircle className="size-4 animate-spin" /> : null}
      {children}
    </div>
  );
}

function MetricCard({
  icon: Icon,
  label,
  value,
  detail,
  positive,
}: {
  icon: React.ComponentType<{ className?: string }>;
  label: string;
  value: string;
  detail: string;
  positive?: boolean;
}) {
  return (
    <div className="rounded-xl border bg-muted/15 p-3">
      <div className="flex items-center gap-1.5 text-[10px] text-muted-foreground">
        <Icon className="size-3.5" />
        {label}
      </div>
      <div className="mt-2 font-mono text-lg font-semibold tabular-nums">{value}</div>
      <div
        className={cn(
          "mt-1 text-[10px] text-muted-foreground",
          positive === true && "text-emerald-600",
          positive === false && "text-rose-600",
        )}
      >
        {detail}
      </div>
    </div>
  );
}

function ReportHeader({
  children,
  align = "right",
}: {
  children: React.ReactNode;
  align?: "left" | "right";
}) {
  return (
    <th
      className={cn(
        "whitespace-nowrap px-4 py-2.5 font-medium leading-4",
        align === "left" ? "text-left" : "text-right",
      )}
    >
      {children}
    </th>
  );
}

function MoneyReturnCell({ value }: { value: number }) {
  return (
    <td
      className={cn(
        "px-4 py-3 text-right font-mono tabular-nums",
        value > 0 && "text-emerald-600",
        value < 0 && "text-rose-600",
      )}
    >
      {value >= 0 ? "+" : ""}
      {formatNumber(value)}
    </td>
  );
}

function RatioCell({
  value,
  insufficient = false,
}: {
  value: number | null;
  insufficient?: boolean;
}) {
  return (
    <td
      className={cn(
        "px-4 py-3 text-right font-mono tabular-nums",
        value !== null && value > 0 && "text-emerald-600",
        value !== null && value < 0 && "text-rose-600",
      )}
    >
      {value == null && insufficient ? "数据不足" : formatNullableRatio(value)}
    </td>
  );
}

function formatUnsignedAmount(value: string) {
  return value.startsWith("-") ? value.slice(1) : value;
}

function formatMoney(value: number, currency: string) {
  return `${formatNumber(value)} ${currency}`;
}

function formatNumber(value: number) {
  return new Intl.NumberFormat("en-US", {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(value);
}

function formatRatio(value: number) {
  return `${value >= 0 ? "+" : ""}${(value * 100).toFixed(2)}%`;
}

function formatNullableRatio(value: number | null | undefined) {
  return value == null ? "—" : formatRatio(value);
}

function formatChartDate(value: string) {
  const match = /^(\d{4})-(\d{2})-(\d{2})/.exec(value);
  return match ? `${match[2]}/${match[3]}` : value;
}

function formatDateTime(value: string) {
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return value;
  return new Intl.DateTimeFormat("zh-CN", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  }).format(date);
}

function statusLabel(status: string) {
  if (status === "provisional") return "临时";
  if (status === "failed") return "失败";
  if (status === "recomputing") return "重算中";
  return "正式";
}

function flowTypeLabel(type: CashFlowType) {
  const labels: Record<CashFlowType, string> = {
    subscription: "申购",
    redemption: "赎回",
    deposit: "充值",
    withdrawal: "提现",
  };
  return labels[type];
}

function flowStatusLabel(status: ProductCashFlow["status"]) {
  if (status === "confirmed") return "已确认";
  if (status === "canceled") return "已撤销";
  return "待确认";
}

function errorMessage(error: unknown, fallback: string) {
  return error instanceof Error ? error.message : fallback;
}
