export type AuthStatus = "loading" | "anonymous" | "authenticated";

export interface AuthUser {
  username: string;
  permission: string;
}

export interface AuthSessionResponse {
  authenticated: boolean;
  username?: string;
  permission?: string;
  error?: string;
}

export interface LoginPayload {
  username: string;
  password: string;
}

export const AUTH_REQUIRED_PATHS = [
  "/trading",
  "/accounts",
  "/reports",
  "/ai-settings",
] as const;

export function isAuthRequiredPath(pathname: string | null | undefined): boolean {
  if (!pathname) return false;
  return AUTH_REQUIRED_PATHS.some(
    (path) => pathname === path || pathname.startsWith(`${path}/`),
  );
}

function apiBaseUrl(): string {
  return (process.env.NEXT_PUBLIC_API_BASE_URL ?? "").replace(/\/+$/, "");
}

function authUrl(path: string): string {
  return `${apiBaseUrl()}${path}`;
}

export function parseAuthSession(payload: AuthSessionResponse): AuthUser | null {
  if (!payload.authenticated) return null;
  const username = payload.username?.trim();
  if (!username) return null;
  return {
    username,
    permission: payload.permission?.trim() || "user",
  };
}

export async function fetchAuthSession(): Promise<AuthUser | null> {
  const response = await fetch(authUrl("/api/v1/auth/session"), {
    method: "GET",
    credentials: "include",
    headers: { Accept: "application/json" },
    cache: "no-store",
  });
  if (!response.ok) {
    throw new Error("无法获取登录状态");
  }
  const payload = (await response.json()) as AuthSessionResponse;
  return parseAuthSession(payload);
}

export async function loginWithPassword(
  payload: LoginPayload,
): Promise<AuthUser> {
  const response = await fetch(authUrl("/api/v1/auth/login"), {
    method: "POST",
    credentials: "include",
    headers: {
      Accept: "application/json",
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      username: payload.username.trim(),
      password: payload.password,
    }),
  });
  const body = (await response.json().catch(() => ({}))) as AuthSessionResponse;
  if (!response.ok) {
    throw new Error(
      body.error === "invalid credentials"
        ? "账户名或密码错误"
        : body.error || "登录失败，请稍后重试",
    );
  }
  const user = parseAuthSession(body);
  if (!user) {
    throw new Error("登录响应无效");
  }
  return user;
}

export async function logoutSession(): Promise<void> {
  const response = await fetch(authUrl("/api/v1/auth/logout"), {
    method: "POST",
    credentials: "include",
    headers: { Accept: "application/json" },
  });
  if (!response.ok) {
    throw new Error("退出登录失败");
  }
}
