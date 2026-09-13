import type { Exchange } from "@/types/market";

export type TradingStatus =
  | "checking"
  | "ready"
  | "wallet_unauthorized"
  | "private_key_mismatch"
  | "account_not_found"
  | "api_wallet_not_found"
  | "instrument_unavailable"
  | "venue_unavailable"
  | "unsupported_auth_mode"
  | "unknown"
  | string;

export interface MarketFeeRate {
  status: string;
  maker: string;
  taker: string;
}

export interface TradingAccountFeeRates {
  spot: MarketFeeRate;
  contract: MarketFeeRate;
  updatedAt: string;
  syncStatus: string;
  syncError: string;
  stale: boolean;
  unsupportedMarkets: string[];
}

export interface TradingAccountDTO {
  id: number;
  productName: string;
  exchange: string;
  exchangeSlug?: string;
  accountName: string;
  hasPassphrase: boolean;
  walletAddress?: string;
  walletType?: string;
  bindingStatus?: string;
  credentialsPresent?: boolean;
  credentialsVerified?: boolean;
  tradingMode?: string;
  tradingReady?: boolean;
  tradingStatus?: TradingStatus;
  tradingUnavailableCode?: string;
  tradingUnavailableReason?: string;
  resolvedAccountIndex?: number;
  resolvedApiKeyIndex?: number;
  spotFee?: MarketFeeRate;
  contractFee?: MarketFeeRate;
  feeSource?: string;
  feeUpdatedAt?: string;
  feeSyncStatus?: string;
  feeSyncError?: string;
  feeStale?: boolean;
  unsupportedMarkets?: string[];
  createdAt: string;
  updatedAt: string;
}

export interface TradingAccountsResponse {
  data: TradingAccountDTO[];
  meta: { total: number };
}

export interface CreateTradingAccountPayload {
  productName: string;
  exchange: string;
  accountName: string;
  apiKey: string;
  apiSecret: string;
  passphrase?: string;
  privateKey?: string;
  walletAddress?: string;
  walletType?: "eoa" | "deposit";
  funderAddress?: string;
  tradingApiKey?: string;
  tradingApiSecret?: string;
  signingAddress?: string;
  vaultAddress?: string;
  accountIndex?: number;
  apiKeyIndex?: number;
}

export interface TradingAccount {
  id: number;
  productName: string;
  exchange: Exchange;
  exchangeSlug: string;
  accountName: string;
  hasPassphrase: boolean;
  walletAddress?: string;
  walletType?: string;
  bindingStatus?: string;
  credentialsPresent?: boolean;
  credentialsVerified?: boolean;
  tradingMode?: string;
  tradingReady?: boolean;
  tradingStatus?: TradingStatus;
  tradingUnavailableCode?: string;
  tradingUnavailableReason?: string;
  resolvedAccountIndex?: number;
  resolvedApiKeyIndex?: number;
  fees?: TradingAccountFeeRates;
  createdAt: string;
  updatedAt: string;
}

export interface ProductGroup {
  productName: string;
  accounts: TradingAccount[];
}

export interface PortfolioPosition {
  key: string;
  kind: "cex" | "polymarket";
  exchange: string;
  symbol: string;
  side: string;
  notionalUsd: string;
  size: string;
  spotSize: string;
  signedContractSize: string;
  entryPrice: string;
  markPrice: string;
  unrealizedPnl: string;
  marketTitle: string;
  outcome: string;
  initialValue: string;
  currentValue: string;
  cashPnl: string;
  conditionId: string;
  tokenId: string;
  endTime: string;
}

export interface TradingAccountSnapshot {
  tradingAccountId: number;
  productName: string;
  exchange: string;
  accountName: string;
  accountEquityUsd: string;
  availableFundsUsd: string;
  riskPercent: string;
  positions: PortfolioPosition[];
  sourceUpdatedAt: string;
  serverTime: string;
  stale: boolean;
  lastError: string;
}

export interface ProductGroupSnapshot {
  productName: string;
  accountCount: number;
  accountEquityUsd: string;
  availableFundsUsd: string;
  positions: Array<{
    symbol: string;
    side: string;
    totalNotionalUsd: string;
    spotSize: string;
    contractSize: string;
  }>;
  sourceUpdatedAt: string;
  serverTime: string;
  stale: boolean;
  partial: boolean;
  errors: string[];
}

export type AccountProfileStepStatus =
  | "compliant"
  | "applied"
  | "pending"
  | "manual_required"
  | "failed";

export interface AccountProfileStepResult {
  step:
    | "unified_account"
    | "multi_asset_cross_margin"
    | "one_way_position";
  status: AccountProfileStepStatus;
  code: string;
  message: string;
}

export interface AccountProfileResult {
  tradingAccountId: number;
  productName: string;
  accountName: string;
  exchange: string;
  overallStatus: AccountProfileStepStatus;
  steps: AccountProfileStepResult[];
}

const exchanges = new Set<Exchange>([
  "Binance",
  "OKX",
  "Bybit",
  "Bitget",
  "Gate",
  "Hyperliquid",
  "Aster",
  "Lighter",
  "Polymarket",
]);

const walletDexSlugs = new Set(["hyperliquid", "aster", "lighter"]);

export function isWalletDexExchange(exchange: string): boolean {
  const trimmed = exchange.trim();
  if (trimmed === "Hyperliquid" || trimmed === "Aster" || trimmed === "Lighter") {
    return true;
  }
  return walletDexSlugs.has(trimmed.toLowerCase());
}

export function walletDexBindPayload(
  payload: CreateTradingAccountPayload,
): CreateTradingAccountPayload {
  if (!isWalletDexExchange(payload.exchange)) {
    return payload;
  }
  return {
    ...payload,
    apiKey: (payload.walletAddress || payload.apiKey).trim(),
    apiSecret: (payload.privateKey || payload.apiSecret).trim(),
  };
}

function apiBaseUrl(): string {
  return (process.env.NEXT_PUBLIC_API_BASE_URL ?? "").replace(/\/+$/, "");
}

function accountsUrl(path = ""): string {
  return `${apiBaseUrl()}/api/v1/trading-accounts${path}`;
}

export function normalizeExchange(value: string): Exchange {
  const trimmed = value.trim();
  if (exchanges.has(trimmed as Exchange)) {
    return trimmed as Exchange;
  }
  switch (trimmed.toLowerCase()) {
    case "okx":
      return "OKX";
    case "hyperliquid":
      return "Hyperliquid";
    case "aster":
      return "Aster";
    case "lighter":
      return "Lighter";
    case "binance":
      return "Binance";
    case "bybit":
      return "Bybit";
    case "bitget":
      return "Bitget";
    case "gate":
      return "Gate";
    case "polymarket":
      return "Polymarket";
    default:
      return "Binance";
  }
}

export function emptyTradingAccountFees(): TradingAccountFeeRates {
  return {
    spot: { status: "", maker: "", taker: "" },
    contract: { status: "", maker: "", taker: "" },
    updatedAt: "",
    syncStatus: "",
    syncError: "",
    stale: false,
    unsupportedMarkets: [],
  };
}

export function mapTradingAccountDto(dto: TradingAccountDTO): TradingAccount {
  const exchangeSlug = (dto.exchangeSlug || dto.exchange).trim().toLowerCase();
  const walletDex = isWalletDexExchange(exchangeSlug || dto.exchange);
  return {
    id: Number(dto.id),
    productName: dto.productName.trim(),
    exchange: normalizeExchange(dto.exchange),
    exchangeSlug,
    accountName: dto.accountName.trim(),
    hasPassphrase: Boolean(dto.hasPassphrase),
    walletAddress: dto.walletAddress ?? "",
    walletType: dto.walletType ?? "",
    bindingStatus: dto.bindingStatus ?? "",
    credentialsPresent: walletDex ? Boolean(dto.credentialsPresent) : true,
    credentialsVerified: walletDex ? Boolean(dto.credentialsVerified) : true,
    tradingMode: String(dto.tradingMode ?? (walletDex ? "" : "cex")),
    tradingReady: walletDex ? Boolean(dto.tradingReady) : dto.tradingReady !== false,
    tradingStatus: String(dto.tradingStatus ?? (walletDex ? "checking" : "ready")),
    tradingUnavailableCode: String(dto.tradingUnavailableCode ?? ""),
    tradingUnavailableReason: String(dto.tradingUnavailableReason ?? ""),
    resolvedAccountIndex: dto.resolvedAccountIndex,
    resolvedApiKeyIndex: dto.resolvedApiKeyIndex,
    fees: mapTradingAccountFees(dto),
    createdAt: dto.createdAt,
    updatedAt: dto.updatedAt,
  };
}

function emptyMarketFee(): MarketFeeRate {
  return { status: "", maker: "", taker: "" };
}

export function mapTradingAccountFees(dto: TradingAccountDTO): TradingAccountFeeRates {
  return {
    spot: dto.spotFee ?? emptyMarketFee(),
    contract: dto.contractFee ?? emptyMarketFee(),
    updatedAt: dto.feeUpdatedAt ?? "",
    syncStatus: dto.feeSyncStatus ?? "",
    syncError: dto.feeSyncError ?? "",
    stale: Boolean(dto.feeStale),
    unsupportedMarkets: dto.unsupportedMarkets ?? [],
  };
}

export function formatFeeRatePercent(rate: string, status: string): string {
  if (status === "unsupported") {
    return "暂不支持";
  }
  const trimmed = rate.trim();
  if (status !== "ok" || trimmed === "") {
    return "";
  }
  const value = Number(trimmed);
  if (!Number.isFinite(value)) {
    return "";
  }
  return `${(value * 100).toFixed(2)}%`;
}

export function formatMarketFeeSummary(label: string, fee: MarketFeeRate): string {
  if (fee.status === "unsupported") {
    return `${label}暂不支持`;
  }
  const maker = formatFeeRatePercent(fee.maker, fee.status);
  const taker = formatFeeRatePercent(fee.taker, fee.status);
  if (!maker || !taker) {
    return "";
  }
  return `${label} Maker ${maker} / Taker ${taker}`;
}

export function formatRelativeFeeUpdated(iso: string, nowMs = Date.now()): string {
  const ts = Date.parse(iso);
  if (!Number.isFinite(ts)) {
    return "";
  }
  const minutes = Math.floor(Math.max(0, nowMs - ts) / 60000);
  if (minutes < 1) {
    return "刚刚更新";
  }
  if (minutes < 60) {
    return `${minutes} 分钟前更新`;
  }
  const hours = Math.floor(minutes / 60);
  if (hours < 24) {
    return `${hours} 小时前更新`;
  }
  return `${Math.floor(hours / 24)} 天前更新`;
}

export function formatAccountFeeSummary(
  fees: TradingAccountFeeRates | undefined,
  nowMs = Date.now(),
): string {
  if (!fees) {
    return "手续费待同步";
  }
  if (
    fees.spot.status === "unsupported" &&
    fees.contract.status === "unsupported"
  ) {
    return "暂不支持";
  }
  const parts: string[] = [];
  const spot = formatMarketFeeSummary("现货", fees.spot);
  const contract = formatMarketFeeSummary("合约", fees.contract);
  if (spot) parts.push(spot);
  if (contract) parts.push(contract);
  if (fees.syncStatus === "pending" && parts.length === 0) {
    return "手续费待同步";
  }
  const failed =
    fees.syncStatus === "failed" ||
    fees.syncStatus === "retrying" ||
    fees.syncStatus === "credential_invalid";
  if (failed && parts.length === 0) {
    return "手续费同步失败";
  }
  if (failed) {
    parts.push("同步失败");
  }
  if (fees.stale) {
    parts.push("已过期");
  }
  const updated = formatRelativeFeeUpdated(fees.updatedAt, nowMs);
  if (updated) {
    parts.push(updated);
  }
  if (parts.length === 0) {
    return "手续费待同步";
  }
  return parts.join(" · ");
}

export function applyTradingReadiness(
  account: TradingAccount,
  ready: Partial<TradingAccountDTO>,
): TradingAccount {
  return {
    ...account,
    credentialsPresent: ready.credentialsPresent ?? account.credentialsPresent,
    credentialsVerified: ready.credentialsVerified ?? account.credentialsVerified,
    tradingMode: ready.tradingMode ?? account.tradingMode,
    tradingReady: Boolean(ready.tradingReady),
    tradingStatus: String(ready.tradingStatus ?? account.tradingStatus),
    tradingUnavailableCode: String(ready.tradingUnavailableCode ?? ""),
    tradingUnavailableReason: String(ready.tradingUnavailableReason ?? ""),
    resolvedAccountIndex: ready.resolvedAccountIndex ?? account.resolvedAccountIndex,
    resolvedApiKeyIndex: ready.resolvedApiKeyIndex ?? account.resolvedApiKeyIndex,
  };
}

export function tradingAccountStatusLabel(account: TradingAccount): string {
  if (account.tradingReady) {
    return account.accountName;
  }
  if (account.tradingStatus === "checking" || account.tradingStatus === "") {
    return `${account.accountName}（交易能力检查中）`;
  }
  return `${account.accountName}（不可交易）`;
}

export async function inspectTradingReadiness(
  id: number,
  signal?: AbortSignal,
): Promise<TradingAccountDTO> {
  const response = await fetch(accountsUrl(`/${id}/trading-readiness`), {
    method: "POST",
    credentials: "include",
    headers: { Accept: "application/json" },
    signal,
  });
  const body = (await response.json().catch(() => ({}))) as {
    data?: TradingAccountDTO;
    error?: string;
  };
  if (!response.ok) {
    throw new Error(body.error || "交易能力检查失败");
  }
  if (!body.data) {
    throw new Error("交易能力检查响应无效");
  }
  return body.data;
}

export function groupAccountsByProduct(accounts: TradingAccount[]): ProductGroup[] {
  const groups = new Map<string, TradingAccount[]>();
  for (const account of accounts) {
    const list = groups.get(account.productName) ?? [];
    list.push(account);
    groups.set(account.productName, list);
  }
  return Array.from(groups.entries())
    .sort(([left], [right]) => left.localeCompare(right, "zh-CN"))
    .map(([productName, items]) => ({
      productName,
      accounts: items.slice().sort((a, b) => {
        const exchangeCmp = a.exchange.localeCompare(b.exchange);
        if (exchangeCmp !== 0) return exchangeCmp;
        return a.accountName.localeCompare(b.accountName, "zh-CN");
      }),
    }));
}

export async function fetchTradingAccounts(
  signal?: AbortSignal,
): Promise<TradingAccount[]> {
  const response = await fetch(accountsUrl(), {
    method: "GET",
    credentials: "include",
    headers: { Accept: "application/json" },
    cache: "no-store",
    signal,
  });
  if (response.status === 401) {
    throw new Error("请先登录");
  }
  if (!response.ok) {
    throw new Error("无法加载交易账户");
  }
  const payload = (await response.json()) as TradingAccountsResponse;
  return (payload.data ?? []).map(mapTradingAccountDto);
}

async function snapshotRequest<T>(path: string, signal?: AbortSignal): Promise<T> {
  const response = await fetch(`${apiBaseUrl()}${path}`, {
    credentials: "include",
    headers: { Accept: "application/json" },
    cache: "no-store",
    signal,
  });
  const body = (await response.json().catch(() => ({}))) as T & { error?: string };
  if (response.status === 401) throw new Error("请先登录");
  if (!response.ok) throw new Error(body.error || "账户快照暂不可用");
  return body;
}

export function fetchTradingAccountSnapshot(
  id: number,
  signal?: AbortSignal,
): Promise<TradingAccountSnapshot> {
  return snapshotRequest(`/api/v1/trading-accounts/${id}/snapshot`, signal);
}

export const ACCOUNT_FUNDS_MAX_AGE_MS = 10 * 60 * 1000;

export function accountSnapshotNeedsLiveRefresh(
  snapshot: Pick<TradingAccountSnapshot, "sourceUpdatedAt" | "serverTime">,
): boolean {
  const source = Date.parse(snapshot.sourceUpdatedAt);
  const server = Date.parse(snapshot.serverTime);
  if (!Number.isFinite(source) || !Number.isFinite(server)) {
    return true;
  }
  return server - source > ACCOUNT_FUNDS_MAX_AGE_MS;
}

const liveSnapshotInflight = new Map<number, Promise<TradingAccountSnapshot>>();

function abortableSnapshot(
  pending: Promise<TradingAccountSnapshot>,
  signal?: AbortSignal,
): Promise<TradingAccountSnapshot> {
  if (!signal) {
    return pending;
  }
  return new Promise((resolve, reject) => {
    const abort = () => {
      reject(new DOMException("The operation was aborted.", "AbortError"));
    };
    if (signal.aborted) {
      abort();
      return;
    }
    signal.addEventListener("abort", abort, { once: true });
    pending.then(
      (value) => {
        signal.removeEventListener("abort", abort);
        if (signal.aborted) {
          abort();
          return;
        }
        resolve(value);
      },
      (error) => {
        signal.removeEventListener("abort", abort);
        reject(error);
      },
    );
  });
}

export function fetchLiveTradingAccountSnapshot(
  id: number,
  signal?: AbortSignal,
): Promise<TradingAccountSnapshot> {
  let pending = liveSnapshotInflight.get(id);
  if (!pending) {
    pending = fetchTradingAccountSnapshot(id).finally(() => {
      if (liveSnapshotInflight.get(id) === pending) {
        liveSnapshotInflight.delete(id);
      }
    });
    liveSnapshotInflight.set(id, pending);
  }
  return abortableSnapshot(pending, signal);
}

export type CachedTradingAccountSnapshotResult =
  | { status: "ok"; snapshot: TradingAccountSnapshot }
  | { status: "missing" };

export async function fetchCachedTradingAccountSnapshot(
  id: number,
  signal?: AbortSignal,
): Promise<CachedTradingAccountSnapshotResult> {
  const response = await fetch(
    `${apiBaseUrl()}/api/v1/trading-accounts/${id}/snapshot?cacheOnly=true`,
    {
      credentials: "include",
      headers: { Accept: "application/json" },
      cache: "no-store",
      signal,
    },
  );
  if (response.status === 204) {
    return { status: "missing" };
  }
  if (response.status === 401) {
    throw new Error("请先登录");
  }
  if (!response.ok) {
    const body = (await response.json().catch(() => ({}))) as { error?: string };
    throw new Error(body.error || "账户快照暂不可用");
  }
  const snapshot = (await response.json()) as TradingAccountSnapshot;
  return { status: "ok", snapshot };
}

export function fetchProductGroupSnapshot(
  productName: string,
  signal?: AbortSignal,
): Promise<ProductGroupSnapshot> {
  return snapshotRequest(
    `/api/v1/trading-account-products/${encodeURIComponent(productName)}/snapshot`,
    signal,
  );
}

export async function createTradingAccount(
  payload: CreateTradingAccountPayload,
): Promise<TradingAccount> {
  const mapped = walletDexBindPayload(payload);
  const response = await fetch(accountsUrl(), {
    method: "POST",
    credentials: "include",
    headers: {
      Accept: "application/json",
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      productName: mapped.productName.trim(),
      exchange: mapped.exchange.trim().toLowerCase(),
      accountName: mapped.accountName.trim(),
      apiKey: mapped.apiKey.trim(),
      apiSecret: mapped.apiSecret.trim(),
      passphrase: mapped.passphrase?.trim() || "",
      privateKey: mapped.privateKey?.trim() || "",
      walletType: mapped.walletType || "",
      funderAddress: mapped.funderAddress?.trim() || "",
      tradingApiKey: isWalletDexExchange(mapped.exchange) ? "" : mapped.tradingApiKey?.trim() || "",
      tradingApiSecret: isWalletDexExchange(mapped.exchange) ? "" : mapped.tradingApiSecret?.trim() || "",
      signingAddress: isWalletDexExchange(mapped.exchange) ? "" : mapped.signingAddress?.trim() || "",
      vaultAddress: mapped.vaultAddress?.trim() || "",
      accountIndex: isWalletDexExchange(mapped.exchange) ? undefined : mapped.accountIndex,
      apiKeyIndex: isWalletDexExchange(mapped.exchange) ? undefined : mapped.apiKeyIndex,
    }),
  });
  const body = (await response.json().catch(() => ({}))) as {
    data?: TradingAccountDTO;
    error?: string;
  };
  if (!response.ok) {
    throw new Error(body.error || "添加交易账户失败");
  }
  if (!body.data) {
    throw new Error("添加交易账户响应无效");
  }
  return mapTradingAccountDto(body.data);
}

export async function deleteTradingAccount(id: number): Promise<void> {
  const response = await fetch(accountsUrl(`/${id}`), {
    method: "DELETE",
    credentials: "include",
    headers: { Accept: "application/json" },
  });
  if (!response.ok) {
    const body = (await response.json().catch(() => ({}))) as { error?: string };
    throw new Error(body.error || "删除交易账户失败");
  }
}

export async function applyTradingAccountProfile(
  id: number,
): Promise<AccountProfileResult> {
  const response = await fetch(
    `${apiBaseUrl()}/api/v1/trader/accounts/${id}/account-profile/apply`,
    {
      method: "POST",
      credentials: "include",
      headers: { Accept: "application/json" },
    },
  );
  const body = (await response.json().catch(() => ({}))) as {
    data?: AccountProfileResult;
    error?: string;
  };
  if (response.status === 401) {
    throw new Error("请先登录");
  }
  if (!response.ok) {
    throw new Error(body.error || "账户模式检查与设置失败");
  }
  if (!body.data) {
    throw new Error("账户模式响应无效");
  }
  return {
    ...body.data,
    tradingAccountId: Number(body.data.tradingAccountId),
    steps: body.data.steps ?? [],
  };
}
