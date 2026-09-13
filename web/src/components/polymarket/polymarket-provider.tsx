"use client";

import * as React from "react";

import { useAuth } from "@/components/auth/auth-provider";
import {
  applySnapshotParts,
  fetchPolymarketAccountSummary,
  fetchPolymarketMarkets,
  fetchPolymarketOpenOrders,
  fetchPolymarketPositions,
  fetchPolymarketSnapshot,
  liveQuoteFromSnapshot,
  mapPolymarketAccountEvent,
  mapSnapshot,
  polymarketAccountStreamUrl,
  polymarketStreamUrl,
  type PolymarketLiveQuote,
} from "@/lib/api/polymarket";
import { fetchTradingAccounts, type TradingAccount } from "@/lib/api/accounts";
import {
  aggdataWebSocketUrl,
  decodeFairPriceMessage,
  fetchAggdataMarkets,
  fetchFairPriceHistory,
  resolveFairPriceMarket,
  type AggdataMarket,
} from "@/lib/api/aggdata";
import {
  isFairPriceStale,
  mergeFairPricePoints,
  sameFairPriceSequence,
  toPolymarketFairPricePoint,
} from "@/lib/polymarket-fairprice";
import { filterFairPricePointsToWindow } from "@/lib/polymarket-chart";
import type {
  PolymarketAccountSummary,
  PolymarketAssetId,
  PolymarketMarket,
  PolymarketMarketSnapshot,
  PolymarketOpenOrder,
  PolymarketPeriodId,
  PolymarketPosition,
  PolymarketFairPricePoint,
  PolymarketFairPriceSource,
  PolymarketFairPriceStatus,
  PolymarketPricePoint,
} from "@/types/polymarket";
import {
  chooseFirstLivePeriod,
  chooseLiveMarket,
  MARKET_REFRESH_MS,
} from "@/lib/polymarket-market";
import { polymarketPeriods } from "@/lib/polymarket";

import {
  DEFAULT_POLYMARKET_ASSET_ID,
  DEFAULT_POLYMARKET_PERIOD_ID,
} from "@/types/polymarket";

interface PolymarketContextValue {
  markets: PolymarketMarket[];
  selectedMarket: PolymarketMarket | null;
  snapshot: PolymarketMarketSnapshot | null;
  chartPoints: PolymarketPricePoint[];
  liveQuote: PolymarketLiveQuote | null;
  currentFairPrice: PolymarketFairPricePoint | null;
  fairPricePoints: PolymarketFairPricePoint[];
  fairPriceSource: PolymarketFairPriceSource | null;
  fairPriceStatus: PolymarketFairPriceStatus;
  fairPriceError: string | null;
  fairPriceHistoryError: string | null;
  selectedAsset: PolymarketAssetId;
  selectedPeriod: PolymarketPeriodId;
  selectAsset: (value: PolymarketAssetId) => void;
  selectPeriod: (value: PolymarketPeriodId) => void;
  loading: boolean;
  snapshotLoading: boolean;
  error: string | null;
  accounts: TradingAccount[];
  selectedAccountId: number | null;
  selectAccount: (id: number) => void;
  summary: PolymarketAccountSummary | null;
  openOrders: PolymarketOpenOrder[];
  openOrdersStale: boolean;
  positions: PolymarketPosition[];
  positionsStale: boolean;
  privateLoading: boolean;
  summaryError: string | null;
  openOrdersError: string | null;
  positionsError: string | null;
  refreshPrivate: () => Promise<void>;
  removeOpenOrder: (orderId: string) => void;
  refreshAccounts: () => Promise<void>;
  refreshMarkets: () => Promise<void>;
}

const PolymarketContext = React.createContext<PolymarketContextValue | null>(null);

const PERIOD_ORDER = polymarketPeriods.map((period) => period.id);

function resetSnapshotState(
  setLiveSnapshot: React.Dispatch<React.SetStateAction<PolymarketMarketSnapshot | null>>,
  setChartPoints: React.Dispatch<React.SetStateAction<PolymarketPricePoint[]>>,
) {
  setLiveSnapshot(null);
  setChartPoints([]);
}

export function PolymarketProvider({ children }: { children: React.ReactNode }) {
  const { status } = useAuth();
  const [markets, setMarkets] = React.useState<PolymarketMarket[]>([]);
  const [liveSnapshot, setLiveSnapshot] = React.useState<PolymarketMarketSnapshot | null>(null);
  const liveSnapshotRef = React.useRef<PolymarketMarketSnapshot | null>(null);
  const [chartPoints, setChartPoints] = React.useState<PolymarketPricePoint[]>([]);
  const chartPointsRef = React.useRef<PolymarketPricePoint[]>([]);
  const [aggdataMarkets, setAggdataMarkets] = React.useState<AggdataMarket[]>([]);
  const [currentFairPrice, setCurrentFairPrice] =
    React.useState<PolymarketFairPricePoint | null>(null);
  const currentFairPriceRef = React.useRef<PolymarketFairPricePoint | null>(null);
  const [fairPricePoints, setFairPricePoints] = React.useState<
    PolymarketFairPricePoint[]
  >([]);
  const fairPricePointsRef = React.useRef<PolymarketFairPricePoint[]>([]);
  const fairSessionRef = React.useRef(0);
  const [fairPriceSource, setFairPriceSource] =
    React.useState<PolymarketFairPriceSource | null>(null);
  const [fairPriceStatus, setFairPriceStatus] =
    React.useState<PolymarketFairPriceStatus>("connecting");
  const [fairPriceError, setFairPriceError] = React.useState<string | null>(null);
  const [fairPriceHistoryError, setFairPriceHistoryError] =
    React.useState<string | null>(null);
  const [selectedAsset, setSelectedAsset] = React.useState<PolymarketAssetId>(
    DEFAULT_POLYMARKET_ASSET_ID,
  );
  const [selectedPeriod, setSelectedPeriod] = React.useState<PolymarketPeriodId>(
    DEFAULT_POLYMARKET_PERIOD_ID,
  );
  const [loading, setLoading] = React.useState(true);
  const [snapshotLoading, setSnapshotLoading] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [accounts, setAccounts] = React.useState<TradingAccount[]>([]);
  const [selectedAccountId, setSelectedAccountId] = React.useState<number | null>(null);
  const [summary, setSummary] = React.useState<PolymarketAccountSummary | null>(null);
  const [openOrders, setOpenOrders] = React.useState<PolymarketOpenOrder[]>([]);
  const [openOrdersStale, setOpenOrdersStale] = React.useState(false);
  const [positions, setPositions] = React.useState<PolymarketPosition[]>([]);
  const [positionsStale, setPositionsStale] = React.useState(false);
  const [privateLoading, setPrivateLoading] = React.useState(false);
  const privateLoadedRef = React.useRef(false);
  const [summaryError, setSummaryError] = React.useState<string | null>(null);
  const [openOrdersError, setOpenOrdersError] = React.useState<string | null>(null);
  const [positionsError, setPositionsError] = React.useState<string | null>(null);

  const ingestSnapshot = React.useCallback((incoming: PolymarketMarketSnapshot) => {
    const current = liveSnapshotRef.current;
    const { liveSnapshot: nextSnapshot, chartPoints: nextPoints } = applySnapshotParts(
      current,
      chartPointsRef.current,
      incoming,
    );
    liveSnapshotRef.current = nextSnapshot;
    setLiveSnapshot(nextSnapshot);
    if (
      incoming.priceSeries.length > 0 ||
      current?.market.id !== incoming.market.id
    ) {
      chartPointsRef.current = nextPoints;
      setChartPoints(nextPoints);
    }
  }, []);

  const refreshMarkets = React.useCallback(async (initial = false) => {
    try {
      const items = await fetchPolymarketMarkets();
      setMarkets(items);
      setError(null);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "市场加载失败");
    } finally {
      if (initial) {
        setLoading(false);
      }
    }
  }, []);

  const refreshMarketsOnly = React.useCallback(async () => {
    await refreshMarkets();
  }, [refreshMarkets]);

  React.useEffect(() => {
    const kickoff = setTimeout(() => void refreshMarkets(true), 0);
    const timer = setInterval(() => void refreshMarkets(), MARKET_REFRESH_MS);
    const onVisibility = () => {
      if (document.visibilityState === "visible") {
        void refreshMarkets();
      }
    };
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      clearTimeout(kickoff);
      clearInterval(timer);
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [refreshMarkets]);

  React.useEffect(() => {
    const controller = new AbortController();
    void fetchAggdataMarkets(controller.signal)
      .then((items) => {
        setAggdataMarkets(items);
        setFairPriceError(null);
      })
      .catch((reason) => {
        if (controller.signal.aborted) return;
        setFairPriceStatus("unavailable");
        setFairPriceError(
          reason instanceof Error ? reason.message : "Fair Price 市场加载失败",
        );
      });
    return () => controller.abort();
  }, []);

  const selectedMarket = React.useMemo(
    () => chooseLiveMarket(markets, selectedAsset, selectedPeriod),
    [markets, selectedAsset, selectedPeriod],
  );

  React.useEffect(() => {
    const fallback = chooseFirstLivePeriod(markets, selectedAsset, PERIOD_ORDER);
    if (!fallback) {
      return;
    }
    if (chooseLiveMarket(markets, selectedAsset, selectedPeriod)) {
      return;
    }
    const applyFallback = setTimeout(() => {
      setSelectedPeriod(fallback);
      resetSnapshotState(setLiveSnapshot, setChartPoints);
      liveSnapshotRef.current = null;
      chartPointsRef.current = [];
      setSnapshotLoading(true);
    }, 0);
    return () => clearTimeout(applyFallback);
  }, [markets, selectedAsset, selectedPeriod]);

  const selectAsset = React.useCallback((value: PolymarketAssetId) => {
    setSelectedAsset(value);
    resetSnapshotState(setLiveSnapshot, setChartPoints);
    liveSnapshotRef.current = null;
    chartPointsRef.current = [];
    currentFairPriceRef.current = null;
    fairPricePointsRef.current = [];
    setCurrentFairPrice(null);
    setFairPricePoints([]);
    setFairPriceStatus("connecting");
    setSnapshotLoading(true);
  }, []);

  const selectPeriod = React.useCallback((value: PolymarketPeriodId) => {
    setSelectedPeriod(value);
    resetSnapshotState(setLiveSnapshot, setChartPoints);
    liveSnapshotRef.current = null;
    chartPointsRef.current = [];
    currentFairPriceRef.current = null;
    fairPricePointsRef.current = [];
    setCurrentFairPrice(null);
    setFairPricePoints([]);
    setFairPriceStatus("connecting");
    setSnapshotLoading(true);
  }, []);

  const selectedMarketId = selectedMarket?.id;
  const selectedWindowStart = selectedMarket?.windowStart;
  const selectedWindowEnd = selectedMarket?.windowEnd;

  React.useEffect(() => {
    if (!selectedMarketId) {
      const reset = setTimeout(() => {
        const current = liveSnapshotRef.current;
        if (
          current?.market.asset !== selectedAsset ||
          current?.market.period !== selectedPeriod
        ) {
          resetSnapshotState(setLiveSnapshot, setChartPoints);
          liveSnapshotRef.current = null;
          chartPointsRef.current = [];
        }
        setSnapshotLoading(false);
      }, 0);
      return () => clearTimeout(reset);
    }
    let cancelled = false;
    let etag = "";
    let pollTimer: ReturnType<typeof setTimeout> | null = null;
    let polling = false;
    const loadingTimer = setTimeout(() => setSnapshotLoading(true), 0);
    const poll = async () => {
      try {
        const result = await fetchPolymarketSnapshot(selectedMarketId, etag);
        etag = result.etag || etag;
        if (!cancelled && result.snapshot) {
          ingestSnapshot(result.snapshot);
          setError(null);
          setSnapshotLoading(false);
        }
      } catch (reason) {
        if (!cancelled) {
          setError(reason instanceof Error ? reason.message : "快照加载失败");
          setSnapshotLoading(false);
        }
      } finally {
        if (!cancelled && polling) {
          pollTimer = setTimeout(poll, 5_000);
        }
      }
    };
    void poll();
    const source = new EventSource(polymarketStreamUrl(selectedMarketId), {
      withCredentials: true,
    });
    source.addEventListener("snapshot", (event) => {
      if (cancelled) return;
      const payload = JSON.parse((event as MessageEvent<string>).data) as {
        data: unknown;
      };
      ingestSnapshot(mapSnapshot(payload.data));
      setError(null);
      setSnapshotLoading(false);
      polling = false;
      if (pollTimer) clearTimeout(pollTimer);
    });
    source.onerror = () => {
      if (cancelled || polling) return;
      polling = true;
      pollTimer = setTimeout(poll, 1_000);
    };
    return () => {
      cancelled = true;
      clearTimeout(loadingTimer);
      source.close();
      if (pollTimer) clearTimeout(pollTimer);
    };
  }, [ingestSnapshot, selectedAsset, selectedMarketId, selectedPeriod]);

  React.useEffect(() => {
    const session = ++fairSessionRef.current;
    const sessionActive = () => fairSessionRef.current === session;
    if (
      !selectedMarketId ||
      !selectedWindowStart ||
      !selectedWindowEnd ||
      aggdataMarkets.length === 0
    ) {
      const reset = setTimeout(() => {
        if (!sessionActive()) return;
        currentFairPriceRef.current = null;
        fairPricePointsRef.current = [];
        setCurrentFairPrice(null);
        setFairPricePoints([]);
        setFairPriceSource(null);
        setFairPriceStatus(aggdataMarkets.length === 0 ? "connecting" : "unavailable");
        setFairPriceHistoryError(null);
      }, 0);
      return () => clearTimeout(reset);
    }
    const fairMarket = resolveFairPriceMarket(aggdataMarkets, selectedAsset);
    if (!fairMarket) {
      const reset = setTimeout(() => {
        if (!sessionActive()) return;
        currentFairPriceRef.current = null;
        fairPricePointsRef.current = [];
        setCurrentFairPrice(null);
        setFairPricePoints([]);
        setFairPriceSource(null);
        setFairPriceStatus("unavailable");
        setFairPriceError("未找到唯一的 aggdata Fair Price 市场");
        setFairPriceHistoryError(null);
      }, 0);
      return () => clearTimeout(reset);
    }

    let cancelled = false;
    let socket: WebSocket | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let currentTimer: ReturnType<typeof setTimeout> | null = null;
    let chartTimer: ReturnType<typeof setTimeout> | null = null;
    let staleTimer: ReturnType<typeof setInterval> | null = null;
    let retryCount = 0;
    let pendingCurrent: PolymarketFairPricePoint | null = null;
    const marketWindowEndMs = Date.parse(selectedWindowEnd);
    const controller = new AbortController();
    const resetTimer = setTimeout(() => {
      if (!sessionActive()) return;
      currentFairPriceRef.current = null;
      fairPricePointsRef.current = [];
      setCurrentFairPrice(null);
      setFairPricePoints([]);
      setFairPriceSource({
        profile: fairMarket.profile,
        symbol: fairMarket.symbol,
      });
      setFairPriceStatus("connecting");
      setFairPriceError(null);
      setFairPriceHistoryError(null);
    }, 0);

    const flushCurrent = () => {
      currentTimer = null;
      if (cancelled || !sessionActive() || pendingCurrent == null) return;
      setCurrentFairPrice(pendingCurrent);
      pendingCurrent = null;
    };
    const flushChart = () => {
      chartTimer = null;
      if (!cancelled && sessionActive()) {
        setFairPricePoints(fairPricePointsRef.current);
      }
    };
    const ingestFairPrice = (point: PolymarketFairPricePoint) => {
      if (!sessionActive()) return;
      if (sameFairPriceSequence(currentFairPriceRef.current, point)) return;
      const inWindow = filterFairPricePointsToWindow(
        [point],
        selectedWindowStart,
        selectedWindowEnd,
        marketWindowEndMs,
      ).length > 0;
      if (!inWindow) {
        setFairPriceError("out_of_window");
        return;
      }
      if (isFairPriceStale(point)) {
        setFairPriceStatus("stale");
        setFairPriceError("source_stale");
        return;
      }
      currentFairPriceRef.current = point;
      pendingCurrent = point;
      fairPricePointsRef.current = mergeFairPricePoints(
        fairPricePointsRef.current,
        [point],
        2000,
        {
          start: selectedWindowStart,
          end: selectedWindowEnd,
          nowMs: marketWindowEndMs,
        },
      );
      setFairPriceStatus("live");
      setFairPriceError(null);
      if (currentTimer == null) currentTimer = setTimeout(flushCurrent, 100);
      if (chartTimer == null) chartTimer = setTimeout(flushChart, 1000);
    };

    const historyEnd = new Date(
      Math.min(Date.now(), new Date(selectedWindowEnd).getTime()),
    );
    const historyStart = new Date(selectedWindowStart);
    if (historyStart < historyEnd) {
      void fetchFairPriceHistory(
        fairMarket,
        historyStart.toISOString(),
        historyEnd.toISOString(),
        controller.signal,
      )
        .then((history) => {
          if (cancelled || !sessionActive()) return;
          fairPricePointsRef.current = mergeFairPricePoints(
            fairPricePointsRef.current,
            history.points.map(toPolymarketFairPricePoint),
            2000,
            {
              start: selectedWindowStart,
              end: selectedWindowEnd,
              nowMs: marketWindowEndMs,
            },
          );
          setFairPricePoints(fairPricePointsRef.current);
        })
        .catch((reason) => {
          if (cancelled || controller.signal.aborted) return;
          setFairPriceHistoryError(
            reason instanceof Error ? reason.message : "Fair Price 历史加载失败",
          );
        });
    }

    const connect = () => {
      if (cancelled || !sessionActive() || socket != null) return;
      const nextSocket = new WebSocket(aggdataWebSocketUrl());
      socket = nextSocket;
      nextSocket.onopen = () => {
        retryCount = 0;
        nextSocket.send(JSON.stringify({
          op: "subscribe",
          profile: fairMarket.profile,
          symbol: fairMarket.symbol,
          channel: "fairprice",
        }));
      };
      nextSocket.onmessage = (event) => {
        if (cancelled || !sessionActive() || typeof event.data !== "string") return;
        try {
          const message = decodeFairPriceMessage(event.data);
          if (message.type === "ack") {
            if (!message.ok) {
              setFairPriceError(message.error ?? "Fair Price 订阅被拒绝");
              nextSocket.close();
            }
            return;
          }
          if (message.type === "reset") {
            pendingCurrent = null;
            if (currentTimer) {
              clearTimeout(currentTimer);
              currentTimer = null;
            }
            currentFairPriceRef.current = null;
            setCurrentFairPrice(null);
            setFairPriceStatus("stale");
            setFairPriceError(message.reason);
            return;
          }
          ingestFairPrice(toPolymarketFairPricePoint(message));
        } catch (reason) {
          setFairPriceError(
            reason instanceof Error ? reason.message : "Fair Price 消息解析失败",
          );
        }
      };
      nextSocket.onerror = () => nextSocket.close();
      nextSocket.onclose = () => {
        if (socket === nextSocket) socket = null;
        if (cancelled) return;
        setFairPriceStatus(
          currentFairPriceRef.current == null ? "connecting" : "stale",
        );
        const delay = Math.min(500 * 2 ** retryCount, 10_000);
        retryCount += 1;
        reconnectTimer = setTimeout(connect, delay + Math.random() * 250);
      };
    };
    connect();
    staleTimer = setInterval(() => {
      if (!sessionActive()) return;
      if (currentFairPriceRef.current &&
          isFairPriceStale(currentFairPriceRef.current)) {
        setFairPriceStatus("stale");
        setFairPriceError("source_stale");
      }
    }, 1000);

    return () => {
      cancelled = true;
      controller.abort();
      clearTimeout(resetTimer);
      if (reconnectTimer) clearTimeout(reconnectTimer);
      if (currentTimer) clearTimeout(currentTimer);
      if (chartTimer) clearTimeout(chartTimer);
      if (staleTimer) clearInterval(staleTimer);
      socket?.close();
    };
  }, [
    aggdataMarkets,
    selectedAsset,
    selectedMarketId,
    selectedWindowEnd,
    selectedWindowStart,
  ]);

  const refreshAccounts = React.useCallback(async () => {
    if (status !== "authenticated") return;
    try {
      const items = await fetchTradingAccounts();
      const polymarketAccounts = items.filter(
        (item) => item.exchangeSlug === "polymarket",
      );
      setAccounts(polymarketAccounts);
      setSelectedAccountId((current) =>
        current && polymarketAccounts.some((item) => item.id === current)
          ? current
          : (polymarketAccounts[0]?.id ?? null),
      );
    } catch (reason) {
      setSummaryError(reason instanceof Error ? reason.message : "账户加载失败");
    }
  }, [status]);

  React.useEffect(() => {
    if (status !== "authenticated") {
      return;
    }
    const kickoff = setTimeout(() => void refreshAccounts(), 0);
    return () => clearTimeout(kickoff);
  }, [refreshAccounts, status]);

  React.useEffect(() => {
    if (status !== "authenticated") return;
    const onFocus = () => void refreshAccounts();
    window.addEventListener("focus", onFocus);
    return () => window.removeEventListener("focus", onFocus);
  }, [refreshAccounts, status]);

  const refreshPrivate = React.useCallback(async () => {
    if (status !== "authenticated" || selectedAccountId == null) return;
    if (!privateLoadedRef.current) {
      setPrivateLoading(true);
    }
    setSummaryError(null);
    setOpenOrdersError(null);
    setPositionsError(null);
    const [summaryResult, positionsResult, openOrdersResult] = await Promise.allSettled([
      fetchPolymarketAccountSummary(selectedAccountId),
      fetchPolymarketPositions(selectedAccountId),
      fetchPolymarketOpenOrders(selectedAccountId),
    ]);
    if (summaryResult.status === "fulfilled") {
      setSummary(summaryResult.value);
      setSummaryError(null);
    } else {
      setSummaryError(
        summaryResult.reason instanceof Error
          ? summaryResult.reason.message
          : "账户资产加载失败",
      );
    }
    if (positionsResult.status === "fulfilled") {
      setPositions(positionsResult.value.positions);
      setPositionsStale(positionsResult.value.stale);
      setPositionsError(null);
    } else {
      setPositionsError(
        positionsResult.reason instanceof Error
          ? positionsResult.reason.message
          : "持仓加载失败",
      );
    }
    if (openOrdersResult.status === "fulfilled") {
      setOpenOrders(openOrdersResult.value.openOrders);
      setOpenOrdersStale(openOrdersResult.value.stale);
      setOpenOrdersError(null);
    } else {
      setOpenOrdersError(
        openOrdersResult.reason instanceof Error
          ? openOrdersResult.reason.message
          : "挂单加载失败",
      );
    }
    privateLoadedRef.current = true;
    setPrivateLoading(false);
  }, [selectedAccountId, status]);

  React.useEffect(() => {
    if (selectedAccountId == null) {
      return;
    }
    const kickoff = setTimeout(() => void refreshPrivate(), 0);
    const timer = setInterval(() => void refreshPrivate(), 15_000);
    return () => {
      clearTimeout(kickoff);
      clearInterval(timer);
    };
  }, [refreshPrivate, selectedAccountId]);

  React.useEffect(() => {
    if (status !== "authenticated" || selectedAccountId == null) return;
    let cancelled = false;
    let portfolioTimer: ReturnType<typeof setTimeout> | null = null;
    const source = new EventSource(polymarketAccountStreamUrl(selectedAccountId), {
      withCredentials: true,
    });
    source.addEventListener("account", (message) => {
      if (cancelled) return;
      try {
        const event = mapPolymarketAccountEvent(
          JSON.parse((message as MessageEvent<string>).data),
        );
        if (event.type !== "heartbeat") {
          setOpenOrders(event.openOrders);
          setOpenOrdersStale(false);
          setOpenOrdersError(null);
        }
        if (event.portfolioChanged) {
          if (portfolioTimer) clearTimeout(portfolioTimer);
          portfolioTimer = setTimeout(() => void refreshPrivate(), 250);
        }
      } catch {
        setOpenOrdersError("账户订单事件解析失败");
      }
    });
    return () => {
      cancelled = true;
      source.close();
      if (portfolioTimer) clearTimeout(portfolioTimer);
    };
  }, [refreshPrivate, selectedAccountId, status]);

  const removeOpenOrder = React.useCallback((orderId: string) => {
    setOpenOrders((current) => current.filter((order) => order.id !== orderId));
  }, []);

  const visibleSnapshot =
    liveSnapshot != null &&
    selectedMarket != null &&
    liveSnapshot.market.id === selectedMarket.id &&
    liveSnapshot.market.asset === selectedAsset &&
    liveSnapshot.market.period === selectedPeriod
      ? liveSnapshot
      : null;
  const visibleAccountId =
    status === "authenticated" ? selectedAccountId : null;
  const value = React.useMemo<PolymarketContextValue>(() => {
    const visibleChartPoints = visibleSnapshot ? chartPoints : [];
    const marketAligned = visibleSnapshot != null && selectedMarket != null;
    const visibleFairPricePoints =
      marketAligned &&
      (fairPriceStatus === "live" || fairPriceStatus === "stale")
        ? filterFairPricePointsToWindow(
            fairPricePoints,
            selectedMarket.windowStart,
            selectedMarket.windowEnd,
            Date.parse(selectedMarket.windowEnd),
          )
        : [];
    const visibleLiveQuote = visibleSnapshot
      ? liveQuoteFromSnapshot(visibleSnapshot)
      : null;
    return {
      markets,
      selectedMarket,
      snapshot: visibleSnapshot,
      chartPoints: visibleChartPoints,
      liveQuote: visibleLiveQuote,
      currentFairPrice: marketAligned ? currentFairPrice : null,
      fairPricePoints: visibleFairPricePoints,
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
      accounts: status === "authenticated" ? accounts : [],
      selectedAccountId: visibleAccountId,
      selectAccount: setSelectedAccountId,
      summary: visibleAccountId == null ? null : summary,
      openOrders: visibleAccountId == null ? [] : openOrders,
      openOrdersStale,
      positions: visibleAccountId == null ? [] : positions,
      positionsStale,
      privateLoading,
      summaryError: visibleAccountId == null ? null : summaryError,
      openOrdersError: visibleAccountId == null ? null : openOrdersError,
      positionsError: visibleAccountId == null ? null : positionsError,
      refreshPrivate,
      removeOpenOrder,
      refreshAccounts,
      refreshMarkets: refreshMarketsOnly,
    };
  }, [
      markets,
      selectedMarket,
      visibleSnapshot,
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
      status,
      visibleAccountId,
      summary,
      openOrders,
      openOrdersStale,
      positions,
      positionsStale,
      privateLoading,
      summaryError,
      openOrdersError,
      positionsError,
      refreshPrivate,
      removeOpenOrder,
      refreshAccounts,
      refreshMarketsOnly,
    ]);

  return (
    <PolymarketContext.Provider value={value}>{children}</PolymarketContext.Provider>
  );
}

export function usePolymarket(): PolymarketContextValue {
  const value = React.useContext(PolymarketContext);
  if (!value) throw new Error("usePolymarket must be used within PolymarketProvider");
  return value;
}
