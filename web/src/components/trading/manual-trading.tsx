"use client";

import * as React from "react";
import { BookOpen, ShieldCheck } from "lucide-react";

import {
  isWalletDexExchange,
  tradingAccountStatusLabel,
} from "@/lib/api/accounts";
import { useProductTradingAccounts } from "@/hooks/use-product-trading-accounts";
import {
  cancelTraderOrder,
  fetchTraderInstrumentCatalog,
  fetchTraderInstruments,
  fetchTraderOrderPage,
  placeTraderOrder,
  type TraderContractType,
  type TraderInstrument,
  type TraderOrder,
  type TraderOrderType,
  type TraderSide,
  type TraderVenueCapabilities,
} from "@/lib/api/trader";
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
import { WideTableScroll } from "@/components/layout/responsive";
import { isAbortError } from "@/lib/abort";
import { useCloseOnHidden } from "@/lib/close-on-hidden";
import { cn } from "@/lib/utils";

const selectClassName =
  "h-9 w-full rounded-lg border border-input bg-background px-3 text-sm outline-none transition-colors focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50";

type OrderView = "open" | "history";

function instrumentOptions(items: TraderInstrument[]) {
  return items.map((item) => ({
    value: String(item.id),
    label: `${item.baseAsset} / ${item.quoteAsset} · ${item.exchangeSymbol}`,
    keywords: [
      item.exchangeSymbol,
      item.baseAsset,
      item.quoteAsset,
      `${item.baseAsset}${item.quoteAsset}`,
      `${item.baseAsset}/${item.quoteAsset}`,
      `${item.baseAsset}-${item.quoteAsset}`,
    ],
  }));
}

async function loadInstrumentCatalog(
  accountId: number,
  contractType: TraderContractType,
): Promise<{ items: TraderInstrument[]; capabilities: TraderVenueCapabilities }> {
  if (typeof fetchTraderInstrumentCatalog === "function") {
    return fetchTraderInstrumentCatalog(accountId, contractType);
  }
  return {
    items: await fetchTraderInstruments(accountId, contractType),
    capabilities: {
      products: ["spot", "perpetual"],
      quoteAssets: [],
      timeInForce: ["GTC", "IOC", "POST_ONLY"],
      postOnly: true,
      reduceOnly: true,
      makerTwap: true,
      privateOrderStream: false,
      oneWayOnly: false,
    },
  };
}

function terminalOrderStatus(status: string): boolean {
  switch (status) {
    case "filled":
    case "canceled":
    case "rejected":
    case "expired":
      return true;
    default:
      return false;
  }
}

function orderStatusLabel(status: string): string {
  if (status === "pending") return "已提交，待交易所确认";
  if (status === "unknown") return "状态不确定，正在对账";
  return status;
}

function positiveDecimal(value: string): boolean {
  const parsed = Number(value);
  return value.trim() !== "" && Number.isFinite(parsed) && parsed > 0;
}

function mergeOrderRows(current: TraderOrder[], incoming: TraderOrder[]): TraderOrder[] {
  const previous = new Map(current.map((item) => [item.id, item]));
  return incoming.map((item) => {
    const old = previous.get(item.id);
    return old && old.updatedAt === item.updatedAt && old.syncState === item.syncState ? old : item;
  });
}

function formatDecimal(value: string): string {
  const trimmed = value.trim();
  if (!trimmed) return "—";
  if (!trimmed.includes(".")) return trimmed;
  return trimmed.replace(/0+$/, "").replace(/\.$/, "") || "0";
}

export function ManualTradingView() {
  const { accounts, products, accountsError, inspectOne } = useProductTradingAccounts();
  const [productName, setProductName] = React.useState("");
  const [accountId, setAccountId] = React.useState<number | null>(null);
  const [contractType, setContractType] = React.useState<TraderContractType>("perpetual");
  const [instruments, setInstruments] = React.useState<TraderInstrument[]>([]);
  const [capabilities, setCapabilities] = React.useState<TraderVenueCapabilities | null>(null);
  const [instrumentId, setInstrumentId] = React.useState<number | null>(null);
  const [side, setSide] = React.useState<TraderSide>("buy");
  const [orderType, setOrderType] = React.useState<TraderOrderType>("limit");
  const [price, setPrice] = React.useState("");
  const [quantity, setQuantity] = React.useState("");
  const [formError, setFormError] = React.useState<string | null>(null);
  const [confirmOpen, setConfirmOpen] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [message, setMessage] = React.useState<string | null>(null);
  const [messageKind, setMessageKind] = React.useState<"success" | "error">("success");
  const [orders, setOrders] = React.useState<TraderOrder[]>([]);
  const [orderView, setOrderView] = React.useState<OrderView>("open");
  const [nextCursor, setNextCursor] = React.useState("");
  const [orderSyncState, setOrderSyncState] = React.useState<"syncing" | "synced" | "delayed">("syncing");
  const [lastSyncedAt, setLastSyncedAt] = React.useState("");
  const hideHostRef = useCloseOnHidden(() => setConfirmOpen(false));

  const productAccounts = products.find((item) => item.productName === productName)?.accounts ?? [];
  const selectedAccount = productAccounts.find((item) => item.id === accountId) ?? null;
  const visibleInstruments = accountId == null ? [] : instruments;
  const visibleOrders = accountId == null ? [] : orders;
  const visibleInstrumentId = accountId == null ? null : instrumentId;
  const selectedInstrument = visibleInstruments.find((item) => item.id === visibleInstrumentId) ?? null;

  React.useEffect(() => {
    if (productName && products.some((item) => item.productName === productName)) {
      return;
    }
    const first = products[0];
    setProductName(first?.productName ?? "");
    setAccountId(first?.accounts[0]?.id ?? null);
  }, [productName, products]);

  React.useEffect(() => {
    if (accountId == null) {
      return;
    }
    const controller = new AbortController();
    void loadInstrumentCatalog(accountId, contractType)
      .then((catalog) => {
        if (controller.signal.aborted) return;
        if (!catalog.capabilities.products.includes(contractType)) {
          setCapabilities(catalog.capabilities);
          setInstruments([]);
          setInstrumentId(null);
          setContractType(catalog.capabilities.products[0] ?? "perpetual");
          return;
        }
        const items = catalog.items;
        setInstruments(items);
        setCapabilities(catalog.capabilities);
        setInstrumentId((current) =>
          current && items.some((item) => item.id === current) ? current : items[0]?.id ?? null,
        );
      })
      .catch((error: unknown) => {
        if (!controller.signal.aborted) {
          setInstruments([]);
          setCapabilities(null);
          setMessageKind("error");
          setMessage(error instanceof Error ? error.message : "标的加载失败");
        }
      });
    return () => controller.abort();
  }, [accountId, contractType]);

  const refreshOrders = React.useCallback(async (
    targetAccountId: number,
    view: OrderView,
    signal?: AbortSignal,
    cursor = "",
  ) => {
    setOrderSyncState("syncing");
    const page = await fetchTraderOrderPage(targetAccountId, {
      view, limit: 50, cursor, signal,
    });
    setOrders((current) => cursor ? [...current, ...page.items] : mergeOrderRows(current, page.items));
    setNextCursor(page.nextCursor);
    const delayed = page.items.some((item) => item.syncState === "delayed");
    setOrderSyncState(delayed ? "delayed" : "synced");
    const latest = page.items
      .map((item) => item.lastReconciledAt)
      .filter(Boolean)
      .sort()
      .at(-1);
    setLastSyncedAt(latest ?? "");
    return page.items.length;
  }, []);

  React.useEffect(() => {
    if (accountId == null) {
      return;
    }
    let cancelled = false;
    let timer = 0;
    let inFlight: AbortController | null = null;
    async function tick(scheduleNext = true) {
      if (document.visibilityState === "hidden" || cancelled || accountId == null) return;
      window.clearTimeout(timer);
      inFlight?.abort();
      const request = new AbortController();
      inFlight = request;
      let count = 0;
      try {
        count = await refreshOrders(accountId, orderView, request.signal);
      } catch (error) {
        if (!isAbortError(error) && !request.signal.aborted) {
          setOrderSyncState("delayed");
        }
      } finally {
        if (inFlight === request) inFlight = null;
      }
      if (
        !cancelled &&
        scheduleNext &&
        orderView === "open" &&
        !request.signal.aborted
      ) {
        const base = count > 0 ? 4_000 : 12_000;
        const jitter = Math.floor(Math.random() * 1_000);
        timer = window.setTimeout(() => void tick(), base + jitter);
      }
    }
    void tick();
    function onVisibility() {
      if (document.visibilityState === "visible") {
        window.clearTimeout(timer);
        void tick();
      }
    }
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      cancelled = true;
      inFlight?.abort();
      window.clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [accountId, orderView, refreshOrders]);

  const estimated = React.useMemo(() => {
    const qty = Number(quantity);
    const px = Number(price);
    if (!Number.isFinite(qty) || qty <= 0) return "—";
    if (orderType === "market") return "市价估算，非成交承诺";
    if (!Number.isFinite(px) || px <= 0) return "—";
    return `${(qty * px).toFixed(4)} ${selectedInstrument?.quoteAsset || "USDT"}`;
  }, [orderType, price, quantity, selectedInstrument]);

  function validateForm(): string | null {
    if (!selectedAccount || !selectedInstrument) return "请选择账户和标的";
    if (!selectedAccount.tradingReady) {
      if (selectedAccount.tradingStatus === "checking" || selectedAccount.tradingStatus === "") {
        return "交易能力检查中";
      }
      return selectedAccount.tradingUnavailableReason || "当前账户不可交易";
    }
    if (!positiveDecimal(quantity)) return "请输入有效数量";
    if (orderType === "limit" && !positiveDecimal(price)) return "限价单需要有效价格";
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

  async function confirmOrder() {
    if (!selectedAccount || !selectedInstrument || busy) return;
    setBusy(true);
    try {
      if (isWalletDexExchange(selectedAccount.exchangeSlug)) {
        const ready = await inspectOne(selectedAccount.id);
        if (!ready.tradingReady) {
          throw new Error(ready.tradingUnavailableReason || "当前账户不可交易");
        }
      }
      const order = await placeTraderOrder({
        tradingAccountId: selectedAccount.id,
        instrumentId: selectedInstrument.id,
        side,
        orderType,
        quantity: quantity.trim(),
        price: orderType === "limit" ? price.trim() : undefined,
      });
      setMessageKind("success");
      setMessage(`订单 ${order.id} 已提交，状态 ${orderStatusLabel(order.status)}${order.venueOrderId ? `，交易所 ${order.venueOrderId}` : ""}`);
      setConfirmOpen(false);
      const view: OrderView = terminalOrderStatus(order.status) ? "history" : "open";
      setOrderView(view);
      await refreshOrders(selectedAccount.id, view);
    } catch (error) {
      setConfirmOpen(false);
      setMessageKind("error");
      setMessage(error instanceof Error ? error.message : "下单失败");
    } finally {
      setBusy(false);
    }
  }

  async function onCancel(orderId: string) {
    if (busy || accountId == null) return;
    setBusy(true);
    try {
      await cancelTraderOrder(orderId);
      await refreshOrders(accountId, "open");
    } catch (error) {
      setMessageKind("error");
      setMessage(error instanceof Error ? error.message : "撤单失败");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div ref={hideHostRef} className="flex min-h-0 flex-col">
      <div className="shrink-0 border-b px-5 py-4">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div>
            <div className="text-sm font-semibold">手动交易</div>
            <p className="mt-1 text-xs text-muted-foreground">
              先选产品再选账户，交易所由账户决定。确认订单参数后提交。
            </p>
          </div>
          <div className="inline-flex items-center gap-1.5 rounded-full border bg-muted/40 px-2.5 py-1 text-[11px] text-muted-foreground">
            <ShieldCheck className="size-3" />
            下单前需二次确认
          </div>
        </div>
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto p-4 sm:p-6" data-manual-trading-scroll>
        <form className="mx-auto grid max-w-5xl gap-4" onSubmit={openConfirm}>
          <section className="rounded-xl border bg-background/40 p-4">
            <div className="mb-4 flex items-center gap-2">
              <span className="flex size-7 items-center justify-center rounded-lg bg-primary/10 text-primary">
                <BookOpen className="size-3.5" />
              </span>
              <div>
                <div className="text-sm font-medium">交易信息</div>
                <div className="text-[11px] text-muted-foreground">
                  选择产品、执行账户与标的
                </div>
              </div>
            </div>
            {accountsError ? (
              <div className="text-sm text-rose-600">{accountsError}</div>
            ) : (
              <>
              <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
                <Field label="产品">
                  <select
                    className={selectClassName}
                    value={productName}
                    onChange={(event) => {
                      const next = event.target.value;
                      setProductName(next);
                      const nextAccount = products.find((item) => item.productName === next)?.accounts[0];
                      setAccountId(nextAccount?.id ?? null);
                    }}
                  >
                    {products.length === 0 ? <option value="">暂无可用产品</option> : null}
                    {products.map((item) => (
                      <option key={item.productName} value={item.productName}>
                        {item.productName}
                      </option>
                    ))}
                  </select>
                </Field>
                <Field label="交易账户">
                  <select
                    className={selectClassName}
                    value={accountId ?? ""}
                    onChange={(event) => setAccountId(Number(event.target.value) || null)}
                  >
                    {productAccounts.length === 0 ? <option value="">暂无账户</option> : null}
                    {productAccounts.map((item) => (
                      <option key={item.id} value={item.id}>
                        {tradingAccountStatusLabel(item)}
                      </option>
                    ))}
                  </select>
                </Field>
                <Field label="交易所">
                  <Input value={selectedAccount?.exchange ?? "—"} readOnly className="h-9" />
                </Field>
                <Field label="市场类型">
                  <select
                    className={selectClassName}
                    value={contractType}
                    onChange={(event) => setContractType(event.target.value as TraderContractType)}
                  >
                    {(capabilities?.products.length ? capabilities.products : ["perpetual"]).map((product) => (
                      <option key={product} value={product}>
                        {product === "perpetual" ? "线性永续" : "现货"}
                      </option>
                    ))}
                  </select>
                </Field>
                <Field label="交易标的">
                  <SearchableSelect
                    key={`${accountId ?? "none"}-${contractType}`}
                    aria-label="交易标的"
                    value={visibleInstrumentId == null ? "" : String(visibleInstrumentId)}
                    onValueChange={(next) => setInstrumentId(Number(next) || null)}
                    options={instrumentOptions(visibleInstruments)}
                    placeholder="搜索标的，如 BTC 或 BTCUSDT"
                    emptyText={visibleInstruments.length === 0 ? "交易标的尚未同步" : "无匹配标的"}
                    disabled={visibleInstruments.length === 0}
                  />
                </Field>
              </div>
              {selectedAccount && !selectedAccount.tradingReady ? (
                <p className="mt-3 text-sm text-amber-700">
                  {selectedAccount.tradingStatus === "checking" || selectedAccount.tradingStatus === ""
                    ? "交易能力检查中"
                    : selectedAccount.tradingUnavailableReason || "当前账户不可交易"}
                </p>
              ) : null}
              {accountId != null && visibleInstruments.length === 0 ? (
                <p className="mt-3 text-sm text-muted-foreground">交易标的尚未同步</p>
              ) : null}
              </>
            )}
          </section>

          <section className="rounded-xl border bg-background/40 p-4">
            <div className="mb-4">
              <div className="text-sm font-medium">订单参数</div>
              <div className="mt-0.5 text-[11px] text-muted-foreground">
                设置买卖方向、订单类型、价格和数量
              </div>
            </div>
            <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_minmax(240px,0.7fr)]">
              <div className="grid gap-4">
                <Field label="方向">
                  <div className="grid grid-cols-2 gap-2">
                    <Button
                      type="button"
                      variant={side === "buy" ? "default" : "outline"}
                      className={cn(side === "buy" && "bg-emerald-600 text-white hover:bg-emerald-600/90")}
                      onClick={() => setSide("buy")}
                    >
                      买入 / 做多
                    </Button>
                    <Button
                      type="button"
                      variant={side === "sell" ? "default" : "outline"}
                      className={cn(side === "sell" && "bg-rose-600 text-white hover:bg-rose-600/90")}
                      onClick={() => setSide("sell")}
                    >
                      卖出 / 做空
                    </Button>
                  </div>
                </Field>
                <Field label="订单类型">
                  <div className="grid grid-cols-2 rounded-lg border bg-muted/30 p-1">
                    {(["limit", "market"] as const).map((type) => (
                      <button
                        key={type}
                        type="button"
                        onClick={() => {
                          setOrderType(type);
                          setFormError(null);
                        }}
                        className={cn(
                          "rounded-md px-3 py-1.5 text-sm transition-colors",
                          orderType === type
                            ? "bg-background font-medium text-foreground shadow-sm"
                            : "text-muted-foreground hover:text-foreground",
                        )}
                      >
                        {type === "limit" ? "限价单" : "市价单"}
                      </button>
                    ))}
                  </div>
                </Field>
                <div className="grid gap-4 sm:grid-cols-2">
                  <Field label="价格">
                    {orderType === "market" ? (
                      <div
                        data-market-price
                        className="flex h-9 items-center rounded-lg border bg-muted/40 px-3 text-sm text-muted-foreground"
                      >
                        市价 / 按市场最优价
                      </div>
                    ) : (
                      <Input
                        inputMode="decimal"
                        value={price}
                        onChange={(event) => {
                          setPrice(event.target.value);
                          setFormError(null);
                        }}
                        placeholder="输入委托价格"
                        className="h-9 font-mono"
                      />
                    )}
                  </Field>
                  <Field label="数量">
                    <div className="relative">
                      <Input
                        inputMode="decimal"
                        value={quantity}
                        onChange={(event) => {
                          setQuantity(event.target.value);
                          setFormError(null);
                        }}
                        placeholder="输入委托数量"
                        className="h-9 pr-14 font-mono"
                      />
                      <span className="pointer-events-none absolute inset-y-0 right-3 flex items-center text-xs text-muted-foreground">
                        {selectedInstrument?.baseAsset || "—"}
                      </span>
                    </div>
                  </Field>
                </div>
                {formError ? <p className="text-sm text-rose-600">{formError}</p> : null}
              </div>
              <div className="flex flex-col rounded-xl border bg-muted/20 p-4">
                <div className="text-xs font-medium text-muted-foreground">订单预览</div>
                <dl className="mt-4 grid gap-3 text-sm">
                  <SummaryRow label="产品" value={productName || "—"} />
                  <SummaryRow label="账户" value={selectedAccount?.accountName || "—"} />
                  <SummaryRow label="交易所" value={selectedAccount?.exchange || "—"} />
                  <SummaryRow
                    label="标的"
                    value={selectedInstrument ? `${selectedInstrument.baseAsset} / ${selectedInstrument.quoteAsset}` : "—"}
                  />
                  <SummaryRow
                    label="方向"
                    value={side === "buy" ? "买入 / 做多" : "卖出 / 做空"}
                    valueClassName={side === "buy" ? "text-emerald-600" : "text-rose-600"}
                  />
                  <SummaryRow label="类型" value={orderType === "limit" ? "限价单" : "市价单"} />
                  <SummaryRow label="价格" value={orderType === "limit" ? (price || "—") : "市价"} />
                  <SummaryRow label="数量" value={quantity || "—"} />
                  <SummaryRow label="预计金额" value={estimated} />
                </dl>
                <div className="mt-auto pt-6">
                  <Button type="submit" className="w-full" disabled={!selectedAccount?.tradingReady || !selectedInstrument || busy}>
                    提交订单
                  </Button>
                  {message ? (
                    <p className={cn("mt-2 text-center text-[11px]", messageKind === "error" ? "text-rose-600" : "text-muted-foreground")}>
                      {message}
                    </p>
                  ) : (
                    <p className="mt-2 text-center text-[11px] text-muted-foreground">
                      第一次点击只打开确认，不会向交易所下单
                    </p>
                  )}
                </div>
              </div>
            </div>
          </section>

          <OrderTable
            view={orderView}
            onViewChange={(view) => {
              setOrderView(view);
              setOrders([]);
              setNextCursor("");
            }}
            orders={visibleOrders}
            busy={busy}
            onCancel={onCancel}
            syncState={orderSyncState}
            lastSyncedAt={lastSyncedAt}
            hasMore={nextCursor !== ""}
            onLoadMore={() => {
              if (accountId != null && nextCursor) {
                void refreshOrders(accountId, orderView, undefined, nextCursor);
              }
            }}
          />
        </form>
      </div>

      <Sheet open={confirmOpen} onOpenChange={setConfirmOpen}>
        <SheetContent container={hideHostRef}>
          <SheetHeader>
            <SheetTitle>确认下单</SheetTitle>
            <SheetDescription>
              请核对以下参数。确认后才会向交易所提交，且本次确认只生成一次幂等键。
            </SheetDescription>
          </SheetHeader>
          <div className="grid gap-2 px-4 text-sm">
            <SummaryRow label="产品" value={productName || "—"} />
            <SummaryRow label="账户" value={selectedAccount?.accountName || "—"} />
            <SummaryRow label="交易所" value={selectedAccount?.exchange || "—"} />
            <SummaryRow label="标的" value={selectedInstrument?.exchangeSymbol || "—"} />
            <SummaryRow label="方向" value={side === "buy" ? "买入 / 做多" : "卖出 / 做空"} />
            <SummaryRow label="类型" value={orderType === "limit" ? "限价单" : "市价单"} />
            <SummaryRow label="价格" value={orderType === "limit" ? price : "市价"} />
            <SummaryRow label="数量" value={quantity} />
          </div>
          <SheetFooter>
            <Button type="button" variant="outline" onClick={() => setConfirmOpen(false)} disabled={busy}>
              返回修改
            </Button>
            <Button type="button" onClick={() => void confirmOrder()} disabled={busy}>
              {busy ? "提交中…" : "确认下单"}
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>
    </div>
  );
}

function OrderTable({
  view,
  onViewChange,
  orders,
  busy,
  onCancel,
  syncState,
  lastSyncedAt,
  hasMore,
  onLoadMore,
}: {
  view: OrderView;
  onViewChange: (view: OrderView) => void;
  orders: TraderOrder[];
  busy: boolean;
  onCancel: (orderId: string) => void;
  syncState: "syncing" | "synced" | "delayed";
  lastSyncedAt: string;
  hasMore: boolean;
  onLoadMore: () => void;
}) {
  const showCancel = view === "open";
  return (
    <section className="rounded-xl border bg-background/40 p-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="grid grid-cols-2 rounded-lg border bg-muted/30 p-1">
          {(["open", "history"] as const).map((item) => (
            <button
              key={item}
              type="button"
              onClick={() => onViewChange(item)}
              className={cn(
                "rounded-md px-4 py-1.5 text-sm transition-colors",
                view === item ? "bg-background font-medium shadow-sm" : "text-muted-foreground",
              )}
            >
              {item === "open" ? "当前委托" : "订单历史"}
            </button>
          ))}
        </div>
        <div className={cn(
          "text-[11px]",
          syncState === "delayed" ? "text-amber-600" : "text-muted-foreground",
        )}>
          {syncState === "syncing" ? "同步中…" : syncState === "delayed" ? "同步延迟" : "已同步"}
          {lastSyncedAt ? ` · ${lastSyncedAt.slice(0, 19)}` : ""}
        </div>
      </div>
      <WideTableScroll className="mt-3">
        <table className="w-full min-w-[640px] text-left text-xs">
          <thead className="text-muted-foreground">
            <tr>
              <th className="pb-2 font-medium">时间</th>
              <th className="pb-2 font-medium">标的</th>
              <th className="pb-2 font-medium">方向</th>
              <th className="pb-2 font-medium">委托价</th>
              <th className="pb-2 font-medium">数量</th>
              <th className="pb-2 font-medium">已成交</th>
              <th className="pb-2 font-medium">状态</th>
              <th className="pb-2 font-medium">交易所单号</th>
              {showCancel ? <th className="pb-2 font-medium"></th> : null}
            </tr>
          </thead>
          <tbody>
            {orders.length === 0 ? (
              <tr>
                <td colSpan={showCancel ? 9 : 8} className="py-8 text-center text-muted-foreground">
                  {view === "open" ? "暂无当前委托" : "暂无历史订单"}
                </td>
              </tr>
            ) : (
              orders.map((order) => (
                <tr key={order.id} className="border-t">
                  <td className="py-2">{order.createdAt.slice(0, 19)}</td>
                  <td className="py-2">{order.exchangeSymbol}</td>
                  <td className="py-2">{order.side === "buy" ? "买" : "卖"} / {order.orderType === "limit" ? "限价" : "市价"}</td>
                  <td className="py-2 font-mono">{order.orderType === "limit" ? (order.price || "—") : "市价"}</td>
                  <td className="py-2 font-mono">{formatDecimal(order.quantity)} {order.baseAsset}</td>
                  <td className="py-2 font-mono">{formatDecimal(order.filledQuantity || "0")}</td>
                  <td className="py-2">{orderStatusLabel(order.status)}</td>
                  <td className="py-2 font-mono">{order.venueOrderId || "—"}</td>
                  {showCancel ? (
                    <td className="py-2 text-right">
                      {order.orderType === "limit" ? (
                        <Button type="button" size="xs" variant="outline" disabled={busy} onClick={() => onCancel(order.id)}>
                          撤单
                        </Button>
                      ) : null}
                    </td>
                  ) : null}
                </tr>
              ))
            )}
          </tbody>
        </table>
      </WideTableScroll>
      {hasMore ? (
        <div className="mt-3 text-center">
          <Button type="button" size="sm" variant="outline" disabled={busy} onClick={onLoadMore}>
            加载更多
          </Button>
        </div>
      ) : null}
    </section>
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
    <div className="grid gap-1.5 text-sm">
      <span className="text-xs text-muted-foreground">{label}</span>
      {children}
    </div>
  );
}

function SummaryRow({
  label,
  value,
  valueClassName,
}: {
  label: string;
  value: string;
  valueClassName?: string;
}) {
  return (
    <div className="flex items-center justify-between gap-3 border-b pb-2 last:border-0">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className={cn("font-medium", valueClassName)}>{value}</dd>
    </div>
  );
}
