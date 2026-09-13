"use client";

import * as React from "react";
import dynamic from "next/dynamic";
import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useTheme } from "next-themes";
import {
  BarChart3,
  BookOpen,
  Bot,
  ChevronDown,
  CircleDollarSign,
  CircleUserRound,
  Globe2,
  Moon,
  ShieldCheck,
  Sparkles,
  Sun,
  WalletCards,
  Zap,
} from "lucide-react";

import { AuthProvider, useAuth } from "@/components/auth/auth-provider";
import { PersistentWorkspaceBoundary } from "@/components/layout/persistent-workspace-boundary";
import { FundingDashboard } from "@/components/funding/funding-dashboard";
import { TradingWorkspace } from "@/app/trading/page";
import {
  AssistantProvider,
  clearAssistantConversationSession,
} from "@/components/assistant/assistant-provider";
import { AIStatusProvider } from "@/components/assistant/ai-status-provider";
import { FundingProvider } from "@/components/funding/funding-provider";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { isAuthRequiredPath } from "@/lib/api/auth";
import { cn } from "@/lib/utils";

const navigation = [
  { href: "/global-stocks", label: "全球股票", icon: Globe2 },
  { href: "/crypto-options", label: "Polymarket", icon: CircleDollarSign },
  { href: "/funding", label: "Crypto 资金费", icon: Zap },
  { href: "/orderbook", label: "聚合盘口", icon: BookOpen },
  { href: "/trading", label: "实盘交易", icon: ShieldCheck },
  { href: "/accounts", label: "账户管理", icon: WalletCards },
  { href: "/reports", label: "报表分析", icon: BarChart3 },
  { href: "/ai-settings", label: "AI 设置", icon: Sparkles },
];

function ResearchAssistantPlaceholder() {
  return (
    <div
      aria-label="投研助手加载中"
      className="fixed inset-y-16 right-0 hidden w-14 border-l bg-card/70 assistant-dock:block"
    />
  );
}

const ResearchAssistant = dynamic(
  () =>
    import("@/components/assistant/research-assistant").then(
      (module) => module.ResearchAssistant,
    ),
  {
    ssr: false,
    loading: ResearchAssistantPlaceholder,
  },
);

interface NavigationLinksProps {
  pathname: string;
  status: ReturnType<typeof useAuth>["status"];
  requireAuth: ReturnType<typeof useAuth>["requireAuth"];
}

export function NavigationLinks({
  pathname,
  status,
  requireAuth,
}: NavigationLinksProps) {
  const router = useRouter();
  const [pendingHref, setPendingHref] = React.useState<string | null>(null);
  const activePath = pendingHref ?? pathname;

  return (
    <>
      {navigation.map((item) => {
        const active = activePath === item.href;
        const pending = pendingHref === item.href;
        const Icon = item.icon;
        const guarded = isAuthRequiredPath(item.href);
        return (
          <Link
            key={item.href}
            href={item.href}
            prefetch
            aria-current={pathname === item.href ? "page" : undefined}
            aria-busy={pending || undefined}
            onMouseEnter={() => router.prefetch(item.href)}
            onFocus={() => router.prefetch(item.href)}
            onClick={(event) => {
              if (
                event.button !== 0 ||
                event.metaKey ||
                event.ctrlKey ||
                event.shiftKey ||
                event.altKey
              ) {
                return;
              }
              if (guarded && status !== "authenticated") {
                event.preventDefault();
                if (status !== "loading") requireAuth(item.href);
                return;
              }
              setPendingHref(item.href === pathname ? null : item.href);
            }}
            className={cn(
              "relative flex h-9 shrink-0 items-center gap-1.5 rounded-md px-2 text-[13px] transition-colors xl:gap-2 xl:px-3 xl:text-sm",
              active
                ? "bg-primary/10 font-medium text-primary"
                : "text-muted-foreground hover:bg-muted hover:text-foreground",
            )}
          >
            <Icon className={cn("size-4", pending && "animate-pulse")} />
            {item.label}
            {active && (
              <span
                className={cn(
                  "absolute inset-x-3 -bottom-[14px] h-0.5 rounded-full bg-primary",
                  pending && "animate-pulse",
                )}
              />
            )}
          </Link>
        );
      })}
    </>
  );
}

function AccountMenu() {
  const { status, user, openLogin, logout } = useAuth();

  if (status === "loading") {
    return (
      <Button variant="ghost" className="gap-2 px-2" disabled>
        <CircleUserRound className="size-5" />
        <span className="hidden text-xs xl:inline">加载中…</span>
      </Button>
    );
  }

  if (status !== "authenticated" || !user) {
    return (
      <Button
        variant="ghost"
        className="gap-2 px-2"
        onClick={() => openLogin()}
      >
        <CircleUserRound className="size-5" />
        <span className="hidden text-xs xl:inline">登录</span>
      </Button>
    );
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <Button variant="ghost" className="gap-2 px-2">
            <CircleUserRound className="size-5" />
            <span className="hidden text-xs xl:inline">{user.username}</span>
            <ChevronDown className="hidden size-3 text-muted-foreground xl:inline" />
          </Button>
        }
      />
      <DropdownMenuContent align="end" className="w-48">
        <DropdownMenuGroup>
          <DropdownMenuLabel>
            {user.username}
            <span className="mt-1 block text-[10px] font-normal uppercase tracking-wider text-muted-foreground">
              {user.permission}
            </span>
          </DropdownMenuLabel>
        </DropdownMenuGroup>
        <DropdownMenuSeparator />
        <DropdownMenuItem
          onClick={() => {
            clearAssistantConversationSession(user.username);
            void logout().catch(() => undefined);
          }}
        >
          退出登录
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function KeepAliveFundingPage() {
  return <FundingDashboard />;
}

function KeepAliveTradingPage() {
  return (
    <React.Suspense
      fallback={
        <div className="flex min-h-[var(--app-page-min-height)] items-center justify-center text-sm text-muted-foreground">
          正在加载实盘交易…
        </div>
      }
    >
      <TradingWorkspace />
    </React.Suspense>
  );
}

function AppShellInner({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const { resolvedTheme, setTheme } = useTheme();
  const [assistantOpen, setAssistantOpen] = React.useState(true);
  const [assistantReady, setAssistantReady] = React.useState(false);
  const { status, user, requireAuth } = useAuth();

  React.useEffect(() => {
    const idleAPI = window as unknown as {
      requestIdleCallback?: (
        callback: IdleRequestCallback,
        options?: IdleRequestOptions,
      ) => number;
      cancelIdleCallback?: (id: number) => void;
    };
    if (idleAPI.requestIdleCallback) {
      const id = idleAPI.requestIdleCallback(() => setAssistantReady(true), {
        timeout: 1_500,
      });
      return () => idleAPI.cancelIdleCallback?.(id);
    }
    const timer = window.setTimeout(() => setAssistantReady(true), 300);
    return () => window.clearTimeout(timer);
  }, []);

  return (
    <FundingProvider>
      <AssistantProvider key={user?.username ?? status}>
        <div className="min-h-dvh min-w-0 max-w-full overflow-x-clip">
        <header className="sticky top-0 z-40 h-16 border-b bg-background/88 backdrop-blur-xl">
          <div className="flex h-16 items-center gap-2 px-3 xl:gap-4 xl:px-6">
            <Link href="/funding" className="flex shrink-0 items-center gap-2">
              <span className="flex size-8 items-center justify-center rounded-lg bg-primary text-primary-foreground shadow-sm shadow-primary/20">
                <Bot className="size-4.5" />
              </span>
              <span className="hidden leading-none sm:block">
                <span className="block text-sm font-semibold tracking-tight">
                  龙歪歪Quant
                </span>
                <span className="mt-1 hidden text-[9px] uppercase tracking-[0.26em] text-muted-foreground xl:block">
                  Market Terminal
                </span>
              </span>
            </Link>

            <nav
              data-nav-scroll
              className="flex min-w-0 flex-1 items-center justify-start gap-0.5 overflow-x-auto overscroll-x-contain px-1 [scrollbar-width:none] [-ms-overflow-style:none] [&::-webkit-scrollbar]:hidden xl:gap-1"
            >
              <NavigationLinks
                key={pathname}
                pathname={pathname}
                status={status}
                requireAuth={requireAuth}
              />
            </nav>

            <div className="flex shrink-0 items-center gap-1 xl:gap-1.5">
              <div className="hidden items-center gap-2 rounded-full border bg-card px-2.5 py-1.5 text-xs text-muted-foreground xl:flex">
                <span className="relative flex size-2">
                  <span className="absolute inline-flex size-full animate-ping rounded-full bg-emerald-400 opacity-50" />
                  <span className="relative inline-flex size-2 rounded-full bg-emerald-500" />
                </span>
                实时 API
              </div>
              <Button
                variant="ghost"
                size="icon"
                aria-label="切换主题"
                onClick={() =>
                  setTheme(resolvedTheme === "dark" ? "light" : "dark")
                }
              >
                <Sun className="hidden size-4 dark:block" />
                <Moon className="size-4 dark:hidden" />
              </Button>
              <AccountMenu />
            </div>
          </div>
        </header>

        <main
          className={cn(
            "w-full min-w-0 max-w-full p-3 transition-[padding] duration-200 sm:p-4 xl:p-6",
            assistantOpen ? "assistant-dock:pr-[366px]" : "assistant-dock:pr-[72px]",
          )}
        >
          <div className="mx-auto w-full min-w-0 max-w-[1920px]">
            <PersistentWorkspaceBoundary
              fundingPage={<KeepAliveFundingPage />}
              tradingPage={<KeepAliveTradingPage />}
            >
              {children}
            </PersistentWorkspaceBoundary>
          </div>
        </main>
        {assistantReady ? (
          <ResearchAssistant
            desktopOpen={assistantOpen}
            onDesktopOpenChange={setAssistantOpen}
          />
        ) : (
          <ResearchAssistantPlaceholder />
        )}
        </div>
      </AssistantProvider>
    </FundingProvider>
  );
}

export function AppShell({ children }: { children: React.ReactNode }) {
  return (
    <AuthProvider>
      <AIStatusProvider>
        <AppShellInner>{children}</AppShellInner>
      </AIStatusProvider>
    </AuthProvider>
  );
}
