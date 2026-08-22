import type { Exchange } from "@/types/market";

export interface TradingAccountDTO {
  id: number;
  productName: string;
  exchange: string;
  exchangeSlug?: string;
  accountName: string;
  apiKeyMasked: string;
  hasPassphrase: boolean;
  walletAddress?: string;
  walletType?: string;
  bindingStatus?: string;
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
  walletType?: "eoa" | "deposit";
  funderAddress?: string;
}

export interface TradingAccount {
  id: number;
  productName: string;
  exchange: Exchange;
  exchangeSlug: string;
  accountName: string;
  apiKeyMasked: string;
  hasPassphrase: boolean;
  walletAddress?: string;
  walletType?: string;
  bindingStatus?: string;
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

const exchanges = new Set<Exchange>([
  "Binance",
  "OKX",
  "Bybit",
  "Bitget",
  "Gate",
  "Hyperliquid",
  "Polymarket",
]);

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

export function mapTradingAccountDto(dto: TradingAccountDTO): TradingAccount {
  return {
    id: Number(dto.id),
    productName: dto.productName.trim(),
    exchange: normalizeExchange(dto.exchange),
    exchangeSlug: (dto.exchangeSlug || dto.exchange).trim().toLowerCase(),
    accountName: dto.accountName.trim(),
    apiKeyMasked: dto.apiKeyMasked,
    hasPassphrase: Boolean(dto.hasPassphrase),
    walletAddress: dto.walletAddress ?? "",
    walletType: dto.walletType ?? "",
    bindingStatus: dto.bindingStatus ?? "",
    createdAt: dto.createdAt,
    updatedAt: dto.updatedAt,
  };
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

export async function fetchTradingAccounts(): Promise<TradingAccount[]> {
  const response = await fetch(accountsUrl(), {
    method: "GET",
    credentials: "include",
    headers: { Accept: "application/json" },
    cache: "no-store",
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
  const response = await fetch(accountsUrl(), {
    method: "POST",
    credentials: "include",
    headers: {
      Accept: "application/json",
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      productName: payload.productName.trim(),
      exchange: payload.exchange.trim().toLowerCase(),
      accountName: payload.accountName.trim(),
      apiKey: payload.apiKey.trim(),
      apiSecret: payload.apiSecret.trim(),
      passphrase: payload.passphrase?.trim() || "",
      privateKey: payload.privateKey?.trim() || "",
      walletType: payload.walletType || "",
      funderAddress: payload.funderAddress?.trim() || "",
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
