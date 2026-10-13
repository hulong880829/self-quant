function apiBaseUrl(): string {
  return (process.env.NEXT_PUBLIC_API_BASE_URL ?? "").replace(/\/+$/, "");
}

function adminUrl(path: string): string {
  return `${apiBaseUrl()}${path}`;
}

async function readJSON<T>(response: Response): Promise<T> {
  const body = (await response.json().catch(() => ({}))) as T & { error?: string };
  if (response.status === 401) {
    throw new Error("请先登录");
  }
  if (response.status === 403) {
    throw new Error(body.error === "admin required" ? "需要管理员权限" : "没有权限");
  }
  if (!response.ok) {
    throw new Error(body.error || "管理接口请求失败");
  }
  return body;
}

function writeHeaders(): HeadersInit {
  return {
    Accept: "application/json",
    "Content-Type": "application/json",
  };
}

export interface PlatformAccount {
  username: string;
  permission: string;
  brokerId: string;
  status: string;
  createdSource: string;
  createdBy: string;
  authVersion: number;
  deletedAt?: string | null;
}

export interface Broker {
  brokerId: string;
  name: string;
  clientSecretMasked: string;
  clientSecret?: string;
  status: string;
  tokenVersion: number;
  createdAt?: string;
  updatedAt?: string;
}

export interface OverviewRow {
  brokerId: string;
  name: string;
  status: string;
  platformAccounts: number;
  running: number;
  closing: number;
  closed: number;
  failed: number;
  equityUsd: string;
  coveredAccounts: number;
  failedSnapshots: number;
  staleSnapshots: number;
}

export async function fetchAdminOverview(signal?: AbortSignal): Promise<{
  platform: OverviewRow;
  brokers: OverviewRow[];
}> {
  const response = await fetch(adminUrl("/api/v1/admin/overview"), {
    credentials: "include",
    headers: { Accept: "application/json" },
    cache: "no-store",
    signal,
  });
  return readJSON(response);
}

export async function fetchAdminBrokers(signal?: AbortSignal): Promise<Broker[]> {
  const response = await fetch(adminUrl("/api/v1/admin/brokers"), {
    credentials: "include",
    headers: { Accept: "application/json" },
    cache: "no-store",
    signal,
  });
  const body = await readJSON<{ brokers: Broker[] }>(response);
  return body.brokers ?? [];
}

export async function createAdminBroker(name: string): Promise<Broker> {
  const response = await fetch(adminUrl("/api/v1/admin/brokers"), {
    method: "POST",
    credentials: "include",
    headers: writeHeaders(),
    body: JSON.stringify({ name }),
  });
  return readJSON<Broker>(response);
}

export async function disableAdminBroker(brokerId: string): Promise<Broker> {
  const response = await fetch(adminUrl(`/api/v1/admin/brokers/${encodeURIComponent(brokerId)}/disable`), {
    method: "POST",
    credentials: "include",
    headers: writeHeaders(),
    body: "{}",
  });
  return readJSON<Broker>(response);
}

export async function restoreAdminBroker(brokerId: string): Promise<Broker> {
  const response = await fetch(adminUrl(`/api/v1/admin/brokers/${encodeURIComponent(brokerId)}/restore`), {
    method: "POST",
    credentials: "include",
    headers: writeHeaders(),
    body: "{}",
  });
  return readJSON<Broker>(response);
}

export async function deleteAdminBroker(brokerId: string): Promise<Broker> {
  const response = await fetch(adminUrl(`/api/v1/admin/brokers/${encodeURIComponent(brokerId)}`), {
    method: "DELETE",
    credentials: "include",
    headers: { Accept: "application/json" },
  });
  return readJSON<Broker>(response);
}

export async function resetAdminBrokerSecret(brokerId: string): Promise<Broker> {
  const response = await fetch(adminUrl(`/api/v1/admin/brokers/${encodeURIComponent(brokerId)}/secret`), {
    method: "POST",
    credentials: "include",
    headers: writeHeaders(),
    body: "{}",
  });
  return readJSON<Broker>(response);
}

export async function fetchAdminPlatformAccounts(signal?: AbortSignal): Promise<PlatformAccount[]> {
  const response = await fetch(adminUrl("/api/v1/admin/platform-accounts"), {
    credentials: "include",
    headers: { Accept: "application/json" },
    cache: "no-store",
    signal,
  });
  const body = await readJSON<{ accounts: PlatformAccount[] }>(response);
  return body.accounts ?? [];
}

export async function createAdminPlatformAccount(payload: {
  username: string;
  password: string;
  brokerId?: string;
}): Promise<PlatformAccount> {
  const response = await fetch(adminUrl("/api/v1/admin/platform-accounts"), {
    method: "POST",
    credentials: "include",
    headers: writeHeaders(),
    body: JSON.stringify(payload),
  });
  return readJSON<PlatformAccount>(response);
}

export async function disableAdminPlatformAccount(username: string): Promise<PlatformAccount> {
  const response = await fetch(
    adminUrl(`/api/v1/admin/platform-accounts/${encodeURIComponent(username)}/disable`),
    { method: "POST", credentials: "include", headers: writeHeaders(), body: "{}" },
  );
  return readJSON<PlatformAccount>(response);
}

export async function restoreAdminPlatformAccount(username: string): Promise<PlatformAccount> {
  const response = await fetch(
    adminUrl(`/api/v1/admin/platform-accounts/${encodeURIComponent(username)}/restore`),
    { method: "POST", credentials: "include", headers: writeHeaders(), body: "{}" },
  );
  return readJSON<PlatformAccount>(response);
}

export async function deleteAdminPlatformAccount(username: string): Promise<PlatformAccount> {
  const response = await fetch(
    adminUrl(`/api/v1/admin/platform-accounts/${encodeURIComponent(username)}`),
    { method: "DELETE", credentials: "include", headers: { Accept: "application/json" } },
  );
  return readJSON<PlatformAccount>(response);
}

export async function resetAdminPlatformAccountPassword(
  username: string,
  password: string,
): Promise<PlatformAccount> {
  const response = await fetch(
    adminUrl(`/api/v1/admin/platform-accounts/${encodeURIComponent(username)}/password`),
    {
      method: "POST",
      credentials: "include",
      headers: writeHeaders(),
      body: JSON.stringify({ password }),
    },
  );
  return readJSON<PlatformAccount>(response);
}

export async function updateAdminPlatformAccountBroker(
  username: string,
  brokerId: string,
): Promise<PlatformAccount> {
  const response = await fetch(
    adminUrl(`/api/v1/admin/platform-accounts/${encodeURIComponent(username)}/broker`),
    {
      method: "PATCH",
      credentials: "include",
      headers: writeHeaders(),
      body: JSON.stringify({ brokerId }),
    },
  );
  return readJSON<PlatformAccount>(response);
}
