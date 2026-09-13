"use client";

import * as React from "react";
import {
  ChevronDown,
  ChevronRight,
  Plus,
  RefreshCw,
  Trash2,
  WalletCards,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import { PageFrame, WideTableScroll, WorkspacePanel } from "@/components/layout/responsive";
import { Input } from "@/components/ui/input";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import {
  applyTradingAccountProfile,
  createTradingAccount,
  deleteTradingAccount,
  fetchProductGroupSnapshot,
  fetchTradingAccountSnapshot,
  fetchTradingAccounts,
  groupAccountsByProduct,
  isWalletDexExchange,
  formatAccountFeeSummary,
  type PortfolioPosition,
  type ProductGroupSnapshot,
  type TradingAccountSnapshot,
  type ProductGroup,
  type TradingAccount,
  type AccountProfileResult,
} from "@/lib/api/accounts";
import { formatCurrency } from "@/lib/market-format";
import { cn } from "@/lib/utils";
import type { Exchange } from "@/types/market";

function formatPlainNumber(value: number) {
  return new Intl.NumberFormat("en-US", {
    maximumFractionDigits: 4,
  }).format(value);
}

function formatDecimalCurrency(value?: string) {
  if (!value?.trim()) return "—";
  const number = Number(value);
  return Number.isFinite(number) ? formatCurrency(number) : "—";
}

function formatDecimal(value?: string) {
  if (!value?.trim()) return "—";
  const number = Number(value);
  return Number.isFinite(number) ? formatPlainNumber(number) : "—";
}

function formatSignedDecimal(value?: string) {
  if (!value?.trim()) return "0";
  const number = Number(value);
  if (!Number.isFinite(number)) return "—";
  if (number === 0) return "0";
  return `${number > 0 ? "+" : ""}${formatPlainNumber(number)}`;
}

const exchangeOptions: Exchange[] = [
  "Binance",
  "OKX",
  "Bybit",
  "Bitget",
  "Gate",
  "Hyperliquid",
  "Aster",
  "Lighter",
  "Polymarket",
];

const accountProfileExchanges = new Set([
  "binance",
  "okx",
  "bybit",
  "bitget",
  "gate",
]);

function MetricHeader({
  label,
  value,
  refreshable = false,
}: {
  label: string;
  value: string;
  refreshable?: boolean;
}) {
  return (
    <div className="min-w-0">
      <div className="flex items-center gap-1.5 text-[11px] text-muted-foreground">
        <span>{label}</span>
        {refreshable ? <RefreshCw className="size-3 opacity-70" /> : null}
      </div>
      <div className="mt-1 font-mono text-sm tabular-nums text-foreground">
        {value}
      </div>
    </div>
  );
}

export function AccountDashboard() {
  const [accounts, setAccounts] = React.useState<TradingAccount[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [selection, setSelection] = React.useState<
    { type: "account"; id: number } | { type: "group"; productName: string } | null
  >(null);
  const [snapshot, setSnapshot] = React.useState<
    TradingAccountSnapshot | ProductGroupSnapshot | null
  >(null);
  const [snapshotLoading, setSnapshotLoading] = React.useState(false);
  const [snapshotError, setSnapshotError] = React.useState<string | null>(null);
  const snapshotCache = React.useRef(
    new Map<string, TradingAccountSnapshot | ProductGroupSnapshot>(),
  );
  const [expanded, setExpanded] = React.useState<Record<string, boolean>>({});
  const [addOpen, setAddOpen] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [profileBusyId, setProfileBusyId] = React.useState<number | null>(null);
  const [profileResults, setProfileResults] = React.useState<
    Record<number, AccountProfileResult>
  >({});
  const [profileErrors, setProfileErrors] = React.useState<Record<number, string>>({});

  const [productName, setProductName] = React.useState("");
  const [exchange, setExchange] = React.useState<Exchange>("Binance");
  const [accountName, setAccountName] = React.useState("");
  const [apiKey, setApiKey] = React.useState("");
  const [apiSecret, setApiSecret] = React.useState("");
  const [passphrase, setPassphrase] = React.useState("");
  const [privateKey, setPrivateKey] = React.useState("");
  const [walletAddress, setWalletAddress] = React.useState("");
  const [vaultAddress, setVaultAddress] = React.useState("");
  const [walletType, setWalletType] = React.useState<"eoa" | "deposit">("eoa");
  const [funderAddress, setFunderAddress] = React.useState("");
  const [formError, setFormError] = React.useState<string | null>(null);

  const applyAccounts = React.useCallback(
    (next: TradingAccount[], preferredId?: number | null) => {
      setAccounts(next);
      const nextGroups = groupAccountsByProduct(next);
      setExpanded((current) => {
        const updated = { ...current };
        for (const group of nextGroups) {
          if (updated[group.productName] === undefined) {
            updated[group.productName] = true;
          }
        }
        return updated;
      });
      setSelection((current) => {
        if (preferredId != null && next.some((item) => item.id === preferredId)) {
          return { type: "account", id: preferredId };
        }
        if (
          current?.type === "account" &&
          next.some((item) => item.id === current.id)
        ) {
          return current;
        }
        if (
          current?.type === "group" &&
          next.some((item) => item.productName === current.productName)
        ) {
          return current;
        }
        return next[0] ? { type: "account", id: next[0].id } : null;
      });
    },
    [],
  );

  const reload = React.useCallback(
    async (preferredId?: number | null) => {
      setLoading(true);
      setError(null);
      try {
        const next = await fetchTradingAccounts();
        applyAccounts(next, preferredId);
      } catch (err) {
        setError(err instanceof Error ? err.message : "加载交易账户失败");
      } finally {
        setLoading(false);
      }
    },
    [applyAccounts],
  );

  React.useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const next = await fetchTradingAccounts();
        if (cancelled) return;
        applyAccounts(next);
        setLoading(false);
      } catch (err) {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : "加载交易账户失败");
        setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [applyAccounts]);

  const groups = React.useMemo(() => groupAccountsByProduct(accounts), [accounts]);
  const selected =
    selection?.type === "account"
      ? accounts.find((item) => item.id === selection.id) ?? null
      : null;
  const selectedProfile = selected ? profileResults[selected.id] : undefined;
  const selectedProfileError = selected ? profileErrors[selected.id] : undefined;
  const selectedGroup =
    selection?.type === "group"
      ? groups.find((item) => item.productName === selection.productName) ?? null
      : null;

  React.useEffect(() => {
    if (!selection) return;
    const key =
      selection.type === "account"
        ? `account:${selection.id}`
        : `group:${selection.productName}`;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let controller: AbortController | undefined;

    const refresh = async () => {
      if (stopped || document.visibilityState !== "visible") return;
      const cached = snapshotCache.current.get(key);
      setSnapshot(cached ?? null);
      setSnapshotError(null);
      controller = new AbortController();
      setSnapshotLoading(!cached);
      try {
        const next =
          selection.type === "account"
            ? await fetchTradingAccountSnapshot(selection.id, controller.signal)
            : await fetchProductGroupSnapshot(
                selection.productName,
                controller.signal,
              );
        if (stopped) return;
        snapshotCache.current.set(key, next);
        setSnapshot(next);
        setSnapshotError(null);
      } catch (err) {
        if (stopped || controller.signal.aborted) return;
        setSnapshotError(err instanceof Error ? err.message : "账户快照加载失败");
      } finally {
        if (!stopped) {
          setSnapshotLoading(false);
          timer = setTimeout(refresh, 3000);
        }
      }
    };
    const onVisibility = () => {
      if (document.visibilityState === "visible") {
        if (timer) clearTimeout(timer);
        void refresh();
      } else {
        if (timer) clearTimeout(timer);
        controller?.abort();
      }
    };
    document.addEventListener("visibilitychange", onVisibility);
    void refresh();
    return () => {
      stopped = true;
      if (timer) clearTimeout(timer);
      controller?.abort();
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [selection]);

  function resetForm() {
    setProductName("");
    setExchange("Binance");
    setAccountName("");
    setApiKey("");
    setApiSecret("");
    setPassphrase("");
    setPrivateKey("");
    setWalletAddress("");
    setVaultAddress("");
    setWalletType("eoa");
    setFunderAddress("");
    setFormError(null);
    setBusy(false);
  }

  async function onCreate(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setFormError(null);
    setBusy(true);
    try {
      const created = await createTradingAccount({
        productName,
        exchange,
        accountName,
        apiKey,
        apiSecret,
        passphrase,
        privateKey,
        walletAddress,
        walletType,
        funderAddress,
        vaultAddress,
      });
      setAddOpen(false);
      resetForm();
      await reload(created.id);
      setExpanded((current) => ({ ...current, [created.productName]: true }));
    } catch (err) {
      setFormError(err instanceof Error ? err.message : "添加失败");
      setBusy(false);
    }
  }

  async function onDelete() {
    if (!selected) return;
    const confirmed = window.confirm(
      `确认删除交易账户「${selected.accountName}」？此操作不可恢复。`,
    );
    if (!confirmed) return;
    setBusy(true);
    setError(null);
    try {
      await deleteTradingAccount(selected.id);
      setSelection(null);
      await reload(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "删除失败");
    } finally {
      setBusy(false);
    }
  }

  async function onApplyAccountProfile() {
    if (!selected || !accountProfileExchanges.has(selected.exchangeSlug)) return;
    const confirmed = window.confirm(
      `确认检查并设置交易账户「${selected.accountName}」？只修改当前账户；平台不会自动撤单、平仓、还款或迁移资产。`,
    );
    if (!confirmed) return;
    setProfileBusyId(selected.id);
    setProfileErrors((current) => {
      const next = { ...current };
      delete next[selected.id];
      return next;
    });
    try {
      const result = await applyTradingAccountProfile(selected.id);
      setProfileResults((current) => ({ ...current, [selected.id]: result }));
    } catch (err) {
      setProfileErrors((current) => ({
        ...current,
        [selected.id]: err instanceof Error ? err.message : "账户模式检查与设置失败",
      }));
    } finally {
      setProfileBusyId(null);
    }
  }

  return (
    <PageFrame>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <div className="text-[11px] font-semibold tracking-[0.22em] text-primary">
            ACCOUNT CENTER
          </div>
          <h1 className="mt-1 text-2xl font-semibold tracking-tight">账户管理</h1>
        </div>
        <div className="flex items-center gap-2">
          <Button
            variant="outline"
            className="gap-1.5"
            onClick={() => {
              resetForm();
              setAddOpen(true);
            }}
            disabled={busy}
          >
            <Plus className="size-4" />
            添加账户
          </Button>
          <Button
            variant="destructive"
            className="gap-1.5"
            onClick={() => void onDelete()}
            disabled={!selected || busy}
          >
            <Trash2 className="size-4" />
            删除账户
          </Button>
        </div>
      </div>

      <div className="rounded-lg border border-primary/20 bg-primary/[0.04] px-4 py-3 text-sm">
        <div className="font-medium">CEX 交易账户模式要求</div>
        <p className="mt-1 text-xs leading-5 text-muted-foreground">
          平台 CEX 交易账户仅支持统一账户、跨币种全仓保证金和净持仓模式；设置可能因交易所条件被拒绝，平台不会自动撤单、平仓或还款。
        </p>
      </div>

      <WorkspacePanel className="flex-1 lg:grid-cols-[280px_minmax(0,1fr)]">
        <aside className="flex min-h-0 flex-col overflow-hidden border-b lg:border-b-0 lg:border-r">
          <div className="border-b px-4 py-3 text-xs font-medium text-muted-foreground">
            产品 / 交易账户
          </div>
          <div className="min-h-0 flex-1 overflow-y-auto p-2">
            {loading ? (
              <div className="px-2 py-6 text-sm text-muted-foreground">加载中…</div>
            ) : error ? (
              <div className="space-y-3 px-2 py-4">
                <p className="text-sm text-destructive">{error}</p>
                <Button size="sm" variant="outline" onClick={() => void reload()}>
                  重试
                </Button>
              </div>
            ) : groups.length === 0 ? (
              <div className="px-2 py-6 text-sm text-muted-foreground">
                暂无交易账户，请先添加。
              </div>
            ) : (
              groups.map((group) => (
                <ProductNode
                  key={group.productName}
                  group={group}
                  expanded={expanded[group.productName] ?? true}
                  selection={selection}
                  onToggle={() =>
                    setExpanded((current) => ({
                      ...current,
                      [group.productName]: !(current[group.productName] ?? true),
                    }))
                  }
                  onSelectAccount={(id) => setSelection({ type: "account", id })}
                  onSelectGroup={(productName) =>
                    setSelection({ type: "group", productName })
                  }
                />
              ))
            )}
          </div>
        </aside>

        <section className="flex min-w-0 flex-col">
          {!selected && !selectedGroup ? (
            <div className="flex flex-1 flex-col items-center justify-center gap-3 p-8 text-center">
              <div className="flex size-12 items-center justify-center rounded-2xl bg-primary/10 text-primary">
                <WalletCards className="size-5" />
              </div>
              <p className="text-sm text-muted-foreground">
                请选择左侧产品组或交易账户以查看实时持仓。
              </p>
            </div>
          ) : selectedGroup ? (
            <GroupSnapshotView
              group={selectedGroup}
              snapshot={
                snapshot && "accountCount" in snapshot ? snapshot : null
              }
              loading={snapshotLoading}
              error={snapshotError}
            />
          ) : selected ? (
            <>
              <div className="border-b px-4 py-3">
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <div>
                    <div className="text-sm font-medium">
                      {selected.exchange} · {selected.accountName}
                    </div>
                    <div className="mt-0.5 text-xs text-muted-foreground">
                      产品：{selected.productName}
                      {selected.walletAddress
                        ? ` · ${selected.walletAddress}`
                        : ""}
                      {" · "}
                      {formatAccountFeeSummary(selected.fees)}
                      {selected.hasPassphrase ? " · Passphrase 已配置" : ""}
                    </div>
                  </div>
                  <div className="flex flex-wrap items-center gap-2">
                    {accountProfileExchanges.has(selected.exchangeSlug) ? (
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={() => void onApplyAccountProfile()}
                        disabled={profileBusyId === selected.id}
                      >
                        {profileBusyId === selected.id
                          ? "检查设置中…"
                          : "一键检查并设置"}
                      </Button>
                    ) : null}
                    <div className="flex flex-wrap items-center gap-5 rounded-lg border bg-muted/30 px-3 py-2">
                      <MetricHeader
                        label="账户权益"
                        value={formatDecimalCurrency(
                          snapshot && "accountEquityUsd" in snapshot
                            ? snapshot.accountEquityUsd
                            : "",
                        )}
                      />
                      <MetricHeader
                        label="可用资金"
                        value={formatDecimalCurrency(
                          snapshot && "availableFundsUsd" in snapshot
                            ? snapshot.availableFundsUsd
                            : "",
                        )}
                      />
                      <MetricHeader
                        label="风险"
                        value={
                          selected.exchange === "Polymarket"
                            ? "—"
                            : snapshot &&
                                "riskPercent" in snapshot &&
                                snapshot.riskPercent
                              ? `${formatDecimal(snapshot.riskPercent)}%`
                              : "—"
                        }
                      />
                    </div>
                  </div>
                </div>
                <div className="mt-2 flex flex-wrap items-center gap-2 text-[11px] text-muted-foreground">
                  {snapshotLoading ? "同步中…" : null}
                  {snapshot && "sourceUpdatedAt" in snapshot
                    ? `更新于 ${new Date(snapshot.sourceUpdatedAt).toLocaleTimeString()}`
                    : null}
                  {snapshot?.stale ? (
                    <span className="rounded bg-amber-500/10 px-1.5 py-0.5 text-amber-600">
                      缓存数据
                    </span>
                  ) : null}
                  {snapshotError ? (
                    <span className="text-destructive">{snapshotError}</span>
                  ) : null}
                </div>
                {selectedProfileError ? (
                  <div className="mt-3 rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs text-destructive">
                    {selectedProfileError}
                  </div>
                ) : null}
                {selectedProfile ? (
                  <AccountProfileResultView result={selectedProfile} />
                ) : null}
              </div>
              <AccountPositionsTable
                polymarket={selected.exchange === "Polymarket"}
                positions={
                  snapshot && "tradingAccountId" in snapshot
                    ? snapshot.positions
                    : []
                }
                loading={snapshotLoading && !snapshot}
              />
            </>
          ) : null}
        </section>
      </WorkspacePanel>

      <Sheet
        open={addOpen}
        onOpenChange={(open) => {
          if (!open) {
            setAddOpen(false);
            resetForm();
          }
        }}
      >
        <SheetContent side="right" className="w-full sm:max-w-md">
          <SheetHeader>
            <SheetTitle>添加交易账户</SheetTitle>
            <SheetDescription>
              填写产品与交易所 API 凭据。Secret 仅加密保存，列表不会回显明文。
            </SheetDescription>
          </SheetHeader>
          <form onSubmit={onCreate} className="flex flex-1 flex-col gap-4 px-4">
            <label className="grid gap-1.5 text-sm">
              <span className="text-muted-foreground">产品名称</span>
              <Input
                value={productName}
                onChange={(event) => setProductName(event.target.value)}
                placeholder="例如 Funding Arb"
                required
                disabled={busy}
              />
            </label>
            <label className="grid gap-1.5 text-sm">
              <span className="text-muted-foreground">交易所</span>
              <select
                className="h-8 w-full rounded-lg border border-input bg-transparent px-2.5 text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
                value={exchange}
                onChange={(event) => setExchange(event.target.value as Exchange)}
                disabled={busy}
              >
                {exchangeOptions.map((item) => (
                  <option key={item} value={item}>
                    {item}
                  </option>
                ))}
              </select>
            </label>
            <label className="grid gap-1.5 text-sm">
              <span className="text-muted-foreground">账户名称</span>
              <Input
                value={accountName}
                onChange={(event) => setAccountName(event.target.value)}
                placeholder="例如 main"
                required
                disabled={busy}
              />
            </label>
            {isWalletDexExchange(exchange) ? (
              <>
                <label className="grid gap-1.5 text-sm">
                  <span className="text-muted-foreground">钱包地址</span>
                  <Input
                    value={walletAddress}
                    onChange={(event) => setWalletAddress(event.target.value)}
                    placeholder="0x…"
                    autoComplete="off"
                    required
                    disabled={busy}
                  />
                </label>
                <label className="grid gap-1.5 text-sm">
                  <span className="text-muted-foreground">私钥</span>
                  <Input
                    type="password"
                    value={privateKey}
                    onChange={(event) => setPrivateKey(event.target.value)}
                    autoComplete="new-password"
                    required
                    disabled={busy}
                  />
                </label>
                <p className="text-xs text-muted-foreground">
                  只需填写主账户钱包地址和 API Wallet 私钥。交易索引由后端自动发现。
                </p>
                <label className="grid gap-1.5 text-sm">
                  <span className="text-muted-foreground">Vault Address（可选）</span>
                  <Input
                    value={vaultAddress}
                    onChange={(event) => setVaultAddress(event.target.value)}
                    placeholder="0x…（Vault）"
                    autoComplete="off"
                    disabled={busy}
                  />
                </label>
              </>
            ) : exchange === "Polymarket" ? (
              <>
                <label className="grid gap-1.5 text-sm">
                  <span className="text-muted-foreground">钱包类型</span>
                  <select
                    className="h-8 rounded-lg border border-input bg-transparent px-2.5"
                    value={walletType}
                    onChange={(event) =>
                      setWalletType(event.target.value as "eoa" | "deposit")
                    }
                    disabled={busy}
                  >
                    <option value="eoa">EOA 钱包</option>
                    <option value="deposit">Proxy Wallet</option>
                  </select>
                </label>
                <label className="grid gap-1.5 text-sm">
                  <span className="text-muted-foreground">钱包私钥</span>
                  <Input
                    type="password"
                    value={privateKey}
                    onChange={(event) => setPrivateKey(event.target.value)}
                    autoComplete="new-password"
                    required
                    disabled={busy}
                  />
                </label>
                {walletType === "deposit" ? (
                  <label className="grid gap-1.5 text-sm">
                    <span className="text-muted-foreground">Funder / Deposit 地址</span>
                    <Input
                      value={funderAddress}
                      onChange={(event) => setFunderAddress(event.target.value)}
                      required
                      disabled={busy}
                    />
                  </label>
                ) : null}
                <p className="text-xs text-muted-foreground">
                  CLOB API 凭据将由服务自动派生，私钥仅加密保存。
                </p>
              </>
            ) : (
              <>
                <label className="grid gap-1.5 text-sm">
                  <span className="text-muted-foreground">API Key</span>
                  <Input
                    value={apiKey}
                    onChange={(event) => setApiKey(event.target.value)}
                    autoComplete="off"
                    required
                    disabled={busy}
                  />
                </label>
                <label className="grid gap-1.5 text-sm">
                  <span className="text-muted-foreground">Secret</span>
                  <Input
                    type="password"
                    value={apiSecret}
                    onChange={(event) => setApiSecret(event.target.value)}
                    autoComplete="off"
                    required
                    disabled={busy}
                  />
                </label>
                <label className="grid gap-1.5 text-sm">
                  <span className="text-muted-foreground">Passphrase（可选）</span>
                  <Input
                    type="password"
                    value={passphrase}
                    onChange={(event) => setPassphrase(event.target.value)}
                    autoComplete="off"
                    disabled={busy}
                  />
                </label>
              </>
            )}
            {formError ? (
              <p className="rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-sm text-destructive">
                {formError}
              </p>
            ) : null}
            <SheetFooter className="px-0">
              <Button type="submit" disabled={busy} className="w-full">
                {busy ? "提交中…" : "确认添加"}
              </Button>
            </SheetFooter>
          </form>
        </SheetContent>
      </Sheet>
    </PageFrame>
  );
}

const accountProfileStepLabels: Record<
  AccountProfileResult["steps"][number]["step"],
  string
> = {
  unified_account: "统一账户",
  multi_asset_cross_margin: "跨币种全仓保证金",
  one_way_position: "净持仓模式",
};

const accountProfileStatusLabels: Record<
  AccountProfileResult["steps"][number]["status"],
  string
> = {
  compliant: "已符合",
  applied: "已设置",
  pending: "处理中",
  manual_required: "需人工处理",
  failed: "失败",
};

function AccountProfileResultView({ result }: { result: AccountProfileResult }) {
  return (
    <div className="mt-3 grid gap-2 sm:grid-cols-3">
      {result.steps.map((step) => (
        <div key={step.step} className="rounded-md border bg-muted/20 px-3 py-2">
          <div className="flex items-center justify-between gap-2 text-xs">
            <span className="font-medium">{accountProfileStepLabels[step.step]}</span>
            <span
              className={cn(
                "rounded px-1.5 py-0.5 text-[10px]",
                step.status === "compliant" || step.status === "applied"
                  ? "bg-emerald-500/10 text-emerald-600"
                  : step.status === "pending" ||
                      step.status === "manual_required"
                    ? "bg-amber-500/10 text-amber-600"
                    : "bg-destructive/10 text-destructive",
              )}
            >
              {accountProfileStatusLabels[step.status]}
            </span>
          </div>
          {step.message || step.code ? (
            <div className="mt-1.5 break-words text-[11px] leading-4 text-muted-foreground">
              {step.code ? `${step.code} · ` : ""}
              {step.message}
            </div>
          ) : null}
        </div>
      ))}
    </div>
  );
}

function AccountPositionsTable({
  polymarket,
  positions,
  loading,
}: {
  polymarket: boolean;
  positions: PortfolioPosition[];
  loading: boolean;
}) {
  if (loading) {
    return <div className="p-8 text-center text-sm text-muted-foreground">加载持仓中…</div>;
  }
  if (positions.length === 0) {
    return <div className="p-8 text-center text-sm text-muted-foreground">当前无有效持仓。</div>;
  }
  const headers = polymarket
    ? ["Market", "Outcome", "均价 → 当前价", "份额", "初始价值", "浮动盈亏", "当前价值"]
    : ["交易所", "标的", "方向", "持仓市值", "持仓量（基础币）", "现货", "开仓均价", "标记价格", "浮动盈亏"];
  const displayedPositions = polymarket
    ? positions
    : [...positions].sort((left, right) => {
        const leftNotional = Number(left.notionalUsd);
        const rightNotional = Number(right.notionalUsd);
        const leftValue = Number.isFinite(leftNotional) ? Math.abs(leftNotional) : -1;
        const rightValue = Number.isFinite(rightNotional) ? Math.abs(rightNotional) : -1;
        return rightValue - leftValue || left.key.localeCompare(right.key);
      });
  return (
    <WideTableScroll className="min-h-0 flex-1">
      <table className="w-full min-w-[920px] text-sm">
        <thead className="sticky top-0 bg-card text-xs text-muted-foreground">
          <tr className="border-b">
            {headers.map((header, index) => (
              <th key={header} className={cn("px-4 py-2.5 font-medium", index === 0 || (polymarket && index === 1) ? "text-left" : "text-right")}>
                {header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {displayedPositions.map((position) => (
            <tr key={position.key} className="border-b last:border-0 hover:bg-muted/30">
              {polymarket ? (
                <>
                  <td className="max-w-[360px] px-4 py-3 font-medium">{position.marketTitle}</td>
                  <td className="px-4 py-3">{position.outcome}</td>
                  <td className="px-4 py-3 text-right font-mono">{formatDecimal(position.entryPrice)} → {formatDecimal(position.markPrice)}</td>
                  <td className="px-4 py-3 text-right font-mono">{formatDecimal(position.size)}</td>
                  <td className="px-4 py-3 text-right font-mono">{formatDecimalCurrency(position.initialValue)}</td>
                  <PnlCell value={position.cashPnl} />
                  <td className="px-4 py-3 text-right font-mono">{formatDecimalCurrency(position.currentValue)}</td>
                </>
              ) : (
                <>
                  <td className="px-4 py-3">{position.exchange}</td>
                  <td className="px-4 py-3 font-medium">{position.symbol}</td>
                  <td className={cn("px-4 py-3 text-right", position.side === "long" ? "text-emerald-600" : "text-rose-600")}>{position.side === "long" ? "多" : "空"}</td>
                  <td className="px-4 py-3 text-right font-mono">{formatDecimalCurrency(position.notionalUsd)}</td>
                  <td className="px-4 py-3 text-right font-mono">{formatDecimal(position.size)}</td>
                  <td className="px-4 py-3 text-right font-mono">{formatSignedDecimal(position.spotSize)}</td>
                  <td className="px-4 py-3 text-right font-mono">{formatDecimal(position.entryPrice)}</td>
                  <td className="px-4 py-3 text-right font-mono">{formatDecimal(position.markPrice)}</td>
                  <PnlCell value={position.unrealizedPnl} />
                </>
              )}
            </tr>
          ))}
        </tbody>
      </table>
    </WideTableScroll>
  );
}

function PnlCell({ value }: { value: string }) {
  const number = Number(value);
  return (
    <td className={cn("px-4 py-3 text-right font-mono", number > 0 ? "text-emerald-600" : number < 0 ? "text-rose-600" : "")}>
      {formatDecimalCurrency(value)}
    </td>
  );
}

function sumProductGroupNotionalUsd(
  positions: ProductGroupSnapshot["positions"],
): string | undefined {
  let total = 0;
  let hasValue = false;
  for (const position of positions) {
    const value = Number(position.totalNotionalUsd);
    if (!Number.isFinite(value)) continue;
    total += value;
    hasValue = true;
  }
  return hasValue ? String(total) : undefined;
}

function formatCapitalUtilization(notionalUsd?: string, equityUsd?: string) {
  const notional = Number(notionalUsd);
  const equity = Number(equityUsd);
  if (!Number.isFinite(notional) || !Number.isFinite(equity) || equity === 0) {
    return "—";
  }
  return `${((notional / equity) * 100).toFixed(2)}%`;
}

function GroupSnapshotView({
  group,
  snapshot,
  loading,
  error,
}: {
  group: ProductGroup;
  snapshot: ProductGroupSnapshot | null;
  loading: boolean;
  error: string | null;
}) {
  const totalPositionNotionalUsd = React.useMemo(
    () => (snapshot ? sumProductGroupNotionalUsd(snapshot.positions) : undefined),
    [snapshot],
  );

  return (
    <>
      <div className="border-b px-4 py-3">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div>
            <div className="text-sm font-medium">{group.productName} · 产品组汇总</div>
            <div className="mt-1 flex gap-2 text-xs text-muted-foreground">
              <span>{group.accounts.length} 个账户</span>
              {loading ? <span>同步中…</span> : null}
              {snapshot?.stale || snapshot?.partial ? <span className="text-amber-600">部分缓存数据</span> : null}
              {error ? <span className="text-destructive">{error}</span> : null}
            </div>
          </div>
          <div className="flex flex-wrap items-center gap-5 rounded-lg border bg-muted/30 px-3 py-2">
            <MetricHeader
              label="账户权益"
              value={formatDecimalCurrency(snapshot?.accountEquityUsd)}
            />
            <MetricHeader
              label="可用资产"
              value={formatDecimalCurrency(snapshot?.availableFundsUsd)}
            />
          </div>
        </div>
      </div>
      {!snapshot || snapshot.positions.length === 0 ? (
        <div className="p-8 text-center text-sm text-muted-foreground">当前无汇总持仓。</div>
      ) : (
        <WideTableScroll className="min-h-0 flex-1">
          <table className="w-full min-w-[640px] text-sm">
            <thead className="sticky top-0 bg-card text-xs text-muted-foreground">
              <tr className="border-b">
                <th className="px-4 py-2.5 text-left font-medium">标的</th>
                <th className="px-4 py-2.5 text-right font-medium">现货持仓量</th>
                <th className="px-4 py-2.5 text-right font-medium">合约持仓量（基础币）</th>
                <th className="px-4 py-2.5 text-right font-medium">总持仓市值</th>
              </tr>
            </thead>
            <tbody>
              {snapshot.positions.map((position) => (
                <tr key={position.symbol} className="border-b last:border-0">
                  <td className="px-4 py-3 font-medium">{position.symbol}</td>
                  <td className="px-4 py-3 text-right font-mono">{formatSignedDecimal(position.spotSize)}</td>
                  <td className="px-4 py-3 text-right font-mono">{formatSignedDecimal(position.contractSize)}</td>
                  <td className="px-4 py-3 text-right font-mono">{formatDecimalCurrency(position.totalNotionalUsd)}</td>
                </tr>
              ))}
            </tbody>
            <tfoot>
              <tr className="border-t bg-muted/30">
                <td colSpan={3} className="px-4 py-3" />
                <td className="px-4 py-3 text-right">
                  <div className="flex flex-wrap items-center justify-end gap-x-5 gap-y-1">
                    <span>
                      <span className="mr-2 text-xs font-medium text-muted-foreground">
                        资金利用率
                      </span>
                      <span className="font-mono font-medium">
                        {formatCapitalUtilization(
                          totalPositionNotionalUsd,
                          snapshot.accountEquityUsd,
                        )}
                      </span>
                    </span>
                    <span>
                      <span className="mr-2 text-xs font-medium text-muted-foreground">
                        产品持仓总市值
                      </span>
                      <span className="font-mono font-medium">
                        {formatDecimalCurrency(totalPositionNotionalUsd)}
                      </span>
                    </span>
                  </div>
                </td>
              </tr>
            </tfoot>
          </table>
        </WideTableScroll>
      )}
    </>
  );
}

function ProductNode({
  group,
  expanded,
  selection,
  onToggle,
  onSelectAccount,
  onSelectGroup,
}: {
  group: ProductGroup;
  expanded: boolean;
  selection:
    | { type: "account"; id: number }
    | { type: "group"; productName: string }
    | null;
  onToggle: () => void;
  onSelectAccount: (id: number) => void;
  onSelectGroup: (productName: string) => void;
}) {
  const groupActive =
    selection?.type === "group" && selection.productName === group.productName;
  return (
    <div className={cn("mb-1 border-l-2 pl-0.5", groupActive ? "border-primary" : "border-transparent")}>
      <div className={cn("flex items-center rounded-md hover:bg-muted", groupActive && "bg-primary/[0.08] text-primary")}>
        <button type="button" onClick={onToggle} className="p-2" aria-label={expanded ? "收起产品组" : "展开产品组"}>
          {expanded ? <ChevronDown className="size-3.5" /> : <ChevronRight className="size-3.5" />}
        </button>
        <button
          type="button"
          onClick={() => onSelectGroup(group.productName)}
          className="flex min-w-0 flex-1 items-center py-1.5 pr-2 text-left text-sm font-medium"
        >
          <span className="truncate">{group.productName}</span>
        <span className="ml-auto text-[11px] text-muted-foreground">
          {group.accounts.length}
        </span>
        </button>
      </div>
      {expanded ? (
        <div className="ml-2 space-y-0.5 border-l pl-2">
          {group.accounts.map((account) => {
            const active = selection?.type === "account" && account.id === selection.id;
            return (
              <button
                key={account.id}
                type="button"
                onClick={() => onSelectAccount(account.id)}
                className={cn(
                  "flex w-full flex-col rounded-md px-2 py-1.5 text-left text-sm transition-colors",
                  active
                    ? "bg-primary/[0.08] font-medium text-primary"
                    : "text-muted-foreground hover:bg-muted hover:text-foreground",
                )}
              >
                <span className="truncate">{account.accountName}</span>
                <span className="truncate text-[11px] opacity-80">
                  {account.exchange}
                </span>
              </button>
            );
          })}
        </div>
      ) : null}
    </div>
  );
}
