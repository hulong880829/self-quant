"use client";

import * as React from "react";

import {
  fetchFundingRates,
  type FundingSnapshot,
} from "@/lib/api/funding";

interface FundingContextValue {
  snapshot: FundingSnapshot | null;
  loading: boolean;
  refreshing: boolean;
  error: string | null;
  lastSuccessAt: number | null;
  retry: () => void;
}

const FundingContext = React.createContext<FundingContextValue | null>(null);

export function FundingProvider({ children }: { children: React.ReactNode }) {
  const [snapshot, setSnapshot] = React.useState<FundingSnapshot | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [refreshing, setRefreshing] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [lastSuccessAt, setLastSuccessAt] = React.useState<number | null>(null);
  const activeRequest = React.useRef<AbortController | null>(null);

  const load = React.useCallback(async () => {
    if (activeRequest.current) return;
    const controller = new AbortController();
    activeRequest.current = controller;
    setRefreshing(true);

    try {
      const nextSnapshot = await fetchFundingRates(controller.signal);
      setSnapshot(nextSnapshot);
      setLastSuccessAt(Date.now());
      setError(null);
    } catch (reason) {
      if (controller.signal.aborted) return;
      setError(reason instanceof Error ? reason.message : "资金费数据加载失败");
    } finally {
      if (activeRequest.current === controller) {
        activeRequest.current = null;
        setLoading(false);
        setRefreshing(false);
      }
    }
  }, []);

  React.useEffect(() => {
    const initialTimer = window.setTimeout(() => void load(), 0);
    const timer = window.setInterval(() => void load(), 10_000);
    return () => {
      window.clearTimeout(initialTimer);
      window.clearInterval(timer);
      activeRequest.current?.abort();
    };
  }, [load]);

  const value = React.useMemo(
    () => ({
      snapshot,
      loading,
      refreshing,
      error,
      lastSuccessAt,
      retry: () => void load(),
    }),
    [error, lastSuccessAt, load, loading, refreshing, snapshot],
  );

  return (
    <FundingContext.Provider value={value}>{children}</FundingContext.Provider>
  );
}

export function useFundingSnapshot() {
  const value = React.useContext(FundingContext);
  if (!value) {
    throw new Error("useFundingSnapshot 必须在 FundingProvider 内使用");
  }
  return value;
}
