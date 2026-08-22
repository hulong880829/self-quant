"use client";

import * as React from "react";
import Link from "next/link";
import {
  Bot,
  BrainCircuit,
  ChevronRight,
  LoaderCircle,
  MessageCircle,
  PanelRightClose,
  PanelRightOpen,
  Pencil,
  Plus,
  RotateCcw,
  Send,
  ShieldAlert,
  Sparkles,
  Square,
  Trash2,
  Wrench,
} from "lucide-react";

import { useAssistant } from "@/components/assistant/assistant-provider";
import { useAIStatus } from "@/components/assistant/ai-status-provider";
import { Button } from "@/components/ui/button";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { cn } from "@/lib/utils";

const welcomeContent =
  "你好，我是龙歪歪Quant投研助手。我可以基于当前资金费快照，帮助你筛选机会、解释指标和梳理风险。";

const suggestedPrompts = [
  "当前最高资金费机会",
  "分析 BTC 资金费",
  "负费率有什么风险",
];

function ConversationRail() {
  const {
    conversations,
    currentConversationId,
    currentConversation,
    selectConversation,
    renameConversation,
    removeConversation,
  } = useAssistant();

  const renameCurrent = () => {
    if (!currentConversation) return;
    const title = window.prompt("输入新对话名称", currentConversation.title);
    if (title?.trim()) void renameConversation(currentConversation.id, title);
  };

  const deleteCurrent = () => {
    if (!currentConversation) return;
    if (window.confirm(`删除“${currentConversation.title || "新对话"}”？`)) {
      void removeConversation(currentConversation.id);
    }
  };

  return (
    <aside
      aria-label="AI 会话列表"
      className="flex w-12 shrink-0 flex-col items-center gap-2 border-r bg-muted/15 py-3"
    >
      {conversations.slice(0, 5).map((conversation, index) => (
        <button
          type="button"
          key={conversation.id}
          title={`会话 ${index + 1}：${conversation.title || "新对话"}`}
          aria-label={`切换到会话 ${index + 1}：${conversation.title || "新对话"}`}
          aria-current={
            conversation.id === currentConversationId ? "true" : undefined
          }
          onClick={() => void selectConversation(conversation.id)}
          className={cn(
            "flex size-8 shrink-0 items-center justify-center rounded-lg border text-xs font-medium transition-colors",
            conversation.id === currentConversationId
              ? "border-primary bg-primary text-primary-foreground"
              : "bg-background text-muted-foreground hover:border-primary/40 hover:text-foreground",
          )}
        >
          {index + 1}
        </button>
      ))}
      {currentConversation && (
        <div className="mt-auto flex flex-col gap-1 border-t pt-2">
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label="重命名当前对话"
            title="重命名当前对话"
            onClick={renameCurrent}
          >
            <Pencil className="size-3" />
          </Button>
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label="删除当前对话"
            title="删除当前对话"
            onClick={deleteCurrent}
          >
            <Trash2 className="size-3 text-destructive" />
          </Button>
        </div>
      )}
    </aside>
  );
}

function AssistantBody({
  onCollapse,
  showCollapse,
  reserveCloseSpace,
}: {
  onCollapse?: () => void;
  showCollapse?: boolean;
  reserveCloseSpace?: boolean;
}) {
  const { availability } = useAIStatus();
  const {
    messages,
    draft,
    setDraft,
    loadingMessages,
    loadingMore,
    hasMoreMessages,
    streaming,
    error,
    currentConversation,
    loadingConversations,
    startNewConversation,
    loadMoreMessages,
    sendMessage,
    retryMessage,
    stopStreaming,
  } = useAssistant();
  const assistantReady = availability === "valid";
  const messageEnd = React.useRef<HTMLDivElement>(null);
  const messageScroll = React.useRef<HTMLDivElement>(null);
  const preservingHistoryScroll = React.useRef(false);
  const previousMessageCount = React.useRef(0);

  const statusText =
    availability === "loading"
      ? "Loading"
      : assistantReady
        ? "Ready"
        : availability === "anonymous"
          ? "Login"
          : availability === "unconfigured"
            ? "No Key"
            : availability === "invalid"
              ? "Invalid Key"
              : "Unavailable";

  React.useEffect(() => {
    if (
      messages.length >= previousMessageCount.current &&
      !preservingHistoryScroll.current
    ) {
      messageEnd.current?.scrollIntoView({ behavior: "smooth", block: "nearest" });
    }
    previousMessageCount.current = messages.length;
  }, [messages]);

  const handleLoadMore = React.useCallback(async () => {
    const element = messageScroll.current;
    const previousHeight = element?.scrollHeight ?? 0;
    const previousTop = element?.scrollTop ?? 0;
    preservingHistoryScroll.current = true;
    await loadMoreMessages();
    requestAnimationFrame(() => {
      if (element) {
        element.scrollTop =
          previousTop + Math.max(0, element.scrollHeight - previousHeight);
      }
      preservingHistoryScroll.current = false;
    });
  }, [loadMoreMessages]);

  return (
    <div className="flex h-full min-h-0 flex-col bg-card">
      <div className="flex h-16 shrink-0 items-center justify-between border-b px-3">
        <div className="flex min-w-0 items-center gap-2">
          <span className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-primary/12 text-primary">
            <BrainCircuit className="size-4" />
          </span>
          <div className="min-w-0">
            <div className="flex items-center gap-1">
              <span className="max-w-[100px] truncate text-sm font-medium sm:max-w-[140px]">
                {currentConversation?.title || "新对话"}
              </span>
              {loadingConversations && (
                <LoaderCircle className="size-3 animate-spin text-muted-foreground" />
              )}
              <span className="flex shrink-0 items-center gap-1 text-[10px] text-positive">
                <span
                  className={cn(
                    "size-1.5 rounded-full",
                    assistantReady ? "bg-emerald-500" : "bg-amber-500",
                  )}
                />
                {statusText}
              </span>
            </div>
            <div className="ml-2 text-[10px] text-muted-foreground">
              实时快照 · 对话自动保存
            </div>
          </div>
        </div>
        <div
          className={cn(
            "flex items-center gap-1",
            reserveCloseSpace && "mr-9",
          )}
        >
          <Button
            variant="outline"
            size="sm"
            className="h-8 gap-1 px-2 text-xs"
            aria-label="新建对话"
            onClick={startNewConversation}
          >
            <Plus className="size-3.5" />
            新建
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

      <div className="flex min-h-0 flex-1">
        <ConversationRail />
        <div ref={messageScroll} className="min-w-0 flex-1 overflow-y-auto p-3">
        {hasMoreMessages && (
          <div className="mb-3 flex justify-center">
            <Button
              variant="ghost"
              size="sm"
              disabled={loadingMore}
              onClick={() => void handleLoadMore()}
              className="h-7 text-[10px]"
            >
              {loadingMore && <LoaderCircle className="size-3 animate-spin" />}
              加载更早消息
            </Button>
          </div>
        )}
        {loadingMessages ? (
          <div className="flex h-full items-center justify-center text-xs text-muted-foreground">
            <LoaderCircle className="mr-2 size-4 animate-spin" />
            加载对话…
          </div>
        ) : (
          <div className="space-y-4">
            {messages.length === 0 && (
              <MessageBubble role="assistant" content={welcomeContent} />
            )}
            {messages.map((message, index) => (
              <MessageBubble
                key={message.id}
                role={message.role}
                content={message.content}
                tool={message.tool}
                streaming={
                  streaming &&
                  index === messages.length - 1 &&
                  message.role === "assistant"
                }
                onRetry={
                  message.role === "assistant" &&
                  (message.status === "failed" || message.status === "stopped") &&
                  message.clientMessageId
                    ? () => void retryMessage(message.clientMessageId)
                    : undefined
                }
              />
            ))}
            <div ref={messageEnd} />
          </div>
        )}
        </div>
      </div>

      <div className="shrink-0 border-t p-3">
        {error && (
          <div className="mb-2 rounded-md border border-destructive/30 bg-destructive/5 px-2.5 py-2 text-[10px] text-destructive">
            {error}
          </div>
        )}
        {!assistantReady && (
          <div className="mb-3 rounded-lg border border-amber-500/30 bg-amber-500/5 p-3 text-xs leading-5">
            <div className="font-medium">
              {availability === "anonymous"
                ? "登录并绑定 AI Key 后可使用投研助手"
                : availability === "invalid"
                  ? "当前 AI Key 不可用"
                  : "请先配置可用的 AI Key"}
            </div>
            <Link
              href="/ai-settings"
              className="mt-2 inline-flex font-medium text-primary hover:underline"
            >
              前往 AI 设置
            </Link>
          </div>
        )}
        {messages.length === 0 && assistantReady && !loadingMessages && (
          <div className="mb-3 space-y-1.5">
            <div className="flex items-center gap-1 text-[10px] text-muted-foreground">
              <Sparkles className="size-3" />
              试试这样问
            </div>
            {suggestedPrompts.map((prompt) => (
              <button
                type="button"
                key={prompt}
                onClick={() => void sendMessage(prompt)}
                disabled={streaming}
                className="flex w-full items-center justify-between rounded-md border bg-background px-2.5 py-2 text-left text-[11px] transition-colors hover:border-primary/35 hover:bg-primary/5 disabled:pointer-events-none disabled:opacity-50"
              >
                {prompt}
                <ChevronRight className="size-3 text-muted-foreground" />
              </button>
            ))}
          </div>
        )}

        <div className="relative rounded-lg border bg-background focus-within:border-primary/45 focus-within:ring-2 focus-within:ring-primary/10">
          <textarea
            value={draft}
            rows={2}
            maxLength={4000}
            disabled={streaming || !assistantReady}
            placeholder="询问资金费、市场机会或风险..."
            onChange={(event) => setDraft(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Enter" && !event.shiftKey) {
                event.preventDefault();
                void sendMessage(draft);
              }
            }}
            className="block w-full resize-none bg-transparent px-3 py-2.5 pr-11 text-xs leading-5 outline-none placeholder:text-muted-foreground disabled:opacity-60"
          />
          <Button
            size="icon-sm"
            className="absolute right-2 bottom-2"
            aria-label={streaming ? "停止生成" : "发送消息"}
            onClick={() =>
              streaming ? stopStreaming() : void sendMessage(draft)
            }
            disabled={!streaming && (!draft.trim() || !assistantReady)}
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

function MessageBubble({
  role,
  content,
  tool,
  streaming,
  onRetry,
}: {
  role: "user" | "assistant";
  content: string;
  tool?: string;
  streaming?: boolean;
  onRetry?: () => void;
}) {
  return (
    <div className={cn("flex gap-2", role === "user" && "justify-end")}>
      {role === "assistant" && (
        <span className="mt-0.5 flex size-6 shrink-0 items-center justify-center rounded-full bg-primary/10 text-primary">
          <Bot className="size-3.5" />
        </span>
      )}
      <div className={cn("max-w-[88%]", role === "user" && "order-first")}>
        {tool && (
          <div className="mb-1.5 inline-flex items-center gap-1 rounded border bg-muted/60 px-2 py-1 text-[9px] text-muted-foreground">
            <Wrench className="size-2.5" />
            {tool === "funding" ? "已读取资金费快照" : tool}
          </div>
        )}
        <div
          className={cn(
            "whitespace-pre-wrap rounded-xl px-3 py-2.5 text-xs leading-5",
            role === "assistant"
              ? "rounded-tl-sm border bg-background"
              : "rounded-tr-sm bg-primary text-primary-foreground",
          )}
        >
          {content}
          {streaming && (
            <span className="ml-0.5 inline-block h-3 w-0.5 animate-pulse bg-primary align-middle" />
          )}
        </div>
        {onRetry && (
          <button
            type="button"
            onClick={onRetry}
            className="mt-1 inline-flex items-center gap-1 text-[10px] text-muted-foreground hover:text-foreground"
          >
            <RotateCcw className="size-2.5" />
            重试
          </button>
        )}
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
        <aside className="fixed top-16 right-0 bottom-0 z-30 hidden w-[350px] border-l shadow-[-10px_0_30px_-24px_rgba(0,0,0,0.5)] assistant-dock:block">
          <AssistantBody
            showCollapse
            onCollapse={() => onDesktopOpenChange(false)}
          />
        </aside>
      ) : (
        <aside className="fixed top-16 right-0 bottom-0 z-30 hidden w-14 border-l bg-card assistant-dock:flex assistant-dock:flex-col assistant-dock:items-center">
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
        className="fixed z-40 gap-2 rounded-full shadow-lg assistant-dock:hidden right-[max(1rem,env(safe-area-inset-right))] bottom-[max(1rem,env(safe-area-inset-bottom))]"
        onClick={() => setMobileOpen(true)}
      >
        <MessageCircle className="size-4" />
        AI 投研助手
      </Button>

      <Sheet open={mobileOpen} onOpenChange={setMobileOpen}>
        <SheetContent
          className="w-full gap-0 p-0 sm:max-w-[420px] assistant-dock:hidden"
          overlayClassName="supports-backdrop-filter:backdrop-blur-none"
        >
          <SheetHeader className="sr-only">
            <SheetTitle>AI 投研助手</SheetTitle>
            <SheetDescription>基于实时资金费快照的流式量化投研助手</SheetDescription>
          </SheetHeader>
          <AssistantBody reserveCloseSpace />
        </SheetContent>
      </Sheet>
    </>
  );
}
