"use client";

import * as React from "react";
import {
  ChevronRight,
  Clock,
  ShieldCheck,
  Zap,
} from "lucide-react";

import { AuthGate } from "@/components/auth/auth-gate";
import { ArbitrageTradingView } from "@/components/trading/arbitrage-trading";
import { ManualTradingView } from "@/components/trading/manual-trading";
import { TwapTradingView } from "@/components/trading/twap-trading";
import { PageFrame, WorkspacePanel } from "@/components/layout/responsive";
import { cn } from "@/lib/utils";

type TradingView = "manual" | "algorithm" | "arbitrage";

const tradingViews: Array<{
  id: TradingView;
  label: string;
  description: string;
  icon: typeof ShieldCheck;
}> = [
  { id: "manual", label: "手动交易", description: "标准订单录入", icon: ShieldCheck },
  { id: "algorithm", label: "TWAP", description: "定时均匀拆单执行", icon: Clock },
  { id: "arbitrage", label: "套利交易", description: "多腿策略执行", icon: Zap },
];

export default function TradingPage() {
  const [activeView, setActiveView] = React.useState<TradingView>("manual");

  return (
    <AuthGate
      redirectTo="/trading"
      title="需要登录"
      description="实盘交易涉及下单与风险确认，请先登录后再访问。"
    >
      <PageFrame>
        <div>
          <div className="text-[11px] font-semibold tracking-[0.22em] text-primary">
            LIVE EXECUTION
          </div>
          <h1 className="mt-1 text-2xl font-semibold tracking-tight">实盘交易</h1>
        </div>

        <WorkspacePanel className="flex-1 lg:grid-cols-[230px_minmax(0,1fr)]">
          <aside className="min-h-0 overflow-y-auto border-b bg-muted/10 lg:border-r lg:border-b-0">
            <div className="border-b px-4 py-3">
              <div className="text-xs font-medium">交易模式</div>
              <div className="mt-1 text-[11px] text-muted-foreground">
                选择订单执行方式
              </div>
            </div>
            <nav className="grid gap-1 p-2">
              {tradingViews.map((view) => {
                const Icon = view.icon;
                const active = view.id === activeView;
                return (
                  <button
                    key={view.id}
                    type="button"
                    onClick={() => setActiveView(view.id)}
                    className={cn(
                      "flex w-full items-center gap-3 rounded-lg px-3 py-3 text-left transition-colors",
                      active
                        ? "bg-primary/[0.09] text-primary"
                        : "text-muted-foreground hover:bg-muted hover:text-foreground",
                    )}
                  >
                    <span
                      className={cn(
                        "flex size-8 shrink-0 items-center justify-center rounded-lg border bg-background",
                        active && "border-primary/30 bg-primary/10",
                      )}
                    >
                      <Icon className="size-4" />
                    </span>
                    <span className="min-w-0">
                      <span className="block truncate text-sm font-medium">
                        {view.label}
                      </span>
                      <span className="block truncate text-[11px] opacity-75">
                        {view.description}
                      </span>
                    </span>
                    <ChevronRight className="ml-auto size-3.5 shrink-0 opacity-50" />
                  </button>
                );
              })}
            </nav>
          </aside>

          <main className="min-h-0 min-w-0 overflow-y-auto" data-trading-scroll>
            {activeView === "manual" ? <ManualTradingView /> : null}
            {activeView === "algorithm" ? <TwapTradingView /> : null}
            {activeView === "arbitrage" ? <ArbitrageTradingView /> : null}
          </main>
        </WorkspacePanel>
      </PageFrame>
    </AuthGate>
  );
}
