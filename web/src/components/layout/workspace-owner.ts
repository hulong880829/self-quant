import type { AuthStatus, AuthUser } from "@/lib/api/auth";

export const KEEP_ALIVE_PATHS = ["/funding", "/trading"] as const;

export type KeepAlivePath = (typeof KEEP_ALIVE_PATHS)[number];

export function isKeepAlivePath(pathname: string): pathname is KeepAlivePath {
  return pathname === "/funding" || pathname === "/trading";
}

export function workspaceIdentity(
  status: AuthStatus,
  user: Pick<AuthUser, "username"> | null,
): string | null {
  if (status === "loading") return null;
  const username = user?.username?.trim();
  return username ? `user:${username}` : "anonymous";
}
