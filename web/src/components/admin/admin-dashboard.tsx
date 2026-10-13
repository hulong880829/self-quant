"use client";

import * as React from "react";

import { PageFrame, WorkspacePanel } from "@/components/layout/responsive";
import { BrokerTable } from "@/components/admin/broker-table";
import { PlatformAccountTable } from "@/components/admin/platform-account-table";
import {
  fetchAdminBrokers,
  fetchAdminOverview,
  fetchAdminPlatformAccounts,
  type Broker,
  type OverviewRow,
  type PlatformAccount,
} from "@/lib/api/admin";

function formatNumber(value?: number) {
  return new Intl.NumberFormat("en-US").format(value ?? 0);
}

function OverviewCards({ row }: { row?: OverviewRow }) {
  const items = [
    ["平台账户", formatNumber(row?.platformAccounts)],
    ["运行中", formatNumber(row?.running)],
    ["平仓中", formatNumber(row?.closing)],
    ["权益 USD", row?.equityUsd?.trim() || "—"],
    ["覆盖账户", formatNumber(row?.coveredAccounts)],
    ["失败快照", formatNumber(row?.failedSnapshots)],
  ];
  return (
    <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-6">
      {items.map(([label, value]) => (
        <div key={label} className="rounded-xl border bg-card p-4 shadow-sm">
          <div className="text-xs text-muted-foreground">{label}</div>
          <div className="mt-2 text-xl font-semibold tracking-tight">{value}</div>
        </div>
      ))}
    </div>
  );
}

type AdminData = {
  overview: OverviewRow;
  brokers: Broker[];
  accounts: PlatformAccount[];
};

function fetchAdminData(signal?: AbortSignal) {
  return Promise.all([
    fetchAdminOverview(signal),
    fetchAdminBrokers(signal),
    fetchAdminPlatformAccounts(signal),
  ]).then(([nextOverview, nextBrokers, nextAccounts]): AdminData => ({
    overview: nextOverview.platform,
    brokers: nextBrokers,
    accounts: nextAccounts,
  }));
}

export function AdminDashboard() {
  const [overview, setOverview] = React.useState<OverviewRow>();
  const [brokers, setBrokers] = React.useState<Broker[]>([]);
  const [accounts, setAccounts] = React.useState<PlatformAccount[]>([]);
  const [error, setError] = React.useState("");

  const applyAdminData = React.useCallback((data: AdminData) => {
    setOverview(data.overview);
    setBrokers(data.brokers);
    setAccounts(data.accounts);
  }, []);

  const reload = React.useCallback(async (signal?: AbortSignal) => {
    applyAdminData(await fetchAdminData(signal));
  }, [applyAdminData]);

  React.useEffect(() => {
    const controller = new AbortController();
    fetchAdminData(controller.signal).then(
      (data) => {
        if (!controller.signal.aborted) applyAdminData(data);
      },
      (err: unknown) => {
        if (!controller.signal.aborted) {
          setError(err instanceof Error ? err.message : "加载管理数据失败");
        }
      },
    );
    return () => controller.abort();
  }, [applyAdminData]);

  return (
    <PageFrame>
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">管理后台</h1>
        <p className="mt-1 text-sm text-muted-foreground">
          平台账户、Broker 与套利覆盖总览。禁用 Broker 或账户不会自动停止已有套利。
        </p>
      </div>
      {error ? (
        <div className="rounded-xl border border-destructive/30 bg-destructive/5 px-4 py-3 text-sm text-destructive">
          {error}
        </div>
      ) : null}
      <OverviewCards row={overview} />
      <WorkspacePanel className="h-auto max-h-none">
        <BrokerTable
          brokers={brokers}
          onChanged={async () => {
            setError("");
            try {
              await reload();
            } catch (err) {
              setError(err instanceof Error ? err.message : "刷新失败");
            }
          }}
        />
      </WorkspacePanel>
      <WorkspacePanel className="h-auto max-h-none">
        <PlatformAccountTable
          accounts={accounts}
          brokers={brokers}
          onChanged={async () => {
            setError("");
            try {
              await reload();
            } catch (err) {
              setError(err instanceof Error ? err.message : "刷新失败");
            }
          }}
        />
      </WorkspacePanel>
    </PageFrame>
  );
}
