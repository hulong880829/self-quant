"use client";

import * as React from "react";

import {
  fetchFundingRates,
  fetchFundingSpreads,
  type FundingSnapshot,
  type FundingSpreadSnapshot,
} from "@/lib/api/funding";

interface FundingContextValue {
  snapshot: FundingSnapshot | null;
  spreadSnapshot: FundingSpreadSnapshot | null;
  loading: boolean;
  refreshing: boolean;
  error: string | null;
  lastSuccessAt: number | null;
  retry: () => void;
}

const FundingContext = React.createContext<FundingContextValue | null>(null);
const REFRESH_INTERVAL_MS = 60_000;

export function FundingProvider({ children }: { children: React.ReactNode }) {
  const [snapshot, setSnapshot] = React.useState<FundingSnapshot | null>(null);
  const [spreadSnapshot, setSpreadSnapshot] =
    React.useState<FundingSpreadSnapshot | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [refreshing, setRefreshing] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [lastSuccessAt, setLastSuccessAt] = React.useState<number | null>(null);
  const activeRequest = React.useRef<AbortController | null>(null);
  const snapshotVersion = React.useRef<string | null>(null);
  const spreadSnapshotVersion = React.useRef<string | null>(null);
  const etag = React.useRef<string | null>(null);
  const spreadEtag = React.useRef<string | null>(null);

  const load = React.useCallback(async () => {
    if (activeRequest.current) return;
    const controller = new AbortController();
    activeRequest.current = controller;
    setRefreshing(true);

    try {
      const [ratesResult, spreadsResult] = await Promise.allSettled([
        fetchFundingRates(etag.current, controller.signal),
        fetchFundingSpreads(spreadEtag.current, controller.signal),
      ]);
      if (ratesResult.status === "rejected") {
        throw ratesResult.reason;
      }
      etag.current = ratesResult.value.etag;
      let changed = false;
      if (ratesResult.value.status === "updated") {
        const nextSnapshot = ratesResult.value.snapshot;
        if (snapshotVersion.current !== nextSnapshot.meta.snapshotVersion) {
          snapshotVersion.current = nextSnapshot.meta.snapshotVersion;
          setSnapshot(nextSnapshot);
          changed = true;
        }
      }
      if (spreadsResult.status === "fulfilled") {
        spreadEtag.current = spreadsResult.value.etag;
        if (spreadsResult.value.status === "updated") {
          const nextSnapshot = spreadsResult.value.snapshot;
          if (spreadSnapshotVersion.current !== nextSnapshot.meta.snapshotVersion) {
            spreadSnapshotVersion.current = nextSnapshot.meta.snapshotVersion;
            setSpreadSnapshot(nextSnapshot);
            changed = true;
          }
        }
      }
      if (changed) {
        setLastSuccessAt(Date.now());
      }
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
    const timer = window.setInterval(() => void load(), REFRESH_INTERVAL_MS);
    return () => {
      window.clearTimeout(initialTimer);
      window.clearInterval(timer);
      activeRequest.current?.abort();
    };
  }, [load]);

  const value = React.useMemo(
    () => ({
      snapshot,
      spreadSnapshot,
      loading,
      refreshing,
      error,
      lastSuccessAt,
      retry: () => void load(),
    }),
    [error, lastSuccessAt, load, loading, refreshing, snapshot, spreadSnapshot],
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
