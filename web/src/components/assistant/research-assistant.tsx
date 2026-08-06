"use client";

import * as React from "react";
import {
  Bot,
  BrainCircuit,
  ChevronRight,
  MessageCircle,
  PanelRightClose,
  PanelRightOpen,
  Send,
  ShieldAlert,
  Sparkles,
  Square,
  Trash2,
  Wrench,
} from "lucide-react";

import { useFundingSnapshot } from "@/components/funding/funding-provider";
import { Button } from "@/components/ui/button";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import {
  formatCurrency,
  formatFundingRate,
  formatPercent,
} from "@/lib/market-format";
import { cn } from "@/lib/utils";
import type { FundingOpportunity } from "@/types/market";

type MessageRole = "assistant" | "user";

interface AssistantMessage {
  id: string;
  role: MessageRole;
  content: string;
  tool?: string;
}

const welcomeMessage: AssistantMessage = {
  id: "welcome",
  role: "assistant",
  content:
    "你好，我是 Nuts Quant 投研助手。我可以基于当前资金费快照，帮助你筛选机会、解释指标和梳理风险。",
};

const suggestedPrompts = [
  "当前最高资金费机会",
  "分析 BTC 资金费",
  "负费率有什么风险",
];

function buildAnswer(
  question: string,
  fundingOpportunities: FundingOpportunity[],
) {
  const normalized = question.toLowerCase();
  if (fundingOpportunities.length === 0) {
    return {
      tool: "当前无可用资金费快照",
      content:
        "资金费实时快照尚未加载成功，暂时无法给出基于市场数据的排名或币种分析。请稍后重试；在数据恢复前，我不会使用旧的模拟数据替代实时结果。",
    };
  }
  const ranked = [...fundingOpportunities].sort(
    (left, right) => right.annualizedRate - left.annualizedRate,
  );
  const best = ranked[0]!;
  const btc = fundingOpportunities.find((item) => item.baseAsset === "BTC");
  const negative = fundingOpportunities.filter(
    (item) => item.nextFundingRate !== null && item.nextFundingRate < 0,
  );

  if (normalized.includes("btc") || normalized.includes("比特币")) {
    if (!btc) {
      return {
        tool: "已读取当前资金费快照",
        content:
          "当前实时快照中没有 BTC 合约，因此无法生成 BTC 费率分析。你可以询问当前最高资金费机会，或等待下一次快照刷新。",
      };
    }
    return {
      tool: "已读取 BTC/USDT 资金费快照",
      content: `${btc.symbol} 当前价格为 ${formatCurrency(btc.latestPrice)}，预计下次资金费率 ${formatFundingRate(btc.nextFundingRate)}，折算年化约 ${formatPercent(btc.annualizedRate, 1)}。\n\n单看费率收益并不足以构成交易依据，还需要扣除双边手续费、滑点并关注现货与永续基差。`,
    };
  }

  if (
    normalized.includes("负") ||
    normalized.includes("风险") ||
    normalized.includes("做多")
  ) {
    const symbols = negative.map((item) => item.symbol).join("、");
    return {
      tool: `已筛选 ${negative.length} 个负费率合约`,
      content: `当前快照中的负费率合约包括 ${symbols || "暂无"}。负费率意味着空头向多头支付资金费，但不等于直接做多就能获利。\n\n主要风险包括价格单边下跌、费率在结算前反转、盘口深度不足，以及跨所对冲时出现单腿暴露。建议先检查成交深度和下一结算周期预测。`,
    };
  }

  if (
    normalized.includes("最高") ||
    normalized.includes("机会") ||
    normalized.includes("排名")
  ) {
    return {
      tool: "已调用资金费机会排行",
      content: `当前快照中，${best.exchange} 的 ${best.symbol} 年化资金费率最高，约为 ${formatPercent(best.annualizedRate, 1)}；预计下次费率为 ${formatFundingRate(best.nextFundingRate)}，持仓名义价值约 ${formatCurrency(best.positionNotional, true)}。\n\n高年化可能由短时拥挤造成。执行前应重点核对盘口深度、费率持续性和可用保证金。`,
    };
  }

  return {
    tool: "已读取当前资金费市场快照",
    content:
      "我已分析当前实时快照。你可以进一步指定交易所、币种或费率方向，例如“筛选 Hyperliquid 正费率机会”或“分析 BTC 资金费风险”。\n\n助手只读取市场快照，不读取账户，也不会提交任何交易。",
  };
}

function AssistantBody({
  onCollapse,
  showCollapse,
}: {
  onCollapse?: () => void;
  showCollapse?: boolean;
}) {
  const { snapshot, loading, error } = useFundingSnapshot();
  const fundingOpportunities = snapshot?.data ?? [];
  const [messages, setMessages] = React.useState<AssistantMessage[]>([
    welcomeMessage,
  ]);
  const [input, setInput] = React.useState("");
  const [streaming, setStreaming] = React.useState(false);
  const streamTimer = React.useRef<ReturnType<typeof setInterval> | null>(null);
  const messageEnd = React.useRef<HTMLDivElement>(null);
  const messageCounter = React.useRef(0);

  const stopStreaming = React.useCallback(() => {
    if (streamTimer.current) {
      clearInterval(streamTimer.current);
      streamTimer.current = null;
    }
    setStreaming(false);
  }, []);

  React.useEffect(() => stopStreaming, [stopStreaming]);

  React.useEffect(() => {
    messageEnd.current?.scrollIntoView({ behavior: "smooth", block: "nearest" });
  }, [messages]);

  const ask = (rawQuestion: string) => {
    const question = rawQuestion.trim();
    if (!question || streaming) return;

    const response = buildAnswer(question, fundingOpportunities);
    messageCounter.current += 1;
    const messageId = messageCounter.current;
    const userMessage: AssistantMessage = {
      id: `user-${messageId}`,
      role: "user",
      content: question,
    };
    const assistantId = `assistant-${messageId}`;

    setInput("");
    setStreaming(true);
    setMessages((current) => [
      ...current,
      userMessage,
      {
        id: assistantId,
        role: "assistant",
        content: "",
        tool: response.tool,
      },
    ]);

    const chunks = Array.from(
      { length: Math.ceil(response.content.length / 8) },
      (_, index) => response.content.slice(index * 8, index * 8 + 8),
    );
    let chunkIndex = 0;
    streamTimer.current = setInterval(() => {
      const chunk = chunks[chunkIndex];
      if (chunk === undefined) {
        stopStreaming();
        return;
      }
      setMessages((current) =>
        current.map((message) =>
          message.id === assistantId
            ? { ...message, content: message.content + chunk }
            : message,
        ),
      );
      chunkIndex += 1;
    }, 45);
  };

  const clearConversation = () => {
    stopStreaming();
    setMessages([welcomeMessage]);
  };

  return (
    <div className="flex h-full min-h-0 flex-col bg-card">
      <div className="flex h-16 shrink-0 items-center justify-between border-b px-4">
        <div className="flex min-w-0 items-center gap-2.5">
          <span className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-primary/12 text-primary">
            <BrainCircuit className="size-4" />
          </span>
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <span className="truncate text-sm font-semibold">AI 投研助手</span>
              <span className="flex items-center gap-1 text-[10px] text-positive">
                <span
                  className={cn(
                    "size-1.5 rounded-full",
                    error ? "bg-amber-500" : "bg-emerald-500",
                  )}
                />
                {loading ? "Loading" : error ? "Stale" : "Ready"}
              </span>
            </div>
            <div className="mt-0.5 text-[10px] text-muted-foreground">
              实时快照 · 只读模式
            </div>
          </div>
        </div>
        <div className="flex items-center gap-1">
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label="清空对话"
            onClick={clearConversation}
          >
            <Trash2 className="size-3.5" />
          </Button>
          {showCollapse && (
            <Button
              variant="ghost"
              size="icon-sm"
              aria-label="收起 AI 助手"
              onClick={onCollapse}
            >
              <PanelRightClose className="size-3.5" />
            </Button>
          )}
        </div>
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto p-3">
        <div className="space-y-4">
          {messages.map((message, index) => {
            const isLast = index === messages.length - 1;
            return (
              <div
                key={message.id}
                className={cn(
                  "flex gap-2",
                  message.role === "user" && "justify-end",
                )}
              >
                {message.role === "assistant" && (
                  <span className="mt-0.5 flex size-6 shrink-0 items-center justify-center rounded-full bg-primary/10 text-primary">
                    <Bot className="size-3.5" />
                  </span>
                )}
                <div
                  className={cn(
                    "max-w-[88%]",
                    message.role === "user" && "order-first",
                  )}
                >
                  {message.tool && (
                    <div className="mb-1.5 inline-flex items-center gap-1 rounded border bg-muted/60 px-2 py-1 text-[9px] text-muted-foreground">
                      <Wrench className="size-2.5" />
                      {message.tool}
                    </div>
                  )}
                  <div
                    className={cn(
                      "whitespace-pre-wrap rounded-xl px-3 py-2.5 text-xs leading-5",
                      message.role === "assistant"
                        ? "rounded-tl-sm border bg-background"
                        : "rounded-tr-sm bg-primary text-primary-foreground",
                    )}
                  >
                    {message.content}
                    {streaming &&
                      isLast &&
                      message.role === "assistant" && (
                        <span className="ml-0.5 inline-block h-3 w-0.5 animate-pulse bg-primary align-middle" />
                      )}
                  </div>
                </div>
              </div>
            );
          })}
          <div ref={messageEnd} />
        </div>
      </div>

      <div className="shrink-0 border-t p-3">
        {messages.length === 1 && (
          <div className="mb-3 space-y-1.5">
            <div className="flex items-center gap-1 text-[10px] text-muted-foreground">
              <Sparkles className="size-3" />
              试试这样问
            </div>
            {suggestedPrompts.map((prompt) => (
              <button
                type="button"
                key={prompt}
                onClick={() => ask(prompt)}
                className="flex w-full items-center justify-between rounded-md border bg-background px-2.5 py-2 text-left text-[11px] transition-colors hover:border-primary/35 hover:bg-primary/5"
              >
                {prompt}
                <ChevronRight className="size-3 text-muted-foreground" />
              </button>
            ))}
          </div>
        )}

        <div className="relative rounded-lg border bg-background focus-within:border-primary/45 focus-within:ring-2 focus-within:ring-primary/10">
          <textarea
            value={input}
            rows={2}
            disabled={streaming}
            placeholder="询问资金费、市场机会或风险..."
            onChange={(event) => setInput(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Enter" && !event.shiftKey) {
                event.preventDefault();
                ask(input);
              }
            }}
            className="block w-full resize-none bg-transparent px-3 py-2.5 pr-11 text-xs leading-5 outline-none placeholder:text-muted-foreground disabled:opacity-60"
          />
          <Button
            size="icon-sm"
            className="absolute right-2 bottom-2"
            aria-label={streaming ? "停止生成" : "发送消息"}
            onClick={() => (streaming ? stopStreaming() : ask(input))}
            disabled={!streaming && !input.trim()}
          >
            {streaming ? (
              <Square className="size-3 fill-current" />
            ) : (
              <Send className="size-3" />
            )}
          </Button>
        </div>
        <div className="mt-2 flex items-center justify-center gap-1 text-[9px] text-muted-foreground">
          <ShieldAlert className="size-2.5" />
          快照分析仅供研究参考，不构成投资建议
        </div>
      </div>
    </div>
  );
}

export function ResearchAssistant({
  desktopOpen,
  onDesktopOpenChange,
}: {
  desktopOpen: boolean;
  onDesktopOpenChange: (open: boolean) => void;
}) {
  const [mobileOpen, setMobileOpen] = React.useState(false);

  return (
    <>
      {desktopOpen ? (
        <aside className="fixed top-16 right-0 bottom-0 z-30 hidden w-[350px] border-l shadow-[-10px_0_30px_-24px_rgba(0,0,0,0.5)] 2xl:block">
          <AssistantBody
            showCollapse
            onCollapse={() => onDesktopOpenChange(false)}
          />
        </aside>
      ) : (
        <aside className="fixed top-16 right-0 bottom-0 z-30 hidden w-14 border-l bg-card 2xl:flex 2xl:flex-col 2xl:items-center">
          <Button
            variant="ghost"
            size="icon"
            className="mt-3"
            aria-label="展开 AI 投研助手"
            onClick={() => onDesktopOpenChange(true)}
          >
            <PanelRightOpen className="size-4" />
          </Button>
          <div className="mt-4 flex flex-col items-center gap-2 text-primary">
            <BrainCircuit className="size-4" />
            <span className="text-[10px] font-medium [writing-mode:vertical-rl]">
              AI 投研助手
            </span>
          </div>
        </aside>
      )}

      <Button
        size="lg"
        className="fixed right-4 bottom-4 z-40 gap-2 rounded-full shadow-lg 2xl:hidden"
        onClick={() => setMobileOpen(true)}
      >
        <MessageCircle className="size-4" />
        AI 投研助手
      </Button>

      <Sheet open={mobileOpen} onOpenChange={setMobileOpen}>
        <SheetContent className="w-full gap-0 p-0 sm:max-w-[420px] 2xl:hidden">
          <SheetHeader className="sr-only">
            <SheetTitle>AI 投研助手</SheetTitle>
            <SheetDescription>静态可交互的量化投研助手</SheetDescription>
          </SheetHeader>
          <AssistantBody />
        </SheetContent>
      </Sheet>
    </>
  );
}
