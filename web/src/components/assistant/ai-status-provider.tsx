"use client";

import * as React from "react";

import { useAuth } from "@/components/auth/auth-provider";
import {
  fetchAICredential,
  type AICredential,
} from "@/lib/api/ai";

export type AIAvailability =
  | "loading"
  | "anonymous"
  | "unconfigured"
  | "unknown"
  | "valid"
  | "invalid"
  | "error";

interface AIStatusContextValue {
  availability: AIAvailability;
  credential: AICredential | null;
  error: string;
  refresh: () => Promise<AICredential | null>;
}

const AIStatusContext = React.createContext<AIStatusContextValue | null>(null);

export function AIStatusProvider({ children }: { children: React.ReactNode }) {
  const { status: authStatus, user } = useAuth();
  const [credential, setCredential] = React.useState<AICredential | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState("");

  const refresh = React.useCallback(async () => {
    if (authStatus !== "authenticated") {
      setCredential(null);
      setLoading(false);
      setError("");
      return null;
    }
    setLoading(true);
    setError("");
    try {
      const next = await fetchAICredential();
      setCredential(next);
      return next;
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "无法读取 AI Key 状态");
      return null;
    } finally {
      setLoading(false);
    }
  }, [authStatus]);

  React.useEffect(() => {
    let cancelled = false;
    if (authStatus !== "authenticated") return;
    (async () => {
      try {
        const next = await fetchAICredential();
        if (!cancelled) {
          setCredential(next);
          setError("");
          setLoading(false);
        }
      } catch (reason) {
        if (!cancelled) {
          setCredential(null);
          setError(
            reason instanceof Error ? reason.message : "无法读取 AI Key 状态",
          );
          setLoading(false);
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [authStatus, user?.username]);

  let availability: AIAvailability;
  if (authStatus === "loading") availability = "loading";
  else if (authStatus !== "authenticated") availability = "anonymous";
  else if (loading) availability = "loading";
  else if (error) availability = "error";
  else if (!credential) availability = "unconfigured";
  else availability = credential.status;

  const value = React.useMemo<AIStatusContextValue>(
    () => ({ availability, credential, error, refresh }),
    [availability, credential, error, refresh],
  );

  return (
    <AIStatusContext.Provider value={value}>
      {children}
    </AIStatusContext.Provider>
  );
}

export function useAIStatus(): AIStatusContextValue {
  const value = React.useContext(AIStatusContext);
  if (!value) {
    throw new Error("useAIStatus must be used within AIStatusProvider");
  }
  return value;
}
