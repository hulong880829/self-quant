"use client";

import * as React from "react";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";

import {
  annualize24h,
  annualize7d,
  formatCompactNumber,
  formatCurrency,
  formatFundingRate,
  formatPercent,
  formatSettlementCountdown,
  rateColor,
  resolveFundingRate,
} from "@/lib/market-format";
import {
  estimateSingleFundingPayback,
  formatFundingPayback,
  fundingPaybackTitle,
} from "@/lib/funding-payback";
import { formatHistoryWindow } from "@/lib/funding-coverage";
import { cn } from "@/lib/utils";

export type FundingMetricRow = {
  positionQuantity: number;
  positionNotional: number;
  baseAsset: string;
  dailyVolume: number;
  annualizedRate: number;
  history7dComplete?: boolean;
  nextSettlementAt: string;
  settlementIntervalHours: number;
  nextFundingRate: number | null;
  currentFundingRate: number | null;
  cumulative24h: number;
  history24hComplete?: boolean;
  cumulative7d: number;
  stale?: boolean;
};

export const SettlementCountdown = React.memo(function SettlementCountdown({
  value,
}: {
  value: string;
}) {
  const [now, setNow] = React.useState(() => Date.now());
  React.useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1_000);
    return () => window.clearInterval(timer);
  }, []);
  return <>{formatSettlementCountdown(value, now)}</>;
});

export function fundingMetricColumns<T extends FundingMetricRow>(): LegacyColumnDef<T>[] {
  return [
    {
      accessorKey: "positionNotional",
      header: "持仓市值 (USDT)",
      cell: ({ row }) => (
        <div
          className="text-right"
          title={`${formatCompactNumber(row.original.positionQuantity)} ${row.original.baseAsset}`}
        >
          <div className="font-mono text-xs">
            {formatCurrency(row.original.positionNotional, true)}
          </div>
          <div className="mt-0.5 text-[10px] text-muted-foreground">USDT</div>
        </div>
      ),
    },
    {
      accessorKey: "dailyVolume",
      header: "日成交额",
      cell: ({ row }) => (
        <div className="text-right font-mono text-xs">
          {formatCurrency(row.original.dailyVolume, true)}
        </div>
      ),
    },
    {
      accessorKey: "annualizedRate",
      header: "历史年化费率",
      cell: ({ row }) => (
        <div
          className={cn(
            "text-right font-mono text-xs font-semibold",
            rateColor(row.original.annualizedRate),
          )}
        >
          {formatHistoryWindow(
            row.original.history7dComplete,
            row.original.annualizedRate,
            (value) => formatPercent(value, 1),
          )}
        </div>
      ),
    },
    {
      id: "settlementCountdown",
      accessorFn: (row) => Date.parse(row.nextSettlementAt),
      header: "距离结算",
      cell: ({ row }) => (
        <div className="text-right">
          <div className="font-mono text-xs">
            <SettlementCountdown value={row.original.nextSettlementAt} />
          </div>
          <div className="mt-0.5 text-[10px] text-muted-foreground">
            {row.original.settlementIntervalHours}H 周期
          </div>
        </div>
      ),
    },
    {
      id: "displayedFundingRate",
      accessorFn: (row) => resolveFundingRate(row.nextFundingRate, row.currentFundingRate),
      header: "资金费率",
      cell: ({ row }) => {
        const displayedRate = resolveFundingRate(
          row.original.nextFundingRate,
          row.original.currentFundingRate,
        );
        return (
          <div
            className={cn(
              "text-right font-mono text-xs font-semibold",
              rateColor(displayedRate),
            )}
          >
            {formatFundingRate(displayedRate)}
          </div>
        );
      },
    },
    {
      id: "annualized24h",
      accessorFn: (row) => annualize24h(row.cumulative24h),
      header: "24H 窗口年化",
      cell: ({ row }) => (
        <div
          className={cn(
            "text-right font-mono text-xs",
            rateColor(annualize24h(row.original.cumulative24h)),
          )}
        >
          {formatHistoryWindow(
            row.original.history24hComplete,
            annualize24h(row.original.cumulative24h),
            (value) => formatPercent(value, 1),
          )}
        </div>
      ),
    },
    {
      id: "annualized7d",
      accessorFn: (row) => annualize7d(row.cumulative7d),
      header: "7D 窗口年化",
      cell: ({ row }) => (
        <div
          className={cn(
            "text-right font-mono text-xs",
            rateColor(annualize7d(row.original.cumulative7d)),
          )}
        >
          {formatHistoryWindow(
            row.original.history7dComplete,
            annualize7d(row.original.cumulative7d),
            (value) => formatPercent(value, 1),
          )}
        </div>
      ),
    },
    {
      id: "fundingPayback",
      accessorFn: (row) =>
        estimateSingleFundingPayback({
          cumulative24h: row.cumulative24h,
          stale: row.stale,
        }).paybackMinutes ?? Number.POSITIVE_INFINITY,
      header: "预计回本时间",
      cell: ({ row }) => {
        const estimate = estimateSingleFundingPayback({
          cumulative24h: row.original.cumulative24h,
          stale: row.original.stale,
        });
        return (
          <div className="text-right font-mono text-xs" title={fundingPaybackTitle(estimate)}>
            {formatFundingPayback(estimate)}
          </div>
        );
      },
    },
  ];
}
