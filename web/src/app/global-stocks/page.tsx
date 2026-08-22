"use client";

import * as React from "react";
import {
  Clock3,
  Globe2,
  Plus,
  RefreshCw,
  Search,
  Trash2,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { PageFrame, WideTableScroll } from "@/components/layout/responsive";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { cn } from "@/lib/utils";

type MarketCode = "US" | "CN" | "HK" | "KR" | "JP";

interface StockQuote {
  id: string;
  symbol: string;
  name: string;
  exchange: string;
  market: MarketCode;
  currency: string;
  price: number;
  change: number;
  changePercent: number;
  open: number;
  high: number;
  low: number;
  previousClose: number;
  volume: number;
  sector: string;
  status: "交易中" | "已收盘" | "盘前";
}

const marketLabels: Record<MarketCode, string> = {
  US: "美国",
  CN: "中国",
  HK: "香港",
  KR: "韩国",
  JP: "日本",
};

const quoteCatalog: StockQuote[] = [
  {
    id: "US:AAPL",
    symbol: "AAPL",
    name: "Apple",
    exchange: "NASDAQ",
    market: "US",
    currency: "USD",
    price: 227.16,
    change: 2.31,
    changePercent: 1.03,
    open: 225.41,
    high: 228.34,
    low: 224.72,
    previousClose: 224.85,
    volume: 48621300,
    sector: "科技",
    status: "交易中",
  },
  {
    id: "US:NVDA",
    symbol: "NVDA",
    name: "NVIDIA",
    exchange: "NASDAQ",
    market: "US",
    currency: "USD",
    price: 183.21,
    change: -1.47,
    changePercent: -0.8,
    open: 184.95,
    high: 186.08,
    low: 181.62,
    previousClose: 184.68,
    volume: 152834200,
    sector: "半导体",
    status: "交易中",
  },
  {
    id: "CN:600519",
    symbol: "600519",
    name: "贵州茅台",
    exchange: "SSE",
    market: "CN",
    currency: "CNY",
    price: 1486.2,
    change: 12.35,
    changePercent: 0.84,
    open: 1472.08,
    high: 1491.88,
    low: 1468.5,
    previousClose: 1473.85,
    volume: 2864500,
    sector: "食品饮料",
    status: "已收盘",
  },
  {
    id: "CN:000001",
    symbol: "000001",
    name: "平安银行",
    exchange: "SZSE",
    market: "CN",
    currency: "CNY",
    price: 11.72,
    change: -0.08,
    changePercent: -0.68,
    open: 11.81,
    high: 11.86,
    low: 11.68,
    previousClose: 11.8,
    volume: 72816400,
    sector: "银行",
    status: "已收盘",
  },
  {
    id: "KR:005930",
    symbol: "005930",
    name: "Samsung Electronics",
    exchange: "KRX",
    market: "KR",
    currency: "KRW",
    price: 103500,
    change: 2100,
    changePercent: 2.07,
    open: 101800,
    high: 104200,
    low: 101500,
    previousClose: 101400,
    volume: 18452700,
    sector: "半导体",
    status: "已收盘",
  },
  {
    id: "HK:0700",
    symbol: "0700",
    name: "腾讯控股",
    exchange: "HKEX",
    market: "HK",
    currency: "HKD",
    price: 558.5,
    change: 4.5,
    changePercent: 0.81,
    open: 554,
    high: 561,
    low: 551.5,
    previousClose: 554,
    volume: 16784900,
    sector: "互联网",
    status: "已收盘",
  },
  {
    id: "JP:7203",
    symbol: "7203",
    name: "Toyota Motor",
    exchange: "TSE",
    market: "JP",
    currency: "JPY",
    price: 2834.5,
    change: -18,
    changePercent: -0.63,
    open: 2856,
    high: 2871.5,
    low: 2821,
    previousClose: 2852.5,
    volume: 21967200,
    sector: "汽车",
    status: "已收盘",
  },
  {
    id: "US:TSLA",
    symbol: "TSLA",
    name: "Tesla",
    exchange: "NASDAQ",
    market: "US",
    currency: "USD",
    price: 354.62,
    change: 5.84,
    changePercent: 1.67,
    open: 350.21,
    high: 358.44,
    low: 347.9,
    previousClose: 348.78,
    volume: 92718500,
    sector: "汽车",
    status: "交易中",
  },
];

const indexOverview = [
  { name: "标普 500", symbol: "SPX", value: "6,412.83", change: 0.48 },
  { name: "纳斯达克", symbol: "IXIC", value: "21,681.90", change: 0.71 },
  { name: "沪深 300", symbol: "CSI300", value: "4,126.34", change: -0.26 },
  { name: "韩国综合", symbol: "KOSPI", value: "3,287.14", change: 1.12 },
];

const initialWatchlist = ["US:AAPL", "US:NVDA", "CN:600519", "KR:005930", "HK:0700"];

export default function GlobalStocksPage() {
  const [watchlistIds, setWatchlistIds] = React.useState(initialWatchlist);
  const [selectedId, setSelectedId] = React.useState(initialWatchlist[0]);
  const [addOpen, setAddOpen] = React.useState(false);
  const [query, setQuery] = React.useState("");
  const [market, setMarket] = React.useState<MarketCode | "ALL">("ALL");

  const watchlist = watchlistIds
    .map((id) => quoteCatalog.find((quote) => quote.id === id))
    .filter((quote): quote is StockQuote => Boolean(quote));
  const selected =
    watchlist.find((quote) => quote.id === selectedId) ?? watchlist[0] ?? null;

  const candidates = quoteCatalog.filter((quote) => {
    const normalized = query.trim().toLowerCase();
    const matchesQuery =
      normalized === "" ||
      quote.symbol.toLowerCase().includes(normalized) ||
      quote.name.toLowerCase().includes(normalized);
    return matchesQuery && (market === "ALL" || quote.market === market);
  });

  function removeFromWatchlist(id: string) {
    setWatchlistIds((current) => {
      const next = current.filter((item) => item !== id);
      if (selectedId === id) setSelectedId(next[0] ?? "");
      return next;
    });
  }

  function addToWatchlist(id: string) {
    setWatchlistIds((current) =>
      current.includes(id) ? current : [...current, id],
    );
    setSelectedId(id);
    setAddOpen(false);
  }

  return (
    <PageFrame>
      <header className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <div className="text-[11px] font-semibold tracking-[0.22em] text-primary">
            GLOBAL EQUITIES
          </div>
          <h1 className="mt-1 text-2xl font-semibold tracking-tight">全球股票</h1>
          <p className="mt-1 text-xs text-muted-foreground">
            自选行情 · 静态数据演示
          </p>
        </div>
        <div className="flex items-center gap-2">
          <div className="hidden items-center gap-1.5 text-[11px] text-muted-foreground sm:flex">
            <Clock3 className="size-3" />
            更新于 15:08:32
          </div>
          <Button variant="outline" size="sm" disabled>
            <RefreshCw className="size-3.5" />
            刷新
          </Button>
          <Button size="sm" onClick={() => setAddOpen(true)}>
            <Plus className="size-3.5" />
            添加自选
          </Button>
        </div>
      </header>

      <section className="grid gap-2 sm:grid-cols-2 xl:grid-cols-4">
        {indexOverview.map((index) => (
          <div key={index.symbol} className="rounded-xl border bg-card px-4 py-3 shadow-sm">
            <div className="flex items-center justify-between text-xs text-muted-foreground">
              <span>{index.name}</span>
              <span className="font-mono">{index.symbol}</span>
            </div>
            <div className="mt-2 flex items-end justify-between">
              <span className="font-mono text-lg font-semibold tabular-nums">
                {index.value}
              </span>
              <span
                className={cn(
                  "font-mono text-xs",
                  index.change >= 0 ? "text-emerald-600" : "text-rose-600",
                )}
              >
                {formatSignedPercent(index.change)}
              </span>
            </div>
          </div>
        ))}
      </section>

      <div className="grid min-h-0 flex-1 gap-3 xl:grid-cols-[minmax(0,1fr)_320px]">
        <section className="flex min-h-0 min-w-0 flex-col overflow-hidden rounded-xl border bg-card shadow-sm h-[var(--app-panel-height)] max-h-[var(--app-panel-height)]">
          <div className="flex items-center justify-between border-b px-4 py-3">
            <div>
              <div className="text-sm font-medium">我的自选</div>
              <div className="mt-0.5 text-[11px] text-muted-foreground">
                {watchlist.length} 个标的 · 点击行查看行情摘要
              </div>
            </div>
            <Badge variant="outline">跨市场</Badge>
          </div>

          {watchlist.length === 0 ? (
            <div className="flex min-h-0 flex-1 flex-col items-center justify-center gap-3 p-8 text-center">
              <div className="flex size-12 items-center justify-center rounded-2xl bg-primary/10 text-primary">
                <Globe2 className="size-5" />
              </div>
              <div>
                <div className="text-sm font-medium">自选列表为空</div>
                <p className="mt-1 text-xs text-muted-foreground">
                  添加股票后可在这里集中查看全球市场行情。
                </p>
              </div>
              <Button size="sm" onClick={() => setAddOpen(true)}>
                <Plus className="size-3.5" />
                添加自选
              </Button>
            </div>
          ) : (
            <WideTableScroll className="min-h-0 flex-1">
              <table className="w-full min-w-[1040px] text-sm">
                <thead className="sticky top-0 z-10 bg-muted/30 text-[11px] text-muted-foreground">
                  <tr className="border-b">
                    <th className="px-4 py-2.5 text-left font-medium">股票</th>
                    <th className="px-3 py-2.5 text-left font-medium">市场</th>
                    <th className="px-3 py-2.5 text-right font-medium">最新价</th>
                    <th className="px-3 py-2.5 text-right font-medium">涨跌幅</th>
                    <th className="px-3 py-2.5 text-right font-medium">今开</th>
                    <th className="px-3 py-2.5 text-right font-medium">最高</th>
                    <th className="px-3 py-2.5 text-right font-medium">最低</th>
                    <th className="px-3 py-2.5 text-right font-medium">成交量</th>
                    <th className="px-3 py-2.5 text-center font-medium">状态</th>
                    <th className="w-12 px-3 py-2.5" />
                  </tr>
                </thead>
                <tbody>
                  {watchlist.map((quote) => {
                    const active = quote.id === selected?.id;
                    const positive = quote.changePercent >= 0;
                    return (
                      <tr
                        key={quote.id}
                        onClick={() => setSelectedId(quote.id)}
                        className={cn(
                          "cursor-pointer border-b transition-colors last:border-0 hover:bg-muted/30",
                          active && "bg-primary/[0.06]",
                        )}
                      >
                        <td className="px-4 py-3">
                          <div className="font-medium">{quote.name}</div>
                          <div className="mt-0.5 font-mono text-[11px] text-muted-foreground">
                            {quote.symbol}
                          </div>
                        </td>
                        <td className="px-3 py-3">
                          <div>{quote.exchange}</div>
                          <div className="mt-0.5 text-[11px] text-muted-foreground">
                            {marketLabels[quote.market]} · {quote.currency}
                          </div>
                        </td>
                        <td className="px-3 py-3 text-right font-mono tabular-nums">
                          {formatPrice(quote.price, quote.currency)}
                        </td>
                        <td
                          className={cn(
                            "px-3 py-3 text-right font-mono tabular-nums",
                            positive ? "text-emerald-600" : "text-rose-600",
                          )}
                        >
                          <div>{formatSignedPercent(quote.changePercent)}</div>
                          <div className="mt-0.5 text-[11px] opacity-80">
                            {formatSignedNumber(quote.change)}
                          </div>
                        </td>
                        <QuoteCell value={quote.open} currency={quote.currency} />
                        <QuoteCell value={quote.high} currency={quote.currency} />
                        <QuoteCell value={quote.low} currency={quote.currency} />
                        <td className="px-3 py-3 text-right font-mono text-xs tabular-nums">
                          {formatVolume(quote.volume)}
                        </td>
                        <td className="px-3 py-3 text-center">
                          <span
                            className={cn(
                              "inline-flex rounded-full px-2 py-0.5 text-[10px]",
                              quote.status === "交易中"
                                ? "bg-emerald-500/10 text-emerald-600"
                                : "bg-muted text-muted-foreground",
                            )}
                          >
                            {quote.status}
                          </span>
                        </td>
                        <td className="px-3 py-3">
                          <Button
                            type="button"
                            variant="ghost"
                            size="icon-sm"
                            aria-label={`删除 ${quote.name}`}
                            onClick={(event) => {
                              event.stopPropagation();
                              removeFromWatchlist(quote.id);
                            }}
                            className="text-muted-foreground hover:text-destructive"
                          >
                            <Trash2 className="size-3.5" />
                          </Button>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </WideTableScroll>
          )}
        </section>

        <QuoteDetail quote={selected} />
      </div>

      <Sheet
        open={addOpen}
        onOpenChange={(open) => {
          setAddOpen(open);
          if (!open) {
            setQuery("");
            setMarket("ALL");
          }
        }}
      >
        <SheetContent side="right" className="w-full sm:max-w-md">
          <SheetHeader>
            <SheetTitle>添加自选股票</SheetTitle>
            <SheetDescription>
              从全球市场静态标的中选择并加入当前自选列表。
            </SheetDescription>
          </SheetHeader>
          <div className="grid gap-3 px-4">
            <div className="relative">
              <Search className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input
                value={query}
                onChange={(event) => setQuery(event.target.value)}
                placeholder="搜索股票代码或名称"
                className="pl-8"
              />
            </div>
            <div className="flex flex-wrap gap-1.5">
              {(["ALL", "US", "CN", "HK", "KR", "JP"] as const).map((item) => (
                <button
                  key={item}
                  type="button"
                  onClick={() => setMarket(item)}
                  className={cn(
                    "rounded-full border px-2.5 py-1 text-xs transition-colors",
                    market === item
                      ? "border-primary/30 bg-primary/10 text-primary"
                      : "text-muted-foreground hover:bg-muted",
                  )}
                >
                  {item === "ALL" ? "全部" : marketLabels[item]}
                </button>
              ))}
            </div>
          </div>
          <div className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
            <div className="grid gap-2">
              {candidates.map((quote) => {
                const added = watchlistIds.includes(quote.id);
                return (
                  <div
                    key={quote.id}
                    className="flex items-center gap-3 rounded-xl border p-3"
                  >
                    <div className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-muted font-mono text-xs font-semibold">
                      {quote.market}
                    </div>
                    <div className="min-w-0 flex-1">
                      <div className="truncate text-sm font-medium">{quote.name}</div>
                      <div className="mt-0.5 truncate text-[11px] text-muted-foreground">
                        {quote.symbol} · {quote.exchange} · {quote.currency}
                      </div>
                    </div>
                    <Button
                      size="sm"
                      variant={added ? "outline" : "default"}
                      disabled={added}
                      onClick={() => addToWatchlist(quote.id)}
                    >
                      {added ? "已添加" : "添加"}
                    </Button>
                  </div>
                );
              })}
            </div>
          </div>
        </SheetContent>
      </Sheet>
    </PageFrame>
  );
}

function QuoteCell({ value, currency }: { value: number; currency: string }) {
  return (
    <td className="px-3 py-3 text-right font-mono text-xs tabular-nums">
      {formatPrice(value, currency)}
    </td>
  );
}

function QuoteDetail({ quote }: { quote: StockQuote | null }) {
  if (!quote) {
    return (
      <aside className="flex items-center justify-center rounded-xl border bg-card p-6 text-center text-sm text-muted-foreground">
        选择自选股票查看行情摘要。
      </aside>
    );
  }
  const positive = quote.changePercent >= 0;
  return (
    <aside className="rounded-xl border bg-card shadow-sm">
      <div className="border-b p-4">
        <div className="flex items-start justify-between gap-3">
          <div>
            <div className="text-base font-semibold">{quote.name}</div>
            <div className="mt-1 font-mono text-xs text-muted-foreground">
              {quote.symbol} · {quote.exchange}
            </div>
          </div>
          <Badge variant="outline">{quote.sector}</Badge>
        </div>
        <div className="mt-5">
          <div className="font-mono text-3xl font-semibold tabular-nums">
            {formatPrice(quote.price, quote.currency)}
          </div>
          <div
            className={cn(
              "mt-1 font-mono text-sm",
              positive ? "text-emerald-600" : "text-rose-600",
            )}
          >
            {formatSignedNumber(quote.change)} ({formatSignedPercent(quote.changePercent)})
          </div>
        </div>
      </div>

      <div className="border-b p-4">
        <div className="mb-3 flex items-center justify-between text-xs">
          <span className="font-medium">日内走势</span>
          <span className="text-muted-foreground">静态示意</span>
        </div>
        <svg
          viewBox="0 0 280 90"
          role="img"
          aria-label={`${quote.name} 日内走势示意`}
          className={cn(
            "h-24 w-full",
            positive ? "text-emerald-500" : "text-rose-500",
          )}
        >
          <path
            d="M2 73 L25 65 L48 70 L72 48 L96 54 L119 38 L142 44 L166 27 L190 35 L213 17 L237 24 L278 9"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            vectorEffect="non-scaling-stroke"
          />
          <path
            d="M2 73 L25 65 L48 70 L72 48 L96 54 L119 38 L142 44 L166 27 L190 35 L213 17 L237 24 L278 9 L278 90 L2 90 Z"
            fill="currentColor"
            opacity="0.08"
          />
        </svg>
      </div>

      <dl className="grid grid-cols-2 gap-x-5 gap-y-3 p-4 text-xs">
        <DetailMetric label="今开" value={formatPrice(quote.open, quote.currency)} />
        <DetailMetric label="昨收" value={formatPrice(quote.previousClose, quote.currency)} />
        <DetailMetric label="最高" value={formatPrice(quote.high, quote.currency)} />
        <DetailMetric label="最低" value={formatPrice(quote.low, quote.currency)} />
        <DetailMetric label="成交量" value={formatVolume(quote.volume)} />
        <DetailMetric label="交易状态" value={quote.status} />
      </dl>
    </aside>
  );
}

function DetailMetric({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="mt-1 font-mono font-medium tabular-nums">{value}</dd>
    </div>
  );
}

function formatPrice(value: number, currency: string) {
  return new Intl.NumberFormat("en-US", {
    style: "currency",
    currency,
    minimumFractionDigits: currency === "KRW" || currency === "JPY" ? 0 : 2,
    maximumFractionDigits: currency === "KRW" || currency === "JPY" ? 0 : 2,
  }).format(value);
}

function formatVolume(value: number) {
  return new Intl.NumberFormat("zh-CN", {
    notation: "compact",
    maximumFractionDigits: 2,
  }).format(value);
}

function formatSignedPercent(value: number) {
  return `${value >= 0 ? "+" : ""}${value.toFixed(2)}%`;
}

function formatSignedNumber(value: number) {
  return `${value >= 0 ? "+" : ""}${value.toFixed(2)}`;
}
