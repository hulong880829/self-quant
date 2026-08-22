"use client";

import * as React from "react";

import {
  aggdataWebSocketUrl,
  decodeAggdataFrame,
  fetchAggdataHistory,
  fetchAggdataMarkets,
  fetchAggdataSnapshot,
  type AggdataFrame,
  type AggdataHistory,
  type AggdataMarket,
  type AggdataSnapshot,
} from "@/lib/api/aggdata";
import {
  chooseVisibleIncrement,
  generate125Increments,
  inferEffectiveTick,
  type AggregatedBookLevel,
  type FixedDecimal,
  type SpreadPoint,
} from "@/lib/orderbook";

type ConnectionState = "idle" | "connecting" | "live" | "reconnecting";

interface OrderbookContextValue {
  markets: AggdataMarket[];
  selectedSymbol: string;
  selectMarket: (symbol: string) => void;
  levels: AggregatedBookLevel[];
  automaticIncrement: FixedDecimal | null;
  history: SpreadPoint[];
  distribution: number[];
  historyCoverage: number;
  historyGapCount: number;
  historyStartMs: number | null;
  historyEndMs: number | null;
  loading: boolean;
  error: string | null;
  historyLoading: boolean;
  historyError: string | null;
  connection: ConnectionState;
  stale: boolean;
  lastUpdateAt: number | null;
  retry: () => void;
}

const OrderbookContext = React.createContext<OrderbookContextValue | null>(null);
const STALE_AFTER_MS = 15_000;
const MARKETS_CACHE_TTL_MS = 60_000;
const HISTORY_CACHE_TTL_MS = 30_000;
export const HISTORY_REFRESH_INTERVAL_MS = 60_000;

interface TimedCache<T> {
  value: T;
  storedAt: number;
}

let marketsCache: TimedCache<AggdataMarket[]> | null = null;
const historyCache = new Map<string, TimedCache<AggdataHistory>>();

function freshCacheValue<T>(
  cached: TimedCache<T> | null | undefined,
  ttlMs: number,
  now = Date.now(),
) {
  return cached && now - cached.storedAt <= ttlMs ? cached.value : null;
}

export function isNewerBookVersion(
  generation: bigint,
  sequence: bigint,
  currentGeneration: bigint,
  currentSequence: bigint,
) {
  return generation !== currentGeneration || sequence > currentSequence;
}

export function clearOrderbookCachesForTest() {
  marketsCache = null;
  historyCache.clear();
}

export function OrderbookProvider({ children }: { children: React.ReactNode }) {
  const [markets, setMarkets] = React.useState<AggdataMarket[]>([]);
  const [selectedSymbol, setSelectedSymbol] = React.useState("");
  const [levels, setLevels] = React.useState<AggregatedBookLevel[]>([]);
  const [automaticIncrement, setAutomaticIncrement] =
    React.useState<FixedDecimal | null>(null);
  const [history, setHistory] = React.useState<SpreadPoint[]>([]);
  const [distribution, setDistribution] = React.useState<number[]>([]);
  const [historyCoverage, setHistoryCoverage] = React.useState(0);
  const [historyGapCount, setHistoryGapCount] = React.useState(0);
  const [historyStartMs, setHistoryStartMs] = React.useState<number | null>(null);
  const [historyEndMs, setHistoryEndMs] = React.useState<number | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [historyLoading, setHistoryLoading] = React.useState(true);
  const [historyError, setHistoryError] = React.useState<string | null>(null);
  const [connection, setConnection] = React.useState<ConnectionState>("idle");
  const [lastUpdateAt, setLastUpdateAt] = React.useState<number | null>(null);
  const [clock, setClock] = React.useState(0);
  const [generation, setGeneration] = React.useState(0);
  const sequence = React.useRef<bigint>(0n);
  const bookGeneration = React.useRef<bigint>(0n);

  React.useEffect(() => {
    const controller = new AbortController();
    const applyMarkets = (nextMarkets: AggdataMarket[]) => {
      setMarkets(nextMarkets);
      setSelectedSymbol((current) =>
        nextMarkets.some((market) => market.symbol === current)
          ? current
          : (nextMarkets[0]?.symbol ?? ""),
      );
      if (nextMarkets.length === 0) setLoading(false);
      setError(nextMarkets.length === 0 ? "aggdata 未返回可用市场" : null);
    };
    const cachedMarkets = freshCacheValue(
      marketsCache,
      MARKETS_CACHE_TTL_MS,
    );
    if (cachedMarkets) applyMarkets(cachedMarkets);

    void fetchAggdataMarkets(controller.signal)
      .then((nextMarkets) => {
        marketsCache = { value: nextMarkets, storedAt: Date.now() };
        applyMarkets(nextMarkets);
      })
      .catch((reason) => {
        if (!controller.signal.aborted && !cachedMarkets) {
          setError(reason instanceof Error ? reason.message : "市场列表加载失败");
          setLoading(false);
        }
      });
    return () => controller.abort();
  }, [generation]);

  React.useEffect(() => {
    if (!selectedSymbol) return;
    const market = markets.find((item) => item.symbol === selectedSymbol);
    if (!market) return;

    let disposed = false;
    let socket: WebSocket | null = null;
    let reconnectTimer: number | null = null;
    let frameRequest: number | null = null;
    let pendingFrame: AggdataFrame | null = null;
    let snapshotController: AbortController | null = null;
    let retryCount = 0;
    const historyController = new AbortController();
    let historyRequestInFlight = false;
    let historyRefreshTimer: number | null = null;

    const updateAutomaticIncrement = (nextLevels: AggregatedBookLevel[]) => {
      const tick = inferEffectiveTick(nextLevels);
      const increments = tick ? generate125Increments(tick) : [];
      setAutomaticIncrement(
        increments.length > 0
          ? chooseVisibleIncrement(nextLevels, increments)
          : null,
      );
    };

    const applySnapshot = (snapshot: AggdataSnapshot) => {
      if (disposed) return;
      sequence.current = snapshot.sequence;
      bookGeneration.current = snapshot.generation;
      setLevels(snapshot.levels);
      updateAutomaticIncrement(snapshot.levels);
      setLastUpdateAt(Date.parse(snapshot.timestamp));
      setError(null);
      setLoading(false);
    };

    const loadSnapshot = async () => {
      snapshotController?.abort();
      const nextController = new AbortController();
      snapshotController = nextController;
      const snapshot = await fetchAggdataSnapshot(
        market,
        nextController.signal,
      );
      if (
        snapshotController !== nextController ||
        nextController.signal.aborted
      ) return;
      snapshotController = null;
      applySnapshot(snapshot);
    };

    const applyHistory = (nextHistory: AggdataHistory) => {
      setHistory(nextHistory.points);
      setDistribution(nextHistory.distribution);
      setHistoryCoverage(nextHistory.coverage);
      setHistoryGapCount(nextHistory.gapCount);
      setHistoryStartMs(nextHistory.startMs);
      setHistoryEndMs(nextHistory.endMs);
    };
    const cachedHistory = freshCacheValue(
      historyCache.get(market.symbol),
      HISTORY_CACHE_TTL_MS,
    );
    if (cachedHistory) {
      applyHistory(cachedHistory);
      setHistoryLoading(false);
    } else {
      setHistoryLoading(true);
    }
    const loadHistory = async () => {
      if (historyRequestInFlight || disposed) return;
      historyRequestInFlight = true;
      try {
        const nextHistory = await fetchAggdataHistory(
          market.symbol,
          historyController.signal,
        );
        if (disposed) return;
        historyCache.set(market.symbol, {
          value: nextHistory,
          storedAt: Date.now(),
        });
        applyHistory(nextHistory);
        setHistoryError(null);
      } catch (reason) {
        if (!historyController.signal.aborted && !disposed) {
          setHistoryError(
            reason instanceof Error ? reason.message : "24H 历史加载失败",
          );
        }
      } finally {
        historyRequestInFlight = false;
        if (!disposed) setHistoryLoading(false);
      }
    };
    void loadHistory();
    historyRefreshTimer = window.setInterval(() => {
      if (document.visibilityState === "visible") void loadHistory();
    }, HISTORY_REFRESH_INTERVAL_MS);
    const refreshHistoryWhenVisible = () => {
      if (document.visibilityState === "visible") void loadHistory();
    };
    document.addEventListener("visibilitychange", refreshHistoryWhenVisible);

    const cancelPendingFrame = () => {
      pendingFrame = null;
      if (frameRequest !== null) {
        window.cancelAnimationFrame(frameRequest);
        frameRequest = null;
      }
    };

    const queueFrame = (frame: AggdataFrame) => {
      if (
        !isNewerBookVersion(
          frame.generation,
          frame.sequence,
          bookGeneration.current,
          sequence.current,
        )
      ) {
        return;
      }
      if (
        pendingFrame &&
        !isNewerBookVersion(
          frame.generation,
          frame.sequence,
          pendingFrame.generation,
          pendingFrame.sequence,
        )
      ) {
        return;
      }
      pendingFrame = frame;
      if (frameRequest !== null) return;
      frameRequest = window.requestAnimationFrame(() => {
        frameRequest = null;
        const latest = pendingFrame;
        pendingFrame = null;
        if (
          disposed ||
          !latest ||
          !isNewerBookVersion(
            latest.generation,
            latest.sequence,
            bookGeneration.current,
            sequence.current,
          )
        ) {
          return;
        }
        const generationChanged =
          latest.generation !== bookGeneration.current;
        bookGeneration.current = latest.generation;
        sequence.current = latest.sequence;
        React.startTransition(() => {
          setLevels(latest.levels);
          if (generationChanged) updateAutomaticIncrement(latest.levels);
          setLastUpdateAt(Number(latest.timestampMs));
        });
        setConnection("live");
        setError(null);
      });
    };

    const scheduleReconnect = () => {
      if (disposed) return;
      setConnection("reconnecting");
      const delay = Math.min(10_000, 500 * 2 ** retryCount);
      retryCount += 1;
      reconnectTimer = window.setTimeout(connect, delay);
    };

    const reset = async (message: string) => {
      const activeSocket = socket;
      socket = null;
      activeSocket?.close();
      cancelPendingFrame();
      setConnection("reconnecting");
      try {
        await loadSnapshot();
        if (!disposed) connect();
      } catch (reason) {
        if (!disposed && !snapshotController?.signal.aborted) {
          setError(reason instanceof Error ? reason.message : message);
          const delay = Math.min(10_000, 500 * 2 ** retryCount);
          retryCount += 1;
          reconnectTimer = window.setTimeout(() => void reset(message), delay);
        }
      }
    };

    const connect = () => {
      if (disposed || socket) return;
      setConnection(retryCount === 0 ? "connecting" : "reconnecting");
      const ws = new WebSocket(aggdataWebSocketUrl());
      socket = ws;
      ws.binaryType = "arraybuffer";
      ws.onopen = () => {
        if (socket !== ws) return;
        retryCount = 0;
        setConnection("live");
        ws.send(
          JSON.stringify({
            op: "subscribe",
            symbol: market.symbol,
            channel: "orderbook",
            depth: 50,
          }),
        );
      };
      ws.onmessage = (event) => {
        if (socket !== ws) return;
        try {
          if (!(event.data instanceof ArrayBuffer)) {
            const acknowledgement = JSON.parse(String(event.data)) as {
              ok?: boolean;
              op?: string;
              error?: string;
            };
            if (acknowledgement.op === "reset") {
              void reset("服务端要求重置盘口");
              return;
            }
            if (!acknowledgement.ok) {
              throw new Error(acknowledgement.error ?? "aggdata 订阅被拒绝");
            }
            return;
          }
          const frame = decodeAggdataFrame(event.data);
          queueFrame(frame);
        } catch (reason) {
          setError(reason instanceof Error ? reason.message : "实时盘口解码失败");
          void reset("实时盘口解码失败");
        }
      };
      ws.onerror = () => ws.close();
      ws.onclose = () => {
        if (socket !== ws) return;
        socket = null;
        scheduleReconnect();
      };
    };

    void loadSnapshot()
      .then(connect)
      .catch((reason) => {
        if (!disposed && !snapshotController?.signal.aborted) {
          setError(reason instanceof Error ? reason.message : "盘口快照加载失败");
          setLoading(false);
          const delay = Math.min(10_000, 500 * 2 ** retryCount);
          retryCount += 1;
          reconnectTimer = window.setTimeout(
            () => void reset("盘口快照加载失败"),
            delay,
          );
        }
      });

    return () => {
      disposed = true;
      snapshotController?.abort();
      historyController.abort();
      document.removeEventListener("visibilitychange", refreshHistoryWhenVisible);
      if (historyRefreshTimer !== null) {
        window.clearInterval(historyRefreshTimer);
      }
      cancelPendingFrame();
      if (reconnectTimer !== null) window.clearTimeout(reconnectTimer);
      const activeSocket = socket;
      socket = null;
      activeSocket?.close();
    };
  }, [markets, selectedSymbol, generation]);

  React.useEffect(() => {
    const timer = window.setInterval(() => setClock(Date.now()), 1_000);
    return () => window.clearInterval(timer);
  }, []);

  const resetDisplay = React.useCallback(() => {
    setLoading(true);
    setLevels([]);
    setAutomaticIncrement(null);
    setHistory([]);
    setDistribution([]);
    setHistoryCoverage(0);
    setHistoryGapCount(0);
    setHistoryStartMs(null);
    setHistoryEndMs(null);
    setError(null);
    setHistoryLoading(true);
    setHistoryError(null);
    setConnection("idle");
  }, []);
  const selectMarket = React.useCallback((symbol: string) => {
    historyCache.delete(symbol);
    resetDisplay();
    setSelectedSymbol(symbol);
  }, [resetDisplay]);
  const retry = React.useCallback(() => {
    historyCache.delete(selectedSymbol);
    resetDisplay();
    setGeneration((value) => value + 1);
  }, [resetDisplay, selectedSymbol]);

  const value = React.useMemo<OrderbookContextValue>(
    () => ({
      markets,
      selectedSymbol,
      selectMarket,
      levels,
      automaticIncrement,
      history,
      distribution,
      historyCoverage,
      historyGapCount,
      historyStartMs,
      historyEndMs,
      loading,
      error,
      historyLoading,
      historyError,
      connection,
      stale: lastUpdateAt !== null && clock - lastUpdateAt > STALE_AFTER_MS,
      lastUpdateAt,
      retry,
    }),
    [
      clock,
      connection,
      error,
      history,
      historyCoverage,
      historyGapCount,
      historyStartMs,
      historyEndMs,
      historyError,
      historyLoading,
      distribution,
      lastUpdateAt,
      levels,
      automaticIncrement,
      loading,
      markets,
      selectedSymbol,
      selectMarket,
      retry,
    ],
  );

  return (
    <OrderbookContext.Provider value={value}>
      {children}
    </OrderbookContext.Provider>
  );
}

export function useOrderbook() {
  const value = React.useContext(OrderbookContext);
  if (!value) throw new Error("useOrderbook must be used inside OrderbookProvider");
  return value;
}
