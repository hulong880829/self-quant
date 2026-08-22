"use client";

import * as React from "react";
import { ChevronDown, Clock3, Info, Timer, X } from "lucide-react";

import { useAuth } from "@/components/auth/auth-provider";
import { PolymarketPriceChart } from "@/components/polymarket/price-chart";
import { MarketCountdown } from "@/components/polymarket/market-countdown";
import { usePolymarket } from "@/components/polymarket/polymarket-provider";
import { PageFrame, WideTableScroll, WorkspacePanel } from "@/components/layout/responsive";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { cancelPolymarketOrder, placePolymarketOrder } from "@/lib/api/polymarket";
import {
  getAssetSymbol,
  polymarketAssets,
  polymarketPeriods,
} from "@/lib/polymarket";
import { countLiveMarkets, resolveMarketInfo } from "@/lib/polymarket-market";
import { formatCurrency } from "@/lib/market-format";
import type {
  PolymarketAssetId,
  PolymarketMarket,
  PolymarketOpenOrder,
  PolymarketPeriodId,
  PolymarketPosition,
} from "@/types/polymarket";
import { cn } from "@/lib/utils";

function formatPrice(value: number | null) {
  return value == null ? "--" : formatCurrency(value);
}

function quoteCents(value: number | null) {
  return value == null ? "--" : `${Math.round(value * 100)}¢`;
}

function orderErrorMessage(code: string, message: string) {
  if (code === "insufficient_position") return "可卖持仓不足";
  if (code === "insufficient_balance") return "现金余额不足";
  return message || code || "上游拒绝";
}

interface MarketDetails {
  title: string;
  market: PolymarketMarket | null;
  conditionId: string;
  tokenId: string;
  outcome: string;
  source: "挂单" | "持仓";
}

function MarketInfoSheet({
  details,
  onClose,
}: {
  details: MarketDetails | null;
  onClose: () => void;
}) {
  const fields = details
    ? [
        ["来源", details.source],
        ["资产", details.market?.asset ?? "--"],
        ["周期", details.market?.period ?? "--"],
        [
          "开始时间",
          details.market
            ? new Date(details.market.windowStart).toLocaleString("zh-CN")
            : "--",
        ],
        [
          "结束时间",
          details.market
            ? new Date(details.market.windowEnd).toLocaleString("zh-CN")
            : "--",
        ],
        ["结果", details.outcome || "--"],
        ["Slug", details.market?.slug ?? "--"],
      ]
    : [];
  return (
    <Sheet open={details != null} onOpenChange={(open) => !open && onClose()}>
      <SheetContent className="sm:max-w-md">
        <SheetHeader className="border-b pr-12">
          <SheetTitle className="leading-6">{details?.title ?? "Market 信息"}</SheetTitle>
          <SheetDescription>完整市场信息</SheetDescription>
        </SheetHeader>
        {details ? (
          <div className="space-y-5 overflow-y-auto px-4 pb-6">
            <dl className="grid grid-cols-[88px_minmax(0,1fr)] gap-x-3 gap-y-3 text-sm">
              {fields.map(([label, value]) => (
                <React.Fragment key={label}>
                  <dt className="text-muted-foreground">{label}</dt>
                  <dd className="min-w-0 font-medium">{value}</dd>
                </React.Fragment>
              ))}
            </dl>
            {[
              ["Condition ID", details.conditionId],
              ["Token ID", details.tokenId],
            ].map(([label, value]) => (
              <div key={label}>
                <div className="text-xs font-medium text-muted-foreground">{label}</div>
                <div className="mt-1 select-all break-all rounded-md border bg-muted/30 p-2 font-mono text-xs">
                  {value || "--"}
                </div>
              </div>
            ))}
          </div>
        ) : null}
      </SheetContent>
    </Sheet>
  );
}

function AssetDropdown({
  selectedId,
  onSelect,
}: {
  selectedId: PolymarketAssetId;
  onSelect: (id: PolymarketAssetId) => void;
}) {
  const selected = polymarketAssets.find((asset) => asset.id === selectedId);
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={<Button variant="outline" className="h-10 w-full justify-between" />}
      >
        <span className="flex items-center gap-2">
          <span className="flex size-6 items-center justify-center rounded-full bg-muted text-[10px] font-semibold">
            {selected?.symbol ?? "?"}
          </span>
          <span>{selectedId}</span>
        </span>
        <ChevronDown className="size-4 text-muted-foreground" />
      </DropdownMenuTrigger>
      <DropdownMenuContent className="w-(--anchor-width) min-w-[208px]">
        {polymarketAssets.map((asset) => (
          <DropdownMenuItem
            key={asset.id}
            onClick={() => onSelect(asset.id)}
            className={cn(asset.id === selectedId && "bg-primary/[0.08]")}
          >
            <span className="flex size-6 items-center justify-center rounded-full bg-muted text-[10px]">
              {asset.symbol}
            </span>
            {asset.id}
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function PositionsPanel({
  positions,
  markets,
  anonymous,
  noAccount,
  onLogin,
  onOpenMarket,
}: {
  positions: PolymarketPosition[];
  markets: PolymarketMarket[];
  anonymous: boolean;
  noAccount: boolean;
  onLogin: () => void;
  onOpenMarket: (details: MarketDetails) => void;
}) {
  let empty: React.ReactNode = "暂无持仓。";
  if (anonymous) {
    empty = (
      <button type="button" className="text-primary hover:underline" onClick={onLogin}>
        登录后查看持仓
      </button>
    );
  } else if (noAccount) {
    empty = "请先到账户管理绑定 Polymarket 账户。";
  }
  return (
    <div className="overflow-hidden rounded-lg border">
      <WideTableScroll className="max-h-[min(26rem,calc(var(--app-panel-height)-6rem))]">
        <div className="min-w-[760px]">
          <div className="sticky top-0 z-10 grid grid-cols-[1.8fr_1.1fr_.8fr_.9fr_.7fr] gap-2 border-b bg-muted/35 px-3 py-2.5 text-[11px] text-muted-foreground">
            <span>盘口</span>
            <span className="inline-flex items-center gap-1">
              均价 → 当前价 <Info className="size-3" />
            </span>
            <span className="text-right">交易金额</span>
            <span className="text-right">可赢利金额</span>
            <span className="text-right">价值</span>
          </div>
          {positions.length === 0 ? (
            <div className="px-3 py-10 text-center text-sm text-muted-foreground">
              {empty}
            </div>
          ) : (
            positions.map((position) => {
            const marketInfo = resolveMarketInfo(markets, {
              conditionId: position.conditionId,
              tokenId: position.tokenId,
              fallbackTitle: position.market,
            });
            return (
              <div
                key={`${position.tokenId}-${position.outcome}`}
                className="grid grid-cols-[1.8fr_1.1fr_.8fr_.9fr_.7fr] gap-2 border-b px-3 py-3 text-xs last:border-b-0"
              >
              <button
                type="button"
                className="min-w-0 truncate text-left font-medium underline-offset-4 hover:text-primary hover:underline"
                onClick={() =>
                  onOpenMarket({
                    ...marketInfo,
                    conditionId: position.conditionId,
                    tokenId: position.tokenId,
                    outcome: position.outcome,
                    source: "持仓",
                  })
                }
              >
                {marketInfo.title} · {position.outcome}
              </button>
              <span className="font-mono text-muted-foreground">
                {quoteCents(position.averagePrice)} → {quoteCents(position.currentPrice)}
              </span>
              <span className="text-right font-mono">
                {formatCurrency(position.initialValue)}
              </span>
              <span
                className={cn(
                  "text-right font-mono",
                  position.cashPnl >= 0 ? "text-positive" : "text-negative",
                )}
              >
                {position.cashPnl >= 0 ? "+" : ""}
                {formatCurrency(position.cashPnl)}
              </span>
              <span className="text-right font-mono">
                {formatCurrency(position.currentValue)}
              </span>
            </div>
            );
          })
        )}
        </div>
      </WideTableScroll>
    </div>
  );
}

function OpenOrdersPanel({
  openOrders,
  markets,
  anonymous,
  noAccount,
  onLogin,
  cancelingOrderId,
  onCancel,
  onOpenMarket,
}: {
  openOrders: PolymarketOpenOrder[];
  markets: PolymarketMarket[];
  anonymous: boolean;
  noAccount: boolean;
  onLogin: () => void;
  cancelingOrderId: string | null;
  onCancel: (order: PolymarketOpenOrder) => void;
  onOpenMarket: (details: MarketDetails) => void;
}) {
  let empty: React.ReactNode = "暂无挂单。";
  if (anonymous) {
    empty = (
      <button type="button" className="text-primary hover:underline" onClick={onLogin}>
        登录后查看挂单
      </button>
    );
  } else if (noAccount) {
    empty = "请先到账户管理绑定 Polymarket 账户。";
  }
  return (
    <div className="overflow-hidden rounded-lg border">
      <WideTableScroll className="max-h-[min(26rem,calc(var(--app-panel-height)-6rem))]">
        <div className="min-w-[940px]">
          <div className="sticky top-0 z-10 grid grid-cols-[2.2fr_.9fr_.7fr_.9fr_.8fr_1fr_.5fr] gap-2 border-b bg-muted/35 px-3 py-2.5 text-[11px] text-muted-foreground">
            <span>Market</span>
            <span>方向</span>
            <span className="text-right">价格</span>
            <span className="text-right">成交 / 总量</span>
            <span>状态</span>
            <span>时间</span>
            <span className="text-center">撤单</span>
          </div>
          {openOrders.length === 0 ? (
            <div className="px-3 py-10 text-center text-sm text-muted-foreground">
              {empty}
            </div>
          ) : (
            openOrders.map((order) => {
            const marketInfo = resolveMarketInfo(markets, {
              conditionId: order.conditionId,
              tokenId: order.tokenId,
              fallbackTitle: order.marketTitle,
            });
            return (
              <div
                key={order.id}
                className="grid grid-cols-[2.2fr_.9fr_.7fr_.9fr_.8fr_1fr_.5fr] items-center gap-2 border-b px-3 py-3 text-xs last:border-b-0"
              >
              <button
                type="button"
                className="min-w-0 truncate text-left font-medium underline-offset-4 hover:text-primary hover:underline"
                onClick={() =>
                  onOpenMarket({
                    ...marketInfo,
                    conditionId: order.conditionId,
                    tokenId: order.tokenId,
                    outcome: order.outcome,
                    source: "挂单",
                  })
                }
              >
                {marketInfo.title}
              </button>
              <span className="capitalize">
                {order.side} · {order.outcome}
              </span>
              <span className="text-right font-mono">{quoteCents(order.price)}</span>
              <span className="text-right font-mono">
                {order.matchedSize} / {order.originalSize}
              </span>
              <span>{order.status || "--"}</span>
              <span className="text-muted-foreground">
                {order.createdAt
                  ? new Date(order.createdAt).toLocaleString("zh-CN")
                  : "--"}
              </span>
              <Button
                type="button"
                variant="ghost"
                size="icon-sm"
                className="mx-auto text-muted-foreground hover:text-destructive"
                disabled={cancelingOrderId === order.id}
                aria-label={`撤销 ${order.marketTitle || order.id} 挂单`}
                onClick={() => onCancel(order)}
              >
                <X className="size-4" />
              </Button>
            </div>
            );
          })
        )}
        </div>
      </WideTableScroll>
    </div>
  );
}

export function PolymarketDashboard() {
  const auth = useAuth();
  const {
    markets,
    selectedMarket,
    snapshot,
    chartPoints,
    currentFairPrice,
    fairPricePoints,
    fairPriceSource,
    fairPriceStatus,
    fairPriceError,
    fairPriceHistoryError,
    selectedAsset,
    selectedPeriod,
    selectAsset,
    selectPeriod,
    loading,
    snapshotLoading,
    error,
    accounts,
    selectedAccountId,
    selectAccount,
    summary,
    openOrders,
    openOrdersStale,
    positions,
    positionsStale,
    summaryError,
    openOrdersError,
    positionsError,
    refreshPrivate,
    removeOpenOrder,
    refreshMarkets,
  } = usePolymarket();
  const [portfolioTab, setPortfolioTab] = React.useState<"orders" | "positions">(
    "orders",
  );
  const [side, setSide] = React.useState<"buy" | "sell">("buy");
  const [direction, setDirection] = React.useState<"up" | "down">("up");
  const [executionType, setExecutionType] = React.useState<"book" | "limit">("book");
  const [amount, setAmount] = React.useState("1");
  const [limitPrice, setLimitPrice] = React.useState("");
  const [tradeBusy, setTradeBusy] = React.useState(false);
  const [cancelingOrderId, setCancelingOrderId] = React.useState<string | null>(null);
  const [marketDetails, setMarketDetails] = React.useState<MarketDetails | null>(null);
  const [tradeMessage, setTradeMessage] = React.useState<string | null>(null);
  const [tradeMessageKind, setTradeMessageKind] = React.useState<
    "success" | "error" | "info"
  >("info");

  React.useEffect(() => {
    if (tradeMessage == null) return;
    const timer = setTimeout(() => setTradeMessage(null), 1_000);
    return () => clearTimeout(timer);
  }, [tradeMessage]);

  const openPrice = snapshot?.openPrice ?? null;
  const chainlinkPrice = snapshot?.chainlinkPrice ?? null;
  const fairPrice = currentFairPrice?.price ?? null;
  const fairPriceTitle = [
    fairPriceSource
      ? `${fairPriceSource.profile} / ${fairPriceSource.symbol}`
      : "Fair Price 市场未解析",
    currentFairPrice?.modelId ? `Model: ${currentFairPrice.modelId}` : "",
    currentFairPrice?.timestamp
      ? `更新时间: ${new Date(currentFairPrice.timestamp).toLocaleString("zh-CN")}`
      : "",
    currentFairPrice?.degradedReasons.length
      ? `降级原因: ${currentFairPrice.degradedReasons.join(", ")}`
      : "",
    fairPriceError ?? "",
  ]
    .filter(Boolean)
    .join("\n");
  const delta =
    openPrice != null && chainlinkPrice != null ? chainlinkPrice - openPrice : null;
  const upTradeQuote =
    side === "buy" ? (snapshot?.upAsk ?? null) : (snapshot?.upBid ?? null);
  const downTradeQuote =
    side === "buy" ? (snapshot?.downAsk ?? null) : (snapshot?.downBid ?? null);
  const selectedTradeQuote = direction === "up" ? upTradeQuote : downTradeQuote;
  const estimatedLimitTotal =
    executionType === "limit" &&
    Number.isFinite(Number(limitPrice)) &&
    Number.isFinite(Number(amount))
      ? Number(limitPrice) * Number(amount)
      : null;
  const marketReady =
    snapshot != null &&
    selectedMarket != null &&
    snapshot.market.id === selectedMarket.id;
  const selectedMarketCount = React.useCallback(
    (period: PolymarketPeriodId) =>
      countLiveMarkets(markets, selectedAsset, period),
    [markets, selectedAsset],
  );

  const hasSelectedMarket = selectedMarketCount(selectedPeriod) > 0;

  async function submitOrder() {
    setTradeMessage(null);
    if (!auth.requireAuth("/crypto-options")) return;
    if (selectedAccountId == null) {
      setTradeMessageKind("info");
      setTradeMessage("请先到账户管理绑定 Polymarket 账户");
      return;
    }
    if (!marketReady || !snapshot) {
      setTradeMessageKind("info");
      setTradeMessage("市场正在切换，请稍候");
      return;
    }
    const numericAmount = Number(amount);
    if (!Number.isFinite(numericAmount) || numericAmount <= 0) {
      setTradeMessageKind("error");
      setTradeMessage("请输入有效交易金额");
      return;
    }
    if (executionType === "limit") {
      const numericPrice = Number(limitPrice);
      const tickSize = Number(snapshot.market.tickSize);
      const tickAligned =
        Number.isFinite(tickSize) &&
        tickSize > 0 &&
        Math.abs(numericPrice / tickSize - Math.round(numericPrice / tickSize)) <
          1e-8;
      if (
        !Number.isFinite(numericPrice) ||
        numericPrice <= 0 ||
        numericPrice >= 1 ||
        !tickAligned
      ) {
        setTradeMessageKind("error");
        setTradeMessage(`请输入符合 ${snapshot.market.tickSize} tick 的有效限价`);
        return;
      }
    }
    setTradeBusy(true);
    try {
      const order = await placePolymarketOrder({
        tradingAccountId: selectedAccountId,
        marketId: snapshot.market.id,
        outcome: direction,
        side,
        amount,
        amountUnit:
          executionType === "limit" || side === "sell" ? "shares" : "usd",
        executionType,
        ...(executionType === "limit" ? { limitPrice } : {}),
      });
      setTradeMessageKind(order.status === "rejected" ? "error" : "success");
      setTradeMessage(
        order.status === "rejected"
          ? `订单被拒绝：${orderErrorMessage(order.errorCode, order.errorMessage)}`
          : `订单已提交：${order.status}`,
      );
      void refreshPrivate();
    } catch (reason) {
      setTradeMessageKind("error");
      setTradeMessage(reason instanceof Error ? reason.message : "订单提交失败");
    } finally {
      setTradeBusy(false);
    }
  }

  async function cancelOrder(order: PolymarketOpenOrder) {
    if (selectedAccountId == null || cancelingOrderId != null) return;
    setTradeMessage(null);
    setCancelingOrderId(order.id);
    try {
      await cancelPolymarketOrder(selectedAccountId, order.id);
      removeOpenOrder(order.id);
      setTradeMessageKind("success");
      setTradeMessage("撤单成功");
    } catch (reason) {
      setTradeMessageKind("error");
      setTradeMessage(reason instanceof Error ? reason.message : "撤单失败");
    } finally {
      setCancelingOrderId(null);
    }
  }

  function strategyTrade() {
    if (!auth.requireAuth("/crypto-options")) return;
    setTradeMessageKind("info");
    setTradeMessage("策略功能待 Fair Price / 规则接入");
  }

  const title =
    snapshot?.market.title ??
    selectedMarket?.title ??
    `${selectedAsset} ${selectedPeriod}`;
  const windowLabel = snapshot
    ? `${new Date(snapshot.market.windowStart).toLocaleString("zh-CN")} - ${new Date(
        snapshot.market.windowEnd,
      ).toLocaleTimeString("zh-CN")}`
    : selectedMarket
      ? `${new Date(selectedMarket.windowStart).toLocaleString("zh-CN")} - ${new Date(
          selectedMarket.windowEnd,
        ).toLocaleTimeString("zh-CN")}`
      : snapshotLoading
        ? "正在加载市场快照…"
        : hasSelectedMarket
          ? "等待 Chainlink/Open 价格…"
          : "当前币种和周期暂无有效市场";
  const privateError =
    summaryError ||
    (portfolioTab === "orders" ? openOrdersError : positionsError);

  return (
    <PageFrame className="gap-0">
      <MarketInfoSheet details={marketDetails} onClose={() => setMarketDetails(null)} />
      <WorkspacePanel className="flex-1 xl:grid-cols-[240px_minmax(0,1fr)]">
        <aside className="hidden min-h-0 overflow-y-auto border-r xl:block">
          <div className="border-b px-4 py-3 text-xs font-medium text-muted-foreground">
            币种
          </div>
          <div className="border-b p-2">
            <AssetDropdown selectedId={selectedAsset} onSelect={selectAsset} />
          </div>
          <div className="border-b px-4 py-3 text-xs font-medium text-muted-foreground">
            周期
          </div>
          <div className="p-2">
            {polymarketPeriods.map((period) => {
              const count = selectedMarketCount(period.id);
              const disabled = count === 0;
              return (
              <button
                key={period.id}
                type="button"
                disabled={disabled}
                onClick={() => !disabled && selectPeriod(period.id)}
                className={cn(
                  "flex w-full items-center justify-between rounded-lg px-3 py-2.5 text-sm",
                  period.id === selectedPeriod
                    ? "bg-primary/[0.08] font-medium"
                    : "text-muted-foreground hover:bg-muted",
                  disabled && "cursor-not-allowed opacity-45 hover:bg-transparent",
                )}
              >
                <span>{period.label}</span>
                <span className="text-xs">{disabled ? "暂无市场" : count}</span>
              </button>
            )})}
          </div>
        </aside>

        <section className="min-w-0 overflow-y-auto">
          <div className="border-b p-3 xl:hidden">
            <AssetDropdown selectedId={selectedAsset} onSelect={selectAsset} />
            <div className="mt-3 flex gap-2 overflow-x-auto">
              {polymarketPeriods.map((period) => {
                const count = selectedMarketCount(period.id);
                const disabled = count === 0;
                return (
                <button
                  key={period.id}
                  type="button"
                  disabled={disabled}
                  onClick={() => !disabled && selectPeriod(period.id)}
                  className={cn(
                    "shrink-0 rounded-full border px-3 py-1.5 text-xs",
                    period.id === selectedPeriod && "border-primary bg-primary/10",
                    disabled && "cursor-not-allowed opacity-45",
                  )}
                >
                  {period.label}
                </button>
              )})}
            </div>
          </div>

          <div className="grid min-w-0 gap-4 p-3 sm:p-4 xl:grid-cols-[minmax(0,1fr)_minmax(16rem,20rem)]">
            <div className="min-w-0 space-y-4">
              {loading ? (
                <div className="rounded-lg border border-dashed p-6 text-sm text-muted-foreground">
                  正在加载 Polymarket 市场列表…
                </div>
              ) : null}
              {error ? (
                <div className="rounded-lg border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive">
                  {error}
                </div>
              ) : null}
              <div className="rounded-xl border bg-card/85 p-3 shadow-sm">
                <div className="flex flex-wrap items-start justify-between gap-3">
                  <div>
                    <div className="flex items-center gap-2.5">
                      <div className="flex size-8 items-center justify-center rounded-full bg-warning/15 text-sm font-semibold text-warning">
                        {getAssetSymbol(selectedAsset)}
                      </div>
                      <div>
                        <h2 className="text-base font-semibold leading-5">{title}</h2>
                        <p className="mt-0.5 text-xs text-muted-foreground">
                          {windowLabel}
                        </p>
                      </div>
                    </div>
                    <div className="mt-3 grid gap-2 sm:grid-cols-3">
                      <div>
                        <div className="text-[10px] uppercase tracking-[0.16em] text-muted-foreground">
                          目标 / Open
                        </div>
                        <div className="mt-0.5 font-mono text-lg font-semibold">
                          {formatPrice(openPrice)}
                        </div>
                      </div>
                      <div>
                        <div className="text-[10px] uppercase tracking-[0.16em] text-muted-foreground">
                          Chainlink
                        </div>
                        <div className="mt-0.5 font-mono text-lg font-semibold text-warning">
                          {formatPrice(chainlinkPrice)}
                        </div>
                      </div>
                      <div title={fairPriceTitle}>
                        <div className="flex items-center gap-1.5 text-[10px] uppercase tracking-[0.16em] text-muted-foreground">
                          <span
                            className={cn(
                              "size-1.5 rounded-full",
                              fairPriceStatus === "live" &&
                                !currentFairPrice?.degraded
                                ? "bg-positive"
                                : fairPriceStatus === "connecting"
                                  ? "animate-pulse bg-muted-foreground"
                                  : fairPriceStatus === "live"
                                    ? "bg-warning"
                                    : "bg-muted-foreground",
                            )}
                          />
                          Fair Price
                        </div>
                        {fairPrice == null && fairPriceStatus === "connecting" ? (
                          <div className="mt-1 h-6 w-28 animate-pulse rounded bg-muted" />
                        ) : (
                          <div
                            className={cn(
                              "mt-0.5 font-mono text-lg font-semibold",
                              fairPriceStatus === "live"
                                ? "text-[var(--chart-3)]"
                                : "text-muted-foreground",
                            )}
                          >
                            {formatPrice(fairPrice)}
                          </div>
                        )}
                        <div className="mt-0.5 truncate text-[10px] text-muted-foreground">
                          {fairPriceStatus === "live"
                            ? currentFairPrice?.degraded
                              ? "Degraded"
                              : fairPriceSource?.symbol
                            : fairPriceStatus === "stale"
                              ? "Stale"
                              : fairPriceStatus === "unavailable"
                                ? "Unavailable"
                                : "Connecting"}
                        </div>
                      </div>
                    </div>
                    <div
                      className={cn(
                        "mt-1.5 text-xs font-medium",
                        delta == null
                          ? "text-muted-foreground"
                          : delta >= 0
                            ? "text-positive"
                            : "text-negative",
                      )}
                    >
                      {delta == null
                        ? openPrice == null && chainlinkPrice != null
                          ? "Open 待同步"
                          : "等待真实价格"
                        : `${delta >= 0 ? "+" : ""}${formatCurrency(delta)} vs Open`}
                    </div>
                    {snapshot?.stale ? (
                      <Badge variant="outline" className="mt-1.5">
                        行情源已过期
                      </Badge>
                    ) : null}
                  </div>
                  <div className="rounded-lg border bg-muted/40 px-3 py-2 text-center">
                    <div className="flex items-center justify-center gap-1 text-[10px] uppercase text-muted-foreground">
                      <Timer className="size-3" /> 剩余
                    </div>
                    <MarketCountdown
                      windowEnd={
                        snapshot?.market.windowEnd ?? selectedMarket?.windowEnd ?? null
                      }
                      onWindowEnd={() => void refreshMarkets()}
                    />
                  </div>
                </div>
              </div>

              <div className="rounded-xl border bg-card/85 p-4 shadow-sm sm:p-5">
                <div className="mb-4 flex items-center gap-2">
                  <Clock3 className="size-4 text-primary" />
                  <div>
                    <h3 className="text-sm font-semibold">价格走势</h3>
                    <p className="text-xs text-muted-foreground">
                      Open 基准线 · Chainlink 实时价 · Fair Price
                    </p>
                  </div>
                  {fairPriceHistoryError ? (
                    <Badge
                      variant="outline"
                      className="ml-auto max-w-56 truncate text-muted-foreground"
                      title={fairPriceHistoryError}
                    >
                      Fair Price 历史暂不可用
                    </Badge>
                  ) : null}
                </div>
                {snapshotLoading && !snapshot ? (
                  <div className="flex h-72 items-center justify-center rounded-lg border border-dashed text-sm text-muted-foreground">
                    正在切换市场…
                  </div>
                ) : (
                  <div className="relative">
                    <PolymarketPriceChart
                      points={chartPoints}
                      fairPricePoints={fairPricePoints}
                      openPrice={openPrice}
                    />
                    {snapshotLoading || !marketReady ? (
                      <div className="pointer-events-none absolute right-2 top-2 rounded-full border bg-card/90 px-2.5 py-1 text-[11px] text-muted-foreground shadow-sm">
                        正在切换市场…
                      </div>
                    ) : null}
                  </div>
                )}
              </div>

              <div className="rounded-xl border bg-card/85 p-4 shadow-sm sm:p-5">
                <div className="mb-3 flex items-center gap-2">
                  {([
                    ["orders", "挂单"],
                    ["positions", "持仓"],
                  ] as const).map(([value, label]) => (
                    <Button
                      key={value}
                      type="button"
                      size="sm"
                      variant={portfolioTab === value ? "default" : "outline"}
                      onClick={() => setPortfolioTab(value)}
                    >
                      {label}
                    </Button>
                  ))}
                  {(portfolioTab === "orders" ? openOrdersStale : positionsStale) ? (
                    <Badge variant="outline">缓存数据</Badge>
                  ) : null}
                </div>
                {portfolioTab === "orders" ? (
                  <OpenOrdersPanel
                    openOrders={openOrders}
                    markets={markets}
                    anonymous={auth.status !== "authenticated"}
                    noAccount={
                      auth.status === "authenticated" && accounts.length === 0
                    }
                    onLogin={() => auth.openLogin("/crypto-options")}
                    cancelingOrderId={cancelingOrderId}
                    onCancel={(order) => void cancelOrder(order)}
                    onOpenMarket={setMarketDetails}
                  />
                ) : (
                  <PositionsPanel
                    positions={positions}
                    markets={markets}
                    anonymous={auth.status !== "authenticated"}
                    noAccount={
                      auth.status === "authenticated" && accounts.length === 0
                    }
                    onLogin={() => auth.openLogin("/crypto-options")}
                    onOpenMarket={setMarketDetails}
                  />
                )}
                {privateError ? (
                  <p className="mt-2 text-xs text-destructive">{privateError}</p>
                ) : null}
              </div>
            </div>

            <div className="h-fit rounded-xl border bg-card/85 p-4 shadow-sm sm:p-5">
              {auth.status === "authenticated" ? (
                accounts.length > 0 ? (
                  <div className="rounded-lg border bg-muted/30 p-3 text-sm">
                    <select
                      className="w-full bg-transparent font-medium outline-none"
                      value={selectedAccountId ?? ""}
                      onChange={(event) => selectAccount(Number(event.target.value))}
                    >
                      {accounts.map((account) => (
                        <option key={account.id} value={account.id}>
                          {account.accountName}
                        </option>
                      ))}
                    </select>
                    <div className="mt-1 text-muted-foreground">
                      资产：{" "}
                      <span className="font-mono text-foreground">
                        {summary
                          ? formatCurrency(summary.totalAssets)
                          : summaryError
                            ? "--"
                            : "--"}
                      </span>
                      {summary?.stale ? " · 缓存" : ""}
                    </div>
                    {summaryError ? (
                      <div className="mt-1 text-xs text-destructive">{summaryError}</div>
                    ) : null}
                  </div>
                ) : (
                  <div className="rounded-lg border bg-muted/30 p-3 text-sm text-muted-foreground">
                    尚未绑定 Polymarket 账户，请到账户管理添加。
                  </div>
                )
              ) : null}

              <div className="mt-4 flex items-start justify-between gap-3">
                <div className="text-sm font-semibold">{title}</div>
                <select
                  aria-label="订单类型"
                  value={executionType}
                  onChange={(event) => {
                    const value = event.target.value as "book" | "limit";
                    setExecutionType(value);
                    setTradeMessage(null);
                    if (value === "limit" && selectedTradeQuote != null) {
                      setLimitPrice(String(selectedTradeQuote));
                    }
                  }}
                  className="h-9 shrink-0 rounded-md border bg-background px-3 text-sm"
                >
                  <option value="book">盘口</option>
                  <option value="limit">限价</option>
                </select>
              </div>
              <div className="mt-4 grid grid-cols-2 gap-2">
                {(["buy", "sell"] as const).map((value) => (
                  <Button
                    key={value}
                    variant={side === value ? "default" : "outline"}
                    onClick={() => setSide(value)}
                  >
                    {value === "buy" ? "买入" : "卖出"}
                  </Button>
                ))}
              </div>
              <div className="mt-3 grid grid-cols-2 gap-2">
                <Button
                  variant={direction === "up" ? "default" : "outline"}
                  className={cn(direction === "up" && "bg-positive text-white")}
                  onClick={() => setDirection("up")}
                >
                  Up {quoteCents(upTradeQuote)}
                </Button>
                <Button
                  variant={direction === "down" ? "default" : "outline"}
                  className={cn(direction === "down" && "bg-negative text-white")}
                  onClick={() => setDirection("down")}
                >
                  Down {quoteCents(downTradeQuote)}
                </Button>
              </div>
              {executionType === "book" ? (
                <div className="mt-3 rounded-md border bg-muted/20 px-3 py-2 text-xs text-muted-foreground">
                  盘口价：{side === "buy" ? "对手最优卖价" : "对手最优买价"}{" "}
                  <span className="font-mono text-foreground">
                    {quoteCents(selectedTradeQuote)}
                  </span>
                  ，未成交部分立即取消
                </div>
              ) : (
                <>
                  <div className="mt-4 text-sm text-muted-foreground">限价</div>
                  <Input
                    value={limitPrice}
                    onChange={(event) => setLimitPrice(event.target.value)}
                    className="mt-2 h-11 text-right font-mono text-xl"
                    inputMode="decimal"
                    placeholder={snapshot?.market.tickSize ?? "0.01"}
                  />
                </>
              )}
              <div className="mt-4 text-sm text-muted-foreground">
                {executionType === "limit"
                  ? "份额（shares）"
                  : side === "buy"
                    ? "金额（USD）"
                    : "数量（shares）"}
              </div>
              <Input
                value={amount}
                onChange={(event) => setAmount(event.target.value)}
                className="mt-2 h-12 text-right font-mono text-2xl"
                inputMode="decimal"
              />
              <div className="mt-3 grid grid-cols-4 gap-2">
                {["1", "5", "10", "100"].map((value) => (
                  <Button
                    key={value}
                    variant="outline"
                    size="sm"
                    onClick={() => setAmount(value)}
                  >
                    {executionType === "book" && side === "buy" ? "$" : ""}
                    {value}
                  </Button>
                ))}
              </div>
              {executionType === "limit" ? (
                <div className="mt-3 flex items-center justify-between text-sm text-muted-foreground">
                  <span>预计总额</span>
                  <span className="font-mono text-foreground">
                    {estimatedLimitTotal != null && estimatedLimitTotal > 0
                      ? formatCurrency(estimatedLimitTotal)
                      : "--"}
                  </span>
                </div>
              ) : null}
              <Button
                className="mt-4 h-11 w-full text-base"
                onClick={submitOrder}
                disabled={tradeBusy}
              >
                {tradeBusy ? "提交中…" : marketReady ? "交易" : "市场切换中"}
              </Button>
              <Button
                variant="secondary"
                className="mt-2 h-11 w-full text-base"
                onClick={strategyTrade}
              >
                策略交易
              </Button>
              {tradeMessage || privateError ? (
                <p
                  className={cn(
                    "mt-2 rounded-md border px-3 py-2 text-center text-sm font-medium",
                    tradeMessage
                      ? tradeMessageKind === "error"
                        ? "border-destructive/30 bg-destructive/10 text-destructive"
                        : tradeMessageKind === "success"
                          ? "border-positive/30 bg-positive/10 text-positive"
                          : "border-primary/20 bg-primary/[0.06] text-foreground"
                      : "border-destructive/30 bg-destructive/10 text-destructive",
                  )}
                >
                  {tradeMessage || privateError}
                </p>
              ) : null}
              <p className="mt-3 text-center text-[11px] text-muted-foreground">
                交易即表示你同意使用条款
              </p>
            </div>
          </div>
        </section>
      </WorkspacePanel>
    </PageFrame>
  );
}
