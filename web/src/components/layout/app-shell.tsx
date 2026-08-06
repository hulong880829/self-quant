"use client";

import * as React from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { useTheme } from "next-themes";
import {
  BarChart3,
  BookOpen,
  Bot,
  ChevronDown,
  CircleDollarSign,
  CircleUserRound,
  Moon,
  ShieldCheck,
  Sun,
  TrendingUp,
  WalletCards,
  Zap,
} from "lucide-react";

import { ResearchAssistant } from "@/components/assistant/research-assistant";
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
import { cn } from "@/lib/utils";

const navigation = [
  { href: "/a-shares", label: "国内大A股", icon: BarChart3 },
  { href: "/domestic-futures", label: "国内期货", icon: TrendingUp },
  { href: "/crypto-options", label: "Crypto 期权", icon: CircleDollarSign },
  { href: "/funding", label: "Crypto 资金费", icon: Zap },
  { href: "/orderbook", label: "聚合盘口", icon: BookOpen },
  { href: "/trading", label: "实盘交易", icon: ShieldCheck },
  { href: "/accounts", label: "账户管理", icon: WalletCards },
  { href: "/reports", label: "报表分析", icon: BarChart3 },
];

export function AppShell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const { resolvedTheme, setTheme } = useTheme();
  const [assistantOpen, setAssistantOpen] = React.useState(true);

  return (
    <FundingProvider>
      <div className="min-h-screen">
      <header className="sticky top-0 z-40 border-b bg-background/88 backdrop-blur-xl">
        <div className="flex h-16 items-center gap-4 px-4 lg:px-6">
          <Link href="/funding" className="flex shrink-0 items-center gap-2">
            <span className="flex size-8 items-center justify-center rounded-lg bg-primary text-primary-foreground shadow-sm shadow-primary/20">
              <Bot className="size-4.5" />
            </span>
            <span className="hidden leading-none sm:block">
              <span className="block text-sm font-semibold tracking-tight">
                Nuts Quant
              </span>
              <span className="mt-1 block text-[9px] uppercase tracking-[0.26em] text-muted-foreground">
                Market Terminal
              </span>
            </span>
          </Link>

          <nav className="flex min-w-0 flex-1 items-center gap-1 overflow-x-auto px-1 lg:justify-center">
            {navigation.map((item) => {
              const active = pathname === item.href;
              const Icon = item.icon;
              return (
                <Link
                  key={item.href}
                  href={item.href}
                  className={cn(
                    "relative flex h-9 shrink-0 items-center gap-2 rounded-md px-3 text-sm transition-colors",
                    active
                      ? "bg-primary/10 font-medium text-primary"
                      : "text-muted-foreground hover:bg-muted hover:text-foreground",
                  )}
                >
                  <Icon className="size-4" />
                  {item.label}
                  {active && (
                    <span className="absolute inset-x-3 -bottom-[14px] h-0.5 rounded-full bg-primary" />
                  )}
                </Link>
              );
            })}
          </nav>

          <div className="flex shrink-0 items-center gap-1.5">
            <div className="hidden items-center gap-2 rounded-full border bg-card px-2.5 py-1.5 text-xs text-muted-foreground md:flex">
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
            <DropdownMenu>
              <DropdownMenuTrigger
                render={
                  <Button variant="ghost" className="gap-2 px-2">
                    <CircleUserRound className="size-5" />
                    <span className="hidden text-xs lg:inline">量化账户</span>
                    <ChevronDown className="hidden size-3 text-muted-foreground lg:inline" />
                  </Button>
                }
              />
              <DropdownMenuContent align="end" className="w-48">
                <DropdownMenuGroup>
                  <DropdownMenuLabel>演示工作区</DropdownMenuLabel>
                </DropdownMenuGroup>
                <DropdownMenuSeparator />
                <DropdownMenuItem>个人设置</DropdownMenuItem>
                <DropdownMenuItem>API 管理</DropdownMenuItem>
                <DropdownMenuSeparator />
                <DropdownMenuItem>退出登录</DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
        </div>
      </header>

      <main
        className={cn(
          "w-full p-3 transition-[padding] duration-200 sm:p-4 lg:p-6",
          assistantOpen ? "2xl:pr-[366px]" : "2xl:pr-[72px]",
        )}
      >
        <div className="mx-auto w-full max-w-[1920px]">{children}</div>
      </main>
      <ResearchAssistant
        desktopOpen={assistantOpen}
        onDesktopOpenChange={setAssistantOpen}
      />
      </div>
    </FundingProvider>
  );
}
