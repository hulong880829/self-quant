"use client";

import * as React from "react";
import { usePathname, useRouter } from "next/navigation";

import {
  fetchAuthSession,
  isAuthRequiredPath,
  loginWithPassword,
  logoutSession,
  type AuthStatus,
  type AuthUser,
  type LoginPayload,
} from "@/lib/api/auth";
import { LoginDialog } from "@/components/auth/login-dialog";

interface AuthContextValue {
  status: AuthStatus;
  user: AuthUser | null;
  loginOpen: boolean;
  openLogin: (redirectTo?: string | null) => void;
  closeLogin: () => void;
  login: (payload: LoginPayload) => Promise<void>;
  logout: () => Promise<void>;
  requireAuth: (redirectTo: string) => boolean;
}

const AuthContext = React.createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const router = useRouter();
  const pathname = usePathname();
  const [status, setStatus] = React.useState<AuthStatus>("loading");
  const [user, setUser] = React.useState<AuthUser | null>(null);
  const [loginOpen, setLoginOpen] = React.useState(false);
  const redirectRef = React.useRef<string | null>(null);

  React.useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const session = await fetchAuthSession();
        if (cancelled) return;
        setUser(session);
        setStatus(session ? "authenticated" : "anonymous");
      } catch {
        if (cancelled) return;
        setUser(null);
        setStatus("anonymous");
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const openLogin = React.useCallback((redirectTo?: string | null) => {
    redirectRef.current = redirectTo ?? null;
    setLoginOpen(true);
  }, []);

  const closeLogin = React.useCallback(() => {
    setLoginOpen(false);
  }, []);

  const login = React.useCallback(async (payload: LoginPayload) => {
    const nextUser = await loginWithPassword(payload);
    setUser(nextUser);
    setStatus("authenticated");
    setLoginOpen(false);
    const target = redirectRef.current;
    redirectRef.current = null;
    if (target && target !== pathname) {
      router.push(target);
    }
  }, [pathname, router]);

  const logout = React.useCallback(async () => {
    await logoutSession();
    setUser(null);
    setStatus("anonymous");
    if (isAuthRequiredPath(pathname)) {
      router.push("/funding");
    }
  }, [pathname, router]);

  const requireAuth = React.useCallback(
    (redirectTo: string) => {
      if (status === "authenticated") return true;
      openLogin(redirectTo);
      return false;
    },
    [openLogin, status],
  );

  const value = React.useMemo<AuthContextValue>(
    () => ({
      status,
      user,
      loginOpen,
      openLogin,
      closeLogin,
      login,
      logout,
      requireAuth,
    }),
    [status, user, loginOpen, openLogin, closeLogin, login, logout, requireAuth],
  );

  return (
    <AuthContext.Provider value={value}>
      {children}
      <LoginDialog />
    </AuthContext.Provider>
  );
}

export function useAuth(): AuthContextValue {
  const value = React.useContext(AuthContext);
  if (!value) {
    throw new Error("useAuth must be used within AuthProvider");
  }
  return value;
}
