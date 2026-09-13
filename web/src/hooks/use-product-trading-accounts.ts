"use client";

import * as React from "react";

import { isAbortError } from "@/lib/abort";
import {
  applyTradingReadiness,
  fetchTradingAccounts,
  groupAccountsByProduct,
  inspectTradingReadiness,
  isWalletDexExchange,
  type ProductGroup,
  type TradingAccount,
} from "@/lib/api/accounts";

function overlayReadiness(
  items: TradingAccount[],
  readinessById: Map<number, TradingAccount>,
): TradingAccount[] {
  return items.map((item) => {
    if (!isWalletDexExchange(item.exchangeSlug || item.exchange)) {
      return item;
    }
    const cached = readinessById.get(item.id);
    if (cached) {
      return applyTradingReadiness(item, cached);
    }
    return applyTradingReadiness(item, {
      tradingReady: false,
      tradingStatus: "checking",
      tradingUnavailableCode: "",
      tradingUnavailableReason: "",
    });
  });
}

export function useProductTradingAccounts() {
  const [accounts, setAccounts] = React.useState<TradingAccount[]>([]);
  const [accountsError, setAccountsError] = React.useState<string | null>(null);
  const [inspecting, setInspecting] = React.useState(false);
  const inspectedIdsRef = React.useRef(new Set<number>());
  const readinessByIdRef = React.useRef(new Map<number, TradingAccount>());
  const walletInspectStartedRef = React.useRef(false);

  const inspectOne = React.useCallback(async (id: number) => {
    const ready = await inspectTradingReadiness(id);
    setAccounts((current) => {
      const next = current.map((item) =>
        item.id === id ? applyTradingReadiness(item, ready) : item,
      );
      const updated = next.find((item) => item.id === id);
      if (updated) {
        inspectedIdsRef.current.add(id);
        readinessByIdRef.current.set(id, updated);
      }
      return next;
    });
    return ready;
  }, []);

  React.useEffect(() => {
    const controller = new AbortController();
    let cancelled = false;
    void (async () => {
      try {
        const items = await fetchTradingAccounts(controller.signal);
        if (cancelled) return;
        setAccounts(overlayReadiness(items, readinessByIdRef.current));
        setAccountsError(null);
        const toInspect = items.filter(
          (item) =>
            isWalletDexExchange(item.exchangeSlug || item.exchange) &&
            !inspectedIdsRef.current.has(item.id),
        );
        if (walletInspectStartedRef.current || toInspect.length === 0) {
          return;
        }
        walletInspectStartedRef.current = true;
        setInspecting(true);
        const inspected = await Promise.all(
          toInspect.map(async (item) => {
            inspectedIdsRef.current.add(item.id);
            try {
              const ready = await inspectTradingReadiness(
                item.id,
                controller.signal,
              );
              return applyTradingReadiness(item, ready);
            } catch (error) {
              if (cancelled || controller.signal.aborted || isAbortError(error)) {
                throw error;
              }
              return applyTradingReadiness(item, {
                tradingReady: false,
                tradingStatus: "venue_unavailable",
                tradingUnavailableCode: "venue_unavailable",
                tradingUnavailableReason:
                  error instanceof Error ? error.message : "交易能力检查失败",
              });
            }
          }),
        );
        if (cancelled || controller.signal.aborted) return;
        for (const account of inspected) {
          readinessByIdRef.current.set(account.id, account);
        }
        setAccounts((current) =>
          current.map(
            (item) => inspected.find((account) => account.id === item.id) ?? item,
          ),
        );
      } catch (error: unknown) {
        if (cancelled || controller.signal.aborted || isAbortError(error)) return;
        setAccountsError(
          error instanceof Error ? error.message : "账户加载失败",
        );
      } finally {
        if (!cancelled && !controller.signal.aborted) {
          setInspecting(false);
        }
      }
    })();
    return () => {
      cancelled = true;
      controller.abort();
    };
  }, []);

  const products: ProductGroup[] = React.useMemo(
    () => groupAccountsByProduct(accounts),
    [accounts],
  );

  return {
    accounts,
    products,
    accountsError,
    inspecting,
    inspectOne,
  };
}
