"use client";

import * as React from "react";
import { Clock3, ListTree, ShieldCheck } from "lucide-react";

import { WideTableScroll } from "@/components/layout/responsive";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { SearchableSelect } from "@/components/ui/searchable-select";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { fetchTradingAccounts, groupAccountsByProduct, type TradingAccount } from "@/lib/api/accounts";
import {
  CEX_EXCHANGES,
  cancelTraderTwap,
  createTraderTwap,
  fetchTraderInstruments,
  fetchTraderTwap,
  fetchTraderTwapOrders,
  fetchTraderTwapPage,
  type TraderContractType,
  type TraderInstrument,
  type TraderOrder,
  type TraderSide,
  type TraderTwap,
  type TraderTwapOrderType,
  type TraderTwapView,
} from "@/lib/api/trader";
import { cn } from "@/lib/utils";

const selectClassName =
  "h-9 w-full rounded-lg border border-input bg-background px-3 text-sm outline-none transition-colors focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50";

function localDateTime(date: Date): string {
  const offset = date.getTimezoneOffset() * 60_000;
  return new Date(date.getTime() - offset).toISOString().slice(0, 16);
}

function initialTime(minutes: number): string {
  return localDateTime(new Date(Date.now() + minutes * 60_000));
}

function positiveDecimal(value: string): boolean {
  const parsed = Number(value);
  return value.trim() !== "" && Number.isFinite(parsed) && parsed > 0;
}

function positiveInteger(value: string): boolean {
  const parsed = Number(value);
  return Number.isInteger(parsed) && parsed > 0;
}

function instrumentOptions(items: TraderInstrument[]) {
  return items.map((item) => ({
    value: String(item.id),
    label: `${item.baseAsset} / ${item.quoteAsset} · ${item.exchangeSymbol}`,
    keywords: [item.exchangeSymbol, item.baseAsset, item.quoteAsset],
  }));
}

function formatNumber(value: string): string {
  const number = Number(value);
  if (!Number.isFinite(number)) return value || "—";
  return number.toLocaleString(undefined, { maximumFractionDigits: 8 });
}

function formatTime(value: string): string {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

function progressOf(twap: TraderTwap): number {
  const total = Number(twap.totalQty);
  const filled = Number(twap.filledQty);
  if (!(total > 0) || !Number.isFinite(filled)) return 0;
  return Math.min(100, Math.max(0, (filled / total) * 100));
}

const statusLabels: Record<string, string> = {
  pending: "待开始",
  scheduled: "待开始",
  running: "执行中",
  completed: "已完成",
  partially_completed: "部分完成",
  filled: "已完成",
  canceled: "已取消",
  cancelled: "已取消",
  failed: "失败",
  expired: "已过期",
};

function StatusBadge({ status }: { status: string }) {
  const normalized = status.toLowerCase();
  const className =
    normalized === "running"
      ? "border-sky-500/30 bg-sky-500/10 text-sky-700 dark:text-sky-300"
      : normalized === "completed" || normalized === "filled"
        ? "border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300"
        : normalized === "partially_completed"
          ? "border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-300"
        : normalized === "failed"
          ? "border-rose-500/30 bg-rose-500/10 text-rose-700 dark:text-rose-300"
          : "";
  return (
    <Badge variant="outline" className={className}>
      {statusLabels[normalized] ?? (status || "未知")}
    </Badge>
  );
}

export function TwapTradingView() {
  const [accounts, setAccounts] = React.useState<TradingAccount[]>([]);
  const [accountsError, setAccountsError] = React.useState<string | null>(null);
  const [productName, setProductName] = React.useState("");
  const [exchangeSlug, setExchangeSlug] = React.useState("");
  const [accountId, setAccountId] = React.useState<number | null>(null);
  const [contractType, setContractType] = React.useState<TraderContractType>("perpetual");
  const [instruments, setInstruments] = React.useState<TraderInstrument[]>([]);
  const [instrumentId, setInstrumentId] = React.useState<number | null>(null);
  const [side, setSide] = React.useState<TraderSide>("buy");
  const [totalQty, setTotalQty] = React.useState("");
  const [startAt, setStartAt] = React.useState(() => initialTime(5));
  const [endAt, setEndAt] = React.useState(() => initialTime(65));
  const [intervalSeconds, setIntervalSeconds] = React.useState("60");
  const [limitPrice, setLimitPrice] = React.useState("");
  const [maxQty, setMaxQty] = React.useState("");
  const [orderType, setOrderType] = React.useState<TraderTwapOrderType>("maker");
  const [orderTimeoutSeconds, setOrderTimeoutSeconds] = React.useState("30");
  const [formError, setFormError] = React.useState<string | null>(null);
  const [message, setMessage] = React.useState<string | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [confirmOpen, setConfirmOpen] = React.useState(false);
  const [view, setView] = React.useState<TraderTwapView>("running");
  const [twaps, setTwaps] = React.useState<TraderTwap[]>([]);
  const [activeCount, setActiveCount] = React.useState(0);
  const [nextCursor, setNextCursor] = React.useState("");
  const [listState, setListState] = React.useState<"loading" | "ready" | "error">("loading");
  const [detailOpen, setDetailOpen] = React.useState(false);
  const [detail, setDetail] = React.useState<TraderTwap | null>(null);
  const [detailOrders, setDetailOrders] = React.useState<TraderOrder[]>([]);
  const [detailLoading, setDetailLoading] = React.useState(false);

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
    () => Array.from(new Map(productAccounts.map((item) => [item.exchangeSlug, item.exchange])).entries()),
    [productAccounts],
  );
  const exchangeAccounts = productAccounts.filter((item) => item.exchangeSlug === exchangeSlug);
  const selectedAccount = exchangeAccounts.find((item) => item.id === accountId) ?? null;
  const selectedInstrument = instruments.find((item) => item.id === instrumentId) ?? null;

  React.useEffect(() => {
    void fetchTradingAccounts()
      .then((items) => {
        setAccounts(items);
        const firstProduct = groupAccountsByProduct(
          items.filter((item) => CEX_EXCHANGES.has(item.exchangeSlug)),
        )[0];
        const firstAccount = firstProduct?.accounts[0];
        setProductName(firstProduct?.productName ?? "");
        setExchangeSlug(firstAccount?.exchangeSlug ?? "");
        setAccountId(firstAccount?.id ?? null);
      })
      .catch((error: unknown) => {
        setAccountsError(error instanceof Error ? error.message : "账户加载失败");
      });
  }, []);

  React.useEffect(() => {
    if (accountId == null) {
      return;
    }
    let active = true;
    void fetchTraderInstruments(accountId, contractType)
      .then((items) => {
        if (!active) return;
        setInstruments(items);
        setInstrumentId((current) =>
          current && items.some((item) => item.id === current) ? current : items[0]?.id ?? null,
        );
      })
      .catch((error: unknown) => {
        if (!active) return;
        setInstruments([]);
        setInstrumentId(null);
        setMessage(error instanceof Error ? error.message : "标的加载失败");
      });
    return () => {
      active = false;
    };
  }, [accountId, contractType]);

  const refreshTwaps = React.useCallback(async (
    targetView: TraderTwapView,
    cursor = "",
    signal?: AbortSignal,
  ) => {
    if (!cursor) setListState("loading");
    const page = await fetchTraderTwapPage(undefined, {
      view: targetView,
      limit: 50,
      cursor,
      signal,
    });
    setTwaps((current) => (cursor ? [...current, ...page.items] : page.items));
    setNextCursor(page.nextCursor);
    setListState("ready");
  }, []);

  const refreshActiveCount = React.useCallback(async (
    targetAccountId: number,
    signal?: AbortSignal,
  ) => {
    const running = await fetchTraderTwapPage(targetAccountId, {
      view: "running",
      limit: 5,
      signal,
    });
    setActiveCount(running.items.length);
  }, []);

  React.useEffect(() => {
    if (accountId == null) {
      return;
    }
    let active = true;
    let timer = 0;
    const controller = new AbortController();
    async function load() {
      if (document.visibilityState === "hidden") {
        if (active && view === "running") timer = window.setTimeout(load, 8_000);
        return;
      }
      try {
        await Promise.all([
          refreshTwaps(view, "", controller.signal),
          refreshActiveCount(accountId!, controller.signal),
        ]);
      } catch {
        if (!controller.signal.aborted) setListState("error");
      }
      if (active && view === "running") timer = window.setTimeout(load, 8_000);
    }
    const onVisibility = () => {
      if (document.visibilityState === "visible") {
        window.clearTimeout(timer);
        void load();
      }
    };
    document.addEventListener("visibilitychange", onVisibility);
    void load();
    return () => {
      active = false;
      controller.abort();
      window.clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [accountId, refreshActiveCount, refreshTwaps, view]);

  function chooseProduct(next: string) {
    const account = products.find((item) => item.productName === next)?.accounts[0];
    setProductName(next);
    setExchangeSlug(account?.exchangeSlug ?? "");
    setAccountId(account?.id ?? null);
    setInstruments([]);
    setInstrumentId(null);
    if (!account) {
      setTwaps([]);
      setActiveCount(0);
    }
  }

  function chooseExchange(next: string) {
    const account = productAccounts.find((item) => item.exchangeSlug === next);
    setExchangeSlug(next);
    setAccountId(account?.id ?? null);
    setInstruments([]);
    setInstrumentId(null);
    if (!account) {
      setTwaps([]);
      setActiveCount(0);
    }
  }

  function validateForm(): string | null {
    if (!selectedAccount || !selectedInstrument) return "请选择账户和交易标的";
    if (!positiveDecimal(totalQty)) return "请输入有效的总数量";
    if (!startAt || !endAt || new Date(endAt).getTime() <= new Date(startAt).getTime()) {
      return "结束时间必须晚于开始时间";
    }
    if (!positiveInteger(intervalSeconds)) return "执行间隔必须为正整数秒";
    if (limitPrice && !positiveDecimal(limitPrice)) return "限价必须为正数";
    if (maxQty && !positiveDecimal(maxQty)) return "单笔上限必须为正数";
    if (maxQty && Number(maxQty) > Number(totalQty)) return "单笔上限不能超过总数量";
    if (orderType === "maker" && !positiveInteger(orderTimeoutSeconds)) {
      return "Maker 委托超时必须为正整数秒";
    }
    return null;
  }

  function openConfirm(event: React.FormEvent) {
    event.preventDefault();
    const error = validateForm();
    if (error) {
      setFormError(error);
      return;
    }
    setFormError(null);
    setConfirmOpen(true);
  }

  async function confirmCreate() {
    if (!selectedAccount || !selectedInstrument || busy) return;
    setBusy(true);
    try {
      const twap = await createTraderTwap({
        tradingAccountId: selectedAccount.id,
        instrumentId: selectedInstrument.id,
        side,
        totalQty: totalQty.trim(),
        startAt: new Date(startAt).toISOString(),
        endAt: new Date(endAt).toISOString(),
        intervalSeconds: Number(intervalSeconds),
        limitPrice: limitPrice.trim() || undefined,
        maxQty: maxQty.trim() || undefined,
        orderType,
        orderTimeoutSeconds: orderType === "maker" ? Number(orderTimeoutSeconds) : undefined,
      });
      setConfirmOpen(false);
      setView("running");
      setMessage(`TWAP 计划 ${twap.id} 已创建`);
      await Promise.all([
        refreshTwaps("running"),
        refreshActiveCount(selectedAccount.id),
      ]);
    } catch (error) {
      setMessage(error instanceof Error ? error.message : "TWAP 创建失败");
    } finally {
      setBusy(false);
    }
  }

  async function openDetail(twapId: string) {
    setDetailOpen(true);
    setDetailLoading(true);
    setDetail(null);
    setDetailOrders([]);
    try {
      const [nextDetail, orders] = await Promise.all([
        fetchTraderTwap(twapId),
        fetchTraderTwapOrders(twapId),
      ]);
      setDetail(nextDetail);
      setDetailOrders(orders);
    } catch (error) {
      setMessage(error instanceof Error ? error.message : "TWAP 详情加载失败");
    } finally {
      setDetailLoading(false);
    }
  }

  async function onCancel(twapId: string) {
    if (busy) return;
    setBusy(true);
    try {
      const canceled = await cancelTraderTwap(twapId);
      setMessage(`TWAP 计划 ${canceled.id || twapId} 已取消`);
      await Promise.all([
        refreshTwaps(view),
        accountId == null ? Promise.resolve() : refreshActiveCount(accountId),
      ]);
      if (detail?.id === twapId) setDetail(canceled);
    } catch (error) {
      setMessage(error instanceof Error ? error.message : "取消 TWAP 失败");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex min-h-0 flex-col">
      <header className="shrink-0 border-b px-5 py-4">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div>
            <div className="text-sm font-semibold">TWAP 计划</div>
            <p className="mt-1 text-xs text-muted-foreground">
              在指定时间窗内按固定间隔拆单，平滑完成目标数量。
            </p>
          </div>
          <div className="inline-flex items-center gap-3">
            <span className={cn(
              "rounded-full border px-2.5 py-1 text-[11px]",
              activeCount >= 5 ? "border-rose-500/30 text-rose-600" : "text-muted-foreground",
            )}>
              运行中 {activeCount} / 5
            </span>
            <div className="inline-flex items-center gap-1.5 rounded-full border bg-muted/40 px-2.5 py-1 text-[11px] text-muted-foreground">
            <ShieldCheck className="size-3" />
            创建前需二次确认
            </div>
          </div>
        </div>
      </header>

      <div className="min-h-0 flex-1 overflow-y-auto p-4 sm:p-6" data-twap-trading-scroll>
        <div className="mx-auto grid max-w-6xl gap-4">
          <form className="grid gap-4" onSubmit={openConfirm}>
            <section className="rounded-xl border bg-background/40 p-4">
              <div className="mb-4 flex items-center gap-2">
                <span className="flex size-7 items-center justify-center rounded-lg bg-primary/10 text-primary">
                  <ListTree className="size-3.5" />
                </span>
                <div>
                  <div className="text-sm font-medium">执行标的</div>
                  <div className="text-[11px] text-muted-foreground">按产品、交易所、账户依次选择</div>
                </div>
              </div>
              {accountsError ? (
                <p className="text-sm text-rose-600">{accountsError}</p>
              ) : (
                <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-5">
                  <Field label="产品">
                    <select className={selectClassName} value={productName} onChange={(event) => chooseProduct(event.target.value)}>
                      {products.length === 0 ? <option value="">暂无可用产品</option> : null}
                      {products.map((item) => <option key={item.productName} value={item.productName}>{item.productName}</option>)}
                    </select>
                  </Field>
                  <Field label="交易所">
                    <select className={selectClassName} value={exchangeSlug} onChange={(event) => chooseExchange(event.target.value)}>
                      {exchanges.length === 0 ? <option value="">暂无交易所</option> : null}
                      {exchanges.map(([slug, name]) => <option key={slug} value={slug}>{name}</option>)}
                    </select>
                  </Field>
                  <Field label="交易账户">
                    <select className={selectClassName} value={accountId ?? ""} onChange={(event) => {
                      setAccountId(Number(event.target.value) || null);
                      setInstruments([]);
                      setInstrumentId(null);
                    }}>
                      {exchangeAccounts.length === 0 ? <option value="">暂无账户</option> : null}
                      {exchangeAccounts.map((item) => <option key={item.id} value={item.id}>{item.accountName}</option>)}
                    </select>
                  </Field>
                  <Field label="合约类型">
                    <select className={selectClassName} value={contractType} onChange={(event) => {
                      setContractType(event.target.value as TraderContractType);
                      setInstruments([]);
                      setInstrumentId(null);
                    }}>
                      <option value="perpetual">线性永续</option>
                      <option value="spot">现货</option>
                    </select>
                  </Field>
                  <Field label="交易标的">
                    <SearchableSelect
                      key={`${accountId ?? "none"}-${contractType}`}
                      aria-label="TWAP 交易标的"
                      value={instrumentId == null ? "" : String(instrumentId)}
                      onValueChange={(next) => setInstrumentId(Number(next) || null)}
                      options={instrumentOptions(instruments)}
                      placeholder="搜索 BTC 或 BTCUSDT"
                      emptyText="无匹配标的"
                      disabled={instruments.length === 0}
                    />
                  </Field>
                </div>
              )}
            </section>

            <section className="rounded-xl border bg-background/40 p-4">
              <div className="mb-4 flex items-center gap-2">
                <span className="flex size-7 items-center justify-center rounded-lg bg-primary/10 text-primary">
                  <Clock3 className="size-3.5" />
                </span>
                <div>
                  <div className="text-sm font-medium">计划参数</div>
                  <div className="text-[11px] text-muted-foreground">数量均以基础资产计价</div>
                </div>
              </div>
              <div className="grid gap-5 xl:grid-cols-[minmax(0,1.5fr)_minmax(260px,0.65fr)]">
                <div className="grid gap-4">
                  <Field label="方向">
                    <div className="grid grid-cols-2 gap-2">
                      <Button type="button" variant={side === "buy" ? "default" : "outline"} className={cn(side === "buy" && "bg-emerald-600 text-white hover:bg-emerald-600/90")} onClick={() => setSide("buy")}>买入 / 做多</Button>
                      <Button type="button" variant={side === "sell" ? "default" : "outline"} className={cn(side === "sell" && "bg-rose-600 text-white hover:bg-rose-600/90")} onClick={() => setSide("sell")}>卖出 / 做空</Button>
                    </div>
                  </Field>
                  <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
                    <Field label={`总数量（${selectedInstrument?.baseAsset || "基础资产"}）`}>
                      <Input aria-label="总数量" inputMode="decimal" value={totalQty} onChange={(event) => setTotalQty(event.target.value)} placeholder="例如 1.5" className="h-9 font-mono" />
                    </Field>
                    <Field label="开始时间">
                      <Input aria-label="开始时间" type="datetime-local" value={startAt} onChange={(event) => setStartAt(event.target.value)} className="h-9" />
                    </Field>
                    <Field label="结束时间">
                      <Input aria-label="结束时间" type="datetime-local" value={endAt} onChange={(event) => setEndAt(event.target.value)} className="h-9" />
                    </Field>
                    <Field label="执行间隔（秒）">
                      <Input aria-label="执行间隔" type="number" min="1" step="1" value={intervalSeconds} onChange={(event) => setIntervalSeconds(event.target.value)} className="h-9 font-mono" />
                    </Field>
                    <Field label={`单笔上限（${selectedInstrument?.baseAsset || "基础资产"}，可选）`}>
                      <Input aria-label="单笔上限" inputMode="decimal" value={maxQty} onChange={(event) => setMaxQty(event.target.value)} placeholder="由系统均分" className="h-9 font-mono" />
                    </Field>
                    <Field label={`限价（${selectedInstrument?.quoteAsset || "报价资产"}，可选）`}>
                      <Input aria-label="限价" inputMode="decimal" value={limitPrice} onChange={(event) => setLimitPrice(event.target.value)} placeholder="不填则无价格保护" className="h-9 font-mono" />
                    </Field>
                  </div>
                  <Field label="子订单方式">
                    <div className="grid grid-cols-2 rounded-lg border bg-muted/30 p-1">
                      {(["maker", "market"] as const).map((type) => (
                        <button key={type} type="button" onClick={() => setOrderType(type)} className={cn("rounded-md px-3 py-1.5 text-sm transition-colors", orderType === type ? "bg-background font-medium shadow-sm" : "text-muted-foreground")}>
                          {type === "maker" ? "Maker 限价" : "Market 市价"}
                        </button>
                      ))}
                    </div>
                  </Field>
                  {orderType === "maker" ? (
                    <Field label="Maker 委托超时（秒）">
                      <Input aria-label="Maker 委托超时" type="number" min="1" step="1" value={orderTimeoutSeconds} onChange={(event) => setOrderTimeoutSeconds(event.target.value)} className="h-9 font-mono" />
                    </Field>
                  ) : null}
                  {formError ? <p className="text-sm text-rose-600">{formError}</p> : null}
                </div>

                <div className="flex flex-col rounded-xl border bg-muted/20 p-4">
                  <div className="text-xs font-medium text-muted-foreground">计划预览</div>
                  <dl className="mt-4 grid gap-3 text-sm">
                    <SummaryRow label="账户" value={selectedAccount ? `${selectedAccount.exchange} · ${selectedAccount.accountName}` : "—"} />
                    <SummaryRow label="标的" value={selectedInstrument?.exchangeSymbol || "—"} />
                    <SummaryRow label="方向" value={side === "buy" ? "买入 / 做多" : "卖出 / 做空"} valueClassName={side === "buy" ? "text-emerald-600" : "text-rose-600"} />
                    <SummaryRow label="总数量" value={`${totalQty || "—"} ${selectedInstrument?.baseAsset || ""}`} />
                    <SummaryRow label="执行窗口" value={startAt && endAt ? `${startAt.replace("T", " ")} → ${endAt.replace("T", " ")}` : "—"} />
                    <SummaryRow label="间隔" value={`${intervalSeconds || "—"} 秒`} />
                    <SummaryRow label="方式" value={orderType === "maker" ? `Maker · 超时 ${orderTimeoutSeconds || "—"} 秒` : "Market"} />
                  </dl>
                  <div className="mt-auto pt-6">
                    <Button type="submit" className="w-full" disabled={!selectedAccount || !selectedInstrument || busy || activeCount >= 5}>创建 TWAP 计划</Button>
                    <p className={cn("mt-2 text-center text-[11px]", message?.includes("失败") ? "text-rose-600" : "text-muted-foreground")}>
                      {message || "首次点击只打开确认，不会立即创建计划"}
                    </p>
                  </div>
                </div>
              </div>
            </section>
          </form>

          <TwapTable
            view={view}
            onViewChange={(next) => {
              setView(next);
              setTwaps([]);
              setNextCursor("");
            }}
            twaps={twaps}
            accounts={accounts}
            state={listState}
            busy={busy}
            onCancel={onCancel}
            onDetail={openDetail}
            hasMore={Boolean(nextCursor)}
            onLoadMore={() => {
              if (nextCursor) void refreshTwaps(view, nextCursor);
            }}
          />
        </div>
      </div>

      <Sheet open={confirmOpen} onOpenChange={setConfirmOpen}>
        <SheetContent>
          <SheetHeader>
            <SheetTitle>确认创建 TWAP</SheetTitle>
            <SheetDescription>请核对执行窗口与风险参数。确认时会生成唯一幂等键并仅提交一次。</SheetDescription>
          </SheetHeader>
          <dl className="grid gap-2 px-4 text-sm">
            <SummaryRow label="产品 / 交易所" value={`${productName || "—"} / ${selectedAccount?.exchange || "—"}`} />
            <SummaryRow label="账户" value={selectedAccount?.accountName || "—"} />
            <SummaryRow label="标的" value={selectedInstrument?.exchangeSymbol || "—"} />
            <SummaryRow label="方向" value={side === "buy" ? "买入 / 做多" : "卖出 / 做空"} />
            <SummaryRow label="总数量" value={`${totalQty} ${selectedInstrument?.baseAsset || ""}`} />
            <SummaryRow label="开始 / 结束" value={`${startAt.replace("T", " ")} → ${endAt.replace("T", " ")}`} />
            <SummaryRow label="间隔" value={`${intervalSeconds} 秒`} />
            <SummaryRow label="子订单" value={orderType === "maker" ? `Maker · 超时 ${orderTimeoutSeconds} 秒` : "Market"} />
            <SummaryRow label="限价 / 单笔上限" value={`${limitPrice || "无"} / ${maxQty || "自动"}`} />
          </dl>
          <SheetFooter>
            <Button type="button" variant="outline" onClick={() => setConfirmOpen(false)} disabled={busy}>返回修改</Button>
            <Button type="button" onClick={() => void confirmCreate()} disabled={busy}>{busy ? "创建中…" : "确认创建"}</Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>

      <Sheet open={detailOpen} onOpenChange={setDetailOpen}>
        <SheetContent className="sm:max-w-2xl">
          <SheetHeader>
            <SheetTitle>TWAP 详情</SheetTitle>
            <SheetDescription>{detail?.id || "加载计划与子订单…"}</SheetDescription>
          </SheetHeader>
          <div className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
            {detailLoading ? (
              <div className="py-10 text-center text-sm text-muted-foreground">加载中…</div>
            ) : detail ? (
              <div className="grid gap-5">
                <section className="rounded-xl border p-4">
                  <div className="flex items-center justify-between gap-3">
                    <div className="font-medium">{detail.exchangeSymbol}</div>
                    <StatusBadge status={detail.status} />
                  </div>
                  <div className="mt-4 h-2 overflow-hidden rounded-full bg-muted">
                    <div className="h-full rounded-full bg-primary transition-[width]" style={{ width: `${progressOf(detail)}%` }} />
                  </div>
                  <dl className="mt-4 grid gap-3 text-sm">
                    <SummaryRow label="成交进度" value={`${formatNumber(detail.filledQty)} / ${formatNumber(detail.totalQty)} ${detail.baseAsset} · ${progressOf(detail).toFixed(1)}%`} />
                    <SummaryRow label="方向 / 类型" value={`${detail.side === "buy" ? "买入" : "卖出"} / ${detail.orderType === "maker" ? "Maker" : "Market"}`} />
                    <SummaryRow label="平均成交价" value={formatNumber(detail.averagePrice)} />
                    <SummaryRow label="执行窗口" value={`${formatTime(detail.startAt)} → ${formatTime(detail.endAt)}`} />
                    <SummaryRow label="间隔 / 单笔上限" value={`${detail.intervalSeconds} 秒 / ${detail.maxQty || "自动"}`} />
                    <SummaryRow label="限价" value={detail.limitPrice || "无"} />
                    {detail.errorMessage ? <SummaryRow label="错误" value={detail.errorMessage} valueClassName="text-rose-600" /> : null}
                  </dl>
                </section>
                <section>
                  <div className="mb-2 flex items-center justify-between">
                    <div className="text-sm font-medium">子订单</div>
                    <span className="text-xs text-muted-foreground">{detailOrders.length} 笔</span>
                  </div>
                  <WideTableScroll>
                    <table className="w-full min-w-[560px] text-left text-xs">
                      <thead className="text-muted-foreground">
                        <tr><th className="pb-2 font-medium">时间</th><th className="pb-2 font-medium">方向 / 类型</th><th className="pb-2 font-medium">委托</th><th className="pb-2 font-medium">成交</th><th className="pb-2 font-medium">状态</th></tr>
                      </thead>
                      <tbody>
                        {detailOrders.length === 0 ? <tr><td colSpan={5} className="border-t py-8 text-center text-muted-foreground">暂无子订单</td></tr> : detailOrders.map((order) => (
                          <tr key={order.id} className="border-t">
                            <td className="py-2">{formatTime(order.createdAt)}</td>
                            <td className="py-2">{order.side === "buy" ? "买" : "卖"} / {order.orderType === "limit" ? "限价" : "市价"}<div className="text-muted-foreground">片 {order.twapSliceIndex} · 尝试 {order.twapAttemptIndex + 1}</div></td>
                            <td className="py-2 font-mono">{formatNumber(order.quantity)} {order.baseAsset}</td>
                            <td className="py-2 font-mono">{formatNumber(order.filledQuantity)}</td>
                            <td className="py-2">{order.status}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </WideTableScroll>
                </section>
              </div>
            ) : (
              <div className="py-10 text-center text-sm text-muted-foreground">详情加载失败</div>
            )}
          </div>
          {detail && view === "running" ? (
            <SheetFooter>
              <Button type="button" variant="destructive" disabled={busy} onClick={() => void onCancel(detail.id)}>取消计划</Button>
            </SheetFooter>
          ) : null}
        </SheetContent>
      </Sheet>
    </div>
  );
}

function TwapTable({
  view,
  onViewChange,
  twaps,
  accounts,
  state,
  busy,
  onCancel,
  onDetail,
  hasMore,
  onLoadMore,
}: {
  view: TraderTwapView;
  onViewChange: (view: TraderTwapView) => void;
  twaps: TraderTwap[];
  accounts: TradingAccount[];
  state: "loading" | "ready" | "error";
  busy: boolean;
  onCancel: (id: string) => void;
  onDetail: (id: string) => void;
  hasMore: boolean;
  onLoadMore: () => void;
}) {
  const accountNames = new Map(accounts.map((account) => [account.id, account.accountName]));
  return (
    <section className="rounded-xl border bg-background/40 p-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="grid grid-cols-2 rounded-lg border bg-muted/30 p-1">
          {(["running", "closed"] as const).map((item) => (
            <button key={item} type="button" onClick={() => onViewChange(item)} className={cn("rounded-md px-4 py-1.5 text-sm transition-colors", view === item ? "bg-background font-medium shadow-sm" : "text-muted-foreground")}>
              {item === "running" ? "执行中" : "已结束"}
            </button>
          ))}
        </div>
        <div className={cn("text-[11px] text-muted-foreground", state === "error" && "text-rose-600")}>
          {state === "loading" ? "同步中…" : state === "error" ? "同步失败，将自动重试" : view === "running" ? "自动刷新" : "历史快照"}
        </div>
      </div>
      <WideTableScroll className="mt-3">
        <table className="w-full min-w-[940px] text-left text-xs">
          <thead className="text-muted-foreground">
            <tr><th className="pb-2 font-medium">计划 / 标的</th><th className="pb-2 font-medium">交易所 / 账户</th><th className="pb-2 font-medium">方向</th><th className="pb-2 font-medium">执行窗口</th><th className="pb-2 font-medium">成交量</th><th className="pb-2 font-medium">进度</th><th className="pb-2 font-medium">状态</th><th className="pb-2 font-medium"></th></tr>
          </thead>
          <tbody>
            {twaps.length === 0 ? (
              <tr><td colSpan={8} className="border-t py-8 text-center text-muted-foreground">{state === "loading" ? "加载中…" : view === "running" ? "暂无执行中的 TWAP 计划" : "暂无已结束的 TWAP 计划"}</td></tr>
            ) : twaps.map((twap) => {
              const progress = progressOf(twap);
              return (
                <tr key={twap.id} className="border-t">
                  <td className="py-3"><div className="font-medium">{twap.exchangeSymbol || `#${twap.instrumentId}`}</div><div className="mt-0.5 font-mono text-[10px] text-muted-foreground">{twap.id}</div></td>
                  <td className="py-3"><div className="font-medium">{twap.exchange}</div><div className="text-muted-foreground">{accountNames.get(twap.tradingAccountId) ?? `#${twap.tradingAccountId}`}</div></td>
                  <td className={cn("py-3 font-medium", twap.side === "buy" ? "text-emerald-600" : "text-rose-600")}>{twap.side === "buy" ? "买入" : "卖出"}<div className="font-normal text-muted-foreground">{twap.orderType === "maker" ? "Maker" : "Market"}</div></td>
                  <td className="py-3">{formatTime(twap.startAt)}<div className="text-muted-foreground">至 {formatTime(twap.endAt)}</div></td>
                  <td className="py-3 font-mono">{formatNumber(twap.filledQty)} / {formatNumber(twap.totalQty)} {twap.baseAsset}</td>
                  <td className="py-3"><div className="w-24"><div className="h-1.5 overflow-hidden rounded-full bg-muted"><div className="h-full rounded-full bg-primary" style={{ width: `${progress}%` }} /></div><div className="mt-1 text-[10px] text-muted-foreground">{progress.toFixed(1)}%</div></div></td>
                  <td className="py-3"><StatusBadge status={twap.status} /></td>
                  <td className="py-3 text-right"><div className="flex justify-end gap-2"><Button type="button" size="xs" variant="outline" onClick={() => onDetail(twap.id)}>详情</Button>{view === "running" ? <Button type="button" size="xs" variant="outline" disabled={busy} onClick={() => onCancel(twap.id)}>取消</Button> : null}</div></td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </WideTableScroll>
      {hasMore ? <div className="mt-3 text-center"><Button type="button" size="sm" variant="outline" disabled={busy} onClick={onLoadMore}>加载更多</Button></div> : null}
    </section>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return <label className="grid gap-1.5 text-sm"><span className="text-xs text-muted-foreground">{label}</span>{children}</label>;
}

function SummaryRow({ label, value, valueClassName }: { label: string; value: string; valueClassName?: string }) {
  return <div className="flex items-start justify-between gap-3 border-b pb-2 last:border-0"><dt className="shrink-0 text-muted-foreground">{label}</dt><dd className={cn("text-right font-medium", valueClassName)}>{value}</dd></div>;
}
