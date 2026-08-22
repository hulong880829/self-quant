// @vitest-environment jsdom

import * as React from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const apiMocks = vi.hoisted(() => ({
  fetchAIConversations: vi.fn(),
  fetchAIConversationMessages: vi.fn(),
  createAIConversation: vi.fn(),
  renameAIConversation: vi.fn(),
  deleteAIConversation: vi.fn(),
  streamAIChat: vi.fn(),
}));

vi.mock("@/components/auth/auth-provider", () => ({
  useAuth: () => ({
    status: "authenticated",
    user: { username: "alice", permission: "user" },
  }),
}));

vi.mock("@/lib/api/ai", async (importOriginal) => {
  const original = await importOriginal<typeof import("@/lib/api/ai")>();
  return { ...original, ...apiMocks };
});

import {
  AssistantProvider,
  useAssistant,
} from "./assistant-provider";

const conversation = {
  id: "conversation-1",
  title: "BTC",
  lastMessagePreview: "",
  messageCount: 0,
  modelAlias: "free-general",
  lastMessageAt: "",
  expiresAt: "",
  createdAt: "",
  updatedAt: "",
};

function Consumer() {
  const assistant = useAssistant();
  return (
    <>
      <div data-testid="conversation">{assistant.currentConversationId}</div>
      <div data-testid="messages">
        {assistant.messages.map((message) => `${message.id}:${message.content}`).join("|")}
      </div>
      <div data-testid="draft">{assistant.draft}</div>
      <div data-testid="error">{assistant.error}</div>
      <button onClick={assistant.startNewConversation}>new</button>
      <button onClick={() => void assistant.sendMessage("hello")}>send</button>
    </>
  );
}

describe("AssistantProvider", () => {
  beforeEach(() => {
    sessionStorage.clear();
    vi.clearAllMocks();
    vi.stubGlobal("crypto", { randomUUID: () => "client-1" });
    apiMocks.fetchAIConversations.mockResolvedValue({
      data: [conversation],
      nextCursor: "",
    });
    apiMocks.fetchAIConversationMessages.mockResolvedValue({
      data: [
        {
          id: "message-2",
          conversationId: conversation.id,
          turnId: "turn-1",
          clientMessageId: "client-0",
          role: "assistant",
          status: "completed",
          content: "answer",
          tool: "",
          inputTokens: 0,
          outputTokens: 1,
          createdAt: "",
          updatedAt: "",
        },
        {
          id: "message-1",
          conversationId: conversation.id,
          turnId: "turn-1",
          clientMessageId: "client-0",
          role: "user",
          status: "completed",
          content: "question",
          tool: "",
          inputTokens: 0,
          outputTokens: 0,
          createdAt: "",
          updatedAt: "",
        },
      ],
      nextCursor: "",
    });
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("restores a conversation and reverses newest-first messages", async () => {
    sessionStorage.setItem(
      "self-quant:ai-conversation:alice",
      conversation.id,
    );
    render(
      <AssistantProvider>
        <Consumer />
      </AssistantProvider>,
    );

    await waitFor(() =>
      expect(screen.getByTestId("messages").textContent).toBe(
        "message-1:question|message-2:answer",
      ),
    );
    expect(screen.getByTestId("conversation").textContent).toBe(conversation.id);
  });

  it("creates on first send and reconciles optimistic server IDs", async () => {
    apiMocks.fetchAIConversations.mockResolvedValue({ data: [], nextCursor: "" });
    apiMocks.createAIConversation.mockResolvedValue(conversation);
    apiMocks.streamAIChat.mockImplementation(
      async (
        _request: unknown,
        onEvent: (event: Record<string, unknown>) => void,
      ) => {
        onEvent({
          type: "turn",
          conversationId: conversation.id,
          userMessageId: "user-server",
          assistantMessageId: "assistant-server",
          title: "Hello",
        });
        onEvent({ type: "delta", delta: "world" });
        onEvent({ type: "done", status: "completed" });
      },
    );
    render(
      <AssistantProvider>
        <Consumer />
      </AssistantProvider>,
    );
    await waitFor(() =>
      expect(apiMocks.fetchAIConversations).toHaveBeenCalled(),
    );

    fireEvent.click(screen.getByRole("button", { name: "new" }));
    fireEvent.click(screen.getByRole("button", { name: "send" }));

    await waitFor(() =>
      expect(screen.getByTestId("messages").textContent).toBe(
        "user-server:hello|assistant-server:world",
      ),
    );
    expect(apiMocks.streamAIChat).toHaveBeenCalledWith(
      expect.objectContaining({
        conversationId: conversation.id,
        prompt: "hello",
        clientMessageId: "client-1",
      }),
      expect.any(Function),
      expect.any(AbortSignal),
    );
  });

  it("sends when crypto.randomUUID is unavailable", async () => {
    vi.stubGlobal("crypto", {
      getRandomValues(bytes: Uint8Array) {
        bytes.fill(0xab);
        return bytes;
      },
    });
    apiMocks.fetchAIConversations.mockResolvedValue({ data: [], nextCursor: "" });
    apiMocks.createAIConversation.mockResolvedValue(conversation);
    apiMocks.streamAIChat.mockImplementation(
      async (
        _request: unknown,
        onEvent: (event: Record<string, unknown>) => void,
      ) => {
        onEvent({
          type: "turn",
          conversationId: conversation.id,
          userMessageId: "user-server",
          assistantMessageId: "assistant-server",
        });
        onEvent({ type: "delta", delta: "world" });
        onEvent({ type: "done", status: "completed" });
      },
    );
    render(
      <AssistantProvider>
        <Consumer />
      </AssistantProvider>,
    );
    await waitFor(() =>
      expect(apiMocks.fetchAIConversations).toHaveBeenCalled(),
    );

    fireEvent.click(screen.getByRole("button", { name: "send" }));

    await waitFor(() => expect(apiMocks.streamAIChat).toHaveBeenCalled());
    expect(apiMocks.streamAIChat).toHaveBeenCalledWith(
      expect.objectContaining({
        clientMessageId: "abababab-abab-4bab-abab-abababababab",
      }),
      expect.any(Function),
      expect.any(AbortSignal),
    );
  });

  it("restores the prompt and reports a conversation creation failure", async () => {
    apiMocks.fetchAIConversations.mockResolvedValue({ data: [], nextCursor: "" });
    apiMocks.createAIConversation.mockRejectedValue(new Error("创建会话失败"));
    render(
      <AssistantProvider>
        <Consumer />
      </AssistantProvider>,
    );
    await waitFor(() =>
      expect(apiMocks.fetchAIConversations).toHaveBeenCalled(),
    );

    fireEvent.click(screen.getByRole("button", { name: "send" }));

    await waitFor(() =>
      expect(screen.getByTestId("error").textContent).toBe("创建会话失败"),
    );
    expect(screen.getByTestId("draft").textContent).toBe("hello");
    expect(apiMocks.streamAIChat).not.toHaveBeenCalled();
  });

  it("keeps the current conversation when five conversations already exist", async () => {
    apiMocks.fetchAIConversations.mockResolvedValue({
      data: Array.from({ length: 5 }, (_, index) => ({
        ...conversation,
        id: `conversation-${index + 1}`,
        title: `会话 ${index + 1}`,
      })),
      nextCursor: "",
    });
    apiMocks.fetchAIConversationMessages.mockResolvedValue({
      data: [],
      nextCursor: "",
    });
    render(
      <AssistantProvider>
        <Consumer />
      </AssistantProvider>,
    );
    await waitFor(() =>
      expect(screen.getByTestId("conversation").textContent).toBe(
        "conversation-1",
      ),
    );

    fireEvent.click(screen.getByRole("button", { name: "new" }));

    expect(screen.getByTestId("conversation").textContent).toBe(
      "conversation-1",
    );
    expect(screen.getByTestId("error").textContent).toBe(
      "最多保留 5 个会话，请先删除一个",
    );
  });
});
