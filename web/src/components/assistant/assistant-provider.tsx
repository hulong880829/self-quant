"use client";

import * as React from "react";

import { useAuth } from "@/components/auth/auth-provider";
import {
  AIAPIError,
  createAIConversation,
  deleteAIConversation,
  fetchAIConversationMessages,
  fetchAIConversations,
  renameAIConversation,
  streamAIChat,
  type AIConversation,
  type AIConversationMessage,
  type AIStreamEvent,
} from "@/lib/api/ai";
import { createIdempotencyKey } from "@/lib/idempotency-key";

const conversationStoragePrefix = "self-quant:ai-conversation:";
const defaultModelAlias = "free-general";
const maxConversationsPerUser = 5;

export interface AssistantMessage extends AIConversationMessage {
  optimistic?: boolean;
}

interface AssistantContextValue {
  conversations: AIConversation[];
  currentConversationId: string | null;
  currentConversation: AIConversation | null;
  messages: AssistantMessage[];
  draft: string;
  setDraft: (draft: string) => void;
  loadingConversations: boolean;
  loadingMessages: boolean;
  loadingMore: boolean;
  hasMoreMessages: boolean;
  streaming: boolean;
  error: string;
  startNewConversation: () => void;
  selectConversation: (id: string) => Promise<void>;
  renameConversation: (id: string, title: string) => Promise<void>;
  removeConversation: (id: string) => Promise<void>;
  loadMoreMessages: () => Promise<void>;
  sendMessage: (prompt: string) => Promise<void>;
  retryMessage: (clientMessageId: string) => Promise<void>;
  stopStreaming: () => void;
}

const AssistantContext = React.createContext<AssistantContextValue | null>(null);

function storageKey(username: string): string {
  return `${conversationStoragePrefix}${username}`;
}

export function clearAssistantConversationSession(username: string): void {
  sessionStorage.removeItem(storageKey(username));
}

function optimisticMessage(
  id: string,
  conversationId: string,
  role: "user" | "assistant",
  content: string,
  clientMessageId: string,
): AssistantMessage {
  const now = new Date().toISOString();
  return {
    id,
    conversationId,
    turnId: "",
    clientMessageId,
    role,
    status: "streaming",
    content,
    tool: "",
    inputTokens: 0,
    outputTokens: 0,
    createdAt: now,
    updatedAt: now,
    optimistic: true,
  };
}

function normalizeConversationMessages(
  messages: AIConversationMessage[],
): AssistantMessage[] {
  const clientIdsByTurn = new Map(
    messages
      .filter((message) => message.role === "user" && message.clientMessageId)
      .map((message) => [message.turnId, message.clientMessageId]),
  );
  return messages.map((message) => ({
    ...message,
    clientMessageId:
      message.clientMessageId || clientIdsByTurn.get(message.turnId) || "",
  }));
}

export function AssistantProvider({ children }: { children: React.ReactNode }) {
  const { status: authStatus, user } = useAuth();
  const username = user?.username ?? null;
  const [conversations, setConversations] = React.useState<AIConversation[]>([]);
  const [currentConversationId, setCurrentConversationId] = React.useState<
    string | null
  >(null);
  const [messages, setMessages] = React.useState<AssistantMessage[]>([]);
  const [messagesCursor, setMessagesCursor] = React.useState("");
  const [draft, setDraft] = React.useState("");
  const [loadingConversations, setLoadingConversations] = React.useState(
    authStatus === "authenticated" && Boolean(username),
  );
  const [loadingMessages, setLoadingMessages] = React.useState(false);
  const [loadingMore, setLoadingMore] = React.useState(false);
  const [streaming, setStreaming] = React.useState(false);
  const [error, setError] = React.useState("");
  const streamAbort = React.useRef<AbortController | null>(null);
  const loadSequence = React.useRef(0);

  const stopStreaming = React.useCallback(() => {
    streamAbort.current?.abort();
    streamAbort.current = null;
    setMessages((items) =>
      items.map((message) =>
        message.role === "assistant" && message.status === "streaming"
          ? { ...message, status: "stopped" }
          : message,
      ),
    );
    setStreaming(false);
  }, []);

  const loadMessages = React.useCallback(async (conversationId: string) => {
    const sequence = ++loadSequence.current;
    setLoadingMessages(true);
    setMessages([]);
    setMessagesCursor("");
    setError("");
    try {
      const page = await fetchAIConversationMessages(conversationId, 30);
      if (loadSequence.current !== sequence) return;
      setMessages(normalizeConversationMessages([...page.data].reverse()));
      setMessagesCursor(page.nextCursor || "");
    } catch (reason) {
      if (loadSequence.current !== sequence) return;
      if (reason instanceof AIAPIError && reason.status === 404) {
        setConversations((items) =>
          items.filter((item) => item.id !== conversationId),
        );
        setCurrentConversationId(null);
        setMessages([]);
        setMessagesCursor("");
        setError("该对话已过期或被删除，请开始新对话");
      } else {
        setError(reason instanceof Error ? reason.message : "无法读取对话消息");
      }
    } finally {
      if (loadSequence.current === sequence) setLoadingMessages(false);
    }
  }, []);

  React.useEffect(() => {
    if (authStatus !== "authenticated" || !username) {
      return;
    }

    let cancelled = false;
    void fetchAIConversations(20)
      .then((page) => {
        if (cancelled) return;
        setConversations(page.data);
        const stored = sessionStorage.getItem(storageKey(username));
        const selected =
          (stored && page.data.some((item) => item.id === stored)
            ? stored
            : page.data[0]?.id) ?? null;
        setCurrentConversationId(selected);
        if (selected) void loadMessages(selected);
      })
      .catch((reason) => {
        if (!cancelled) {
          setError(
            reason instanceof Error ? reason.message : "无法读取对话列表",
          );
        }
      })
      .finally(() => {
        if (!cancelled) setLoadingConversations(false);
      });
    return () => {
      cancelled = true;
    };
  }, [authStatus, username, loadMessages]);

  React.useEffect(() => {
    if (!username) return;
    if (currentConversationId) {
      sessionStorage.setItem(storageKey(username), currentConversationId);
    } else {
      sessionStorage.removeItem(storageKey(username));
    }
  }, [currentConversationId, username]);

  React.useEffect(() => stopStreaming, [stopStreaming]);
  const startNewConversation = React.useCallback(() => {
    if (conversations.length >= maxConversationsPerUser) {
      setError("最多保留 5 个会话，请先删除一个");
      return;
    }
    stopStreaming();
    loadSequence.current += 1;
    setCurrentConversationId(null);
    setMessages([]);
    setMessagesCursor("");
    setDraft("");
    setError("");
  }, [conversations.length, stopStreaming]);

  const selectConversation = React.useCallback(
    async (id: string) => {
      if (id === currentConversationId) return;
      stopStreaming();
      setCurrentConversationId(id);
      await loadMessages(id);
    },
    [currentConversationId, loadMessages, stopStreaming],
  );

  const recoverMissingConversation = React.useCallback(
    (id: string) => {
      setConversations((items) => items.filter((item) => item.id !== id));
      setCurrentConversationId(null);
      setMessages([]);
      setMessagesCursor("");
      if (username) sessionStorage.removeItem(storageKey(username));
      setError("该对话已过期或被删除，请开始新对话");
    },
    [username],
  );

  const renameConversation = React.useCallback(
    async (id: string, title: string) => {
      const trimmed = title.trim();
      if (!trimmed) return;
      try {
        const updated = await renameAIConversation(id, trimmed);
        setConversations((items) =>
          items.map((item) => (item.id === id ? updated : item)),
        );
      } catch (reason) {
        if (reason instanceof AIAPIError && reason.status === 404) {
          recoverMissingConversation(id);
          return;
        }
        setError(reason instanceof Error ? reason.message : "重命名对话失败");
      }
    },
    [recoverMissingConversation],
  );

  const removeConversation = React.useCallback(
    async (id: string) => {
      if (id === currentConversationId) stopStreaming();
      try {
        await deleteAIConversation(id);
        const remaining = conversations.filter((item) => item.id !== id);
        setConversations(remaining);
        if (id === currentConversationId) {
          const next = remaining[0]?.id ?? null;
          setCurrentConversationId(next);
          if (next) await loadMessages(next);
          else {
            setMessages([]);
            setMessagesCursor("");
          }
        }
      } catch (reason) {
        if (reason instanceof AIAPIError && reason.status === 404) {
          recoverMissingConversation(id);
          return;
        }
        setError(reason instanceof Error ? reason.message : "删除对话失败");
      }
    },
    [
      conversations,
      currentConversationId,
      loadMessages,
      recoverMissingConversation,
      stopStreaming,
    ],
  );

  const loadMoreMessages = React.useCallback(async () => {
    if (!currentConversationId || !messagesCursor || loadingMore) return;
    setLoadingMore(true);
    setError("");
    const sequence = loadSequence.current;
    const conversationId = currentConversationId;
    try {
      const page = await fetchAIConversationMessages(
        conversationId,
        30,
        messagesCursor,
      );
      if (
        loadSequence.current !== sequence ||
        currentConversationId !== conversationId
      ) {
        return;
      }
      setMessages((items) =>
        normalizeConversationMessages([...[...page.data].reverse(), ...items]),
      );
      setMessagesCursor(page.nextCursor || "");
    } catch (reason) {
      if (reason instanceof AIAPIError && reason.status === 404) {
        recoverMissingConversation(currentConversationId);
      } else {
        setError(reason instanceof Error ? reason.message : "无法加载更早消息");
      }
    } finally {
      setLoadingMore(false);
    }
  }, [
    currentConversationId,
    loadingMore,
    messagesCursor,
    recoverMissingConversation,
  ]);

  const applyStreamEvent = React.useCallback(
    (
      event: AIStreamEvent,
      conversationId: string,
      clientMessageId: string,
      optimisticUserId: string,
      optimisticAssistantId: string,
      incrementMessageCount: boolean,
    ) => {
      if (event.conversationId && event.conversationId !== conversationId) return;
      setMessages((items) =>
        items.map((message) => {
          if (
            (message.id === optimisticUserId ||
              (message.clientMessageId === clientMessageId &&
                message.role === "user")) &&
            event.userMessageId
          ) {
            return {
              ...message,
              id: event.userMessageId,
              status: "completed",
              optimistic: false,
            };
          }
          if (
            message.id === optimisticAssistantId ||
            (message.clientMessageId === clientMessageId &&
              message.role === "assistant") ||
            (event.assistantMessageId &&
              message.id === event.assistantMessageId)
          ) {
            return {
              ...message,
              id: event.assistantMessageId || message.id,
              content:
                event.type === "delta" && event.delta
                  ? message.content + event.delta
                  : message.content,
              tool: event.type === "tool" ? event.tool || message.tool : message.tool,
              inputTokens: event.inputTokens ?? message.inputTokens,
              outputTokens: event.outputTokens ?? message.outputTokens,
              status: event.status || message.status,
              optimistic: event.assistantMessageId ? false : message.optimistic,
            };
          }
          return message;
        }),
      );
      if (event.title) {
        setConversations((items) =>
          items.map((item) =>
            item.id === conversationId ? { ...item, title: event.title! } : item,
          ),
        );
      }
      if (event.type === "done") {
        const now = new Date().toISOString();
        setMessages((items) =>
          items.map((message) =>
            message.clientMessageId === clientMessageId
              ? {
                  ...message,
                  status: message.role === "assistant" ? "completed" : message.status,
                  optimistic: false,
                }
              : message,
          ),
        );
        setConversations((items) => {
          const current = items.find((item) => item.id === conversationId);
          if (!current) return items;
          const updated = {
            ...current,
            title: event.title || current.title,
            messageCount:
              current.messageCount + (incrementMessageCount ? 2 : 0),
            lastMessageAt: now,
            updatedAt: now,
          };
          return [updated, ...items.filter((item) => item.id !== conversationId)];
        });
      }
    },
    [],
  );

  const sendWithClientId = React.useCallback(
    async (rawPrompt: string, retryClientMessageId?: string) => {
      const prompt = rawPrompt.trim();
      if (!prompt || streaming || authStatus !== "authenticated") return;
      setError("");

      let conversationId = currentConversationId;
      let conversation: AIConversation | undefined;
      let controller: AbortController | null = null;
      let clientMessageId = retryClientMessageId || "";
      try {
        if (!clientMessageId) {
          clientMessageId = createIdempotencyKey();
        }
        setDraft("");
        if (!conversationId) {
          conversation = await createAIConversation(defaultModelAlias);
          conversationId = conversation.id;
          setConversations((items) => [
            conversation!,
            ...items.filter((item) => item.id !== conversation!.id),
          ]);
          setCurrentConversationId(conversationId);
        }

        const optimisticUserId = `pending-user-${clientMessageId}`;
        const optimisticAssistantId = `pending-assistant-${clientMessageId}`;
        setMessages((items) => {
          if (retryClientMessageId) {
            return items.map((message) =>
              message.clientMessageId === clientMessageId &&
              message.role === "assistant"
                ? {
                    ...message,
                    content: "",
                    tool: "",
                    status: "streaming",
                    optimistic: true,
                  }
                : message,
            );
          }
          return [
            ...items,
            optimisticMessage(
              optimisticUserId,
              conversationId!,
              "user",
              prompt,
              clientMessageId,
            ),
            optimisticMessage(
              optimisticAssistantId,
              conversationId!,
              "assistant",
              "",
              clientMessageId,
            ),
          ];
        });
        setConversations((items) => {
          const current = items.find((item) => item.id === conversationId);
          if (!current) return items;
          const updated = {
            ...current,
            lastMessagePreview: prompt.slice(0, 80),
          };
          return [updated, ...items.filter((item) => item.id !== conversationId)];
        });
        setStreaming(true);
        controller = new AbortController();
        streamAbort.current = controller;
        await streamAIChat(
          {
            conversationId,
            prompt,
            clientMessageId,
            modelAlias: conversation?.modelAlias || defaultModelAlias,
          },
          (event) =>
            applyStreamEvent(
              event,
              conversationId!,
              clientMessageId,
              optimisticUserId,
              optimisticAssistantId,
              !retryClientMessageId,
            ),
          controller.signal,
        );
      } catch (reason) {
        if (reason instanceof DOMException && reason.name === "AbortError") return;
        if (
          conversationId &&
          ((reason instanceof AIAPIError && reason.status === 404) ||
            (reason instanceof Error &&
              /not found|expired/i.test(reason.message)))
        ) {
          recoverMissingConversation(conversationId);
          return;
        }
        const detail =
          reason instanceof Error ? reason.message : "AI 对话暂时不可用";
        setDraft((current) => (current.trim() ? current : prompt));
        setMessages((items) =>
          items.map((message) =>
            message.clientMessageId === clientMessageId &&
            message.role === "assistant"
              ? {
                  ...message,
                  content: message.content || `生成失败：${detail}`,
                  status: "failed",
                }
              : message,
          ),
        );
        setError(detail);
      } finally {
        if (streamAbort.current === controller) {
          streamAbort.current = null;
          setStreaming(false);
        }
      }
    },
    [
      applyStreamEvent,
      authStatus,
      currentConversationId,
      recoverMissingConversation,
      streaming,
    ],
  );

  const sendMessage = React.useCallback(
    (prompt: string) => sendWithClientId(prompt),
    [sendWithClientId],
  );

  const retryMessage = React.useCallback(
    async (clientMessageId: string) => {
      const userMessage = messages.find(
        (message) =>
          message.clientMessageId === clientMessageId &&
          message.role === "user",
      );
      if (userMessage) {
        await sendWithClientId(userMessage.content, clientMessageId);
      }
    },
    [messages, sendWithClientId],
  );

  const currentConversation =
    conversations.find((item) => item.id === currentConversationId) ?? null;
  const value = React.useMemo<AssistantContextValue>(
    () => ({
      conversations,
      currentConversationId,
      currentConversation,
      messages,
      draft,
      setDraft,
      loadingConversations,
      loadingMessages,
      loadingMore,
      hasMoreMessages: Boolean(messagesCursor),
      streaming,
      error,
      startNewConversation,
      selectConversation,
      renameConversation,
      removeConversation,
      loadMoreMessages,
      sendMessage,
      retryMessage,
      stopStreaming,
    }),
    [
      conversations,
      currentConversationId,
      currentConversation,
      messages,
      draft,
      loadingConversations,
      loadingMessages,
      loadingMore,
      messagesCursor,
      streaming,
      error,
      startNewConversation,
      selectConversation,
      renameConversation,
      removeConversation,
      loadMoreMessages,
      sendMessage,
      retryMessage,
      stopStreaming,
    ],
  );

  return (
    <AssistantContext.Provider value={value}>
      {children}
    </AssistantContext.Provider>
  );
}

export function useAssistant(): AssistantContextValue {
  const value = React.useContext(AssistantContext);
  if (!value) {
    throw new Error("useAssistant must be used within AssistantProvider");
  }
  return value;
}
