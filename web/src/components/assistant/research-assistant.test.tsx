// @vitest-environment jsdom

import * as React from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const assistant = vi.hoisted(() => ({
  selectConversation: vi.fn(async () => undefined),
  startNewConversation: vi.fn(),
  renameConversation: vi.fn(async () => undefined),
  removeConversation: vi.fn(async () => undefined),
  setDraft: vi.fn(),
  loadMoreMessages: vi.fn(async () => undefined),
  sendMessage: vi.fn(async () => undefined),
  retryMessage: vi.fn(async () => undefined),
  stopStreaming: vi.fn(),
}));

const conversations = Array.from({ length: 5 }, (_, index) => ({
  id: `conversation-${index + 1}`,
  title: `会话 ${index + 1}`,
  lastMessagePreview: "",
  messageCount: 2,
  modelAlias: "free-general",
  lastMessageAt: "",
  expiresAt: "",
  createdAt: "",
  updatedAt: "",
}));

vi.mock("@/components/assistant/assistant-provider", () => ({
  useAssistant: () => ({
    ...assistant,
    conversations,
    currentConversationId: conversations[0].id,
    currentConversation: conversations[0],
    messages: [],
    draft: "",
    loadingConversations: false,
    loadingMessages: false,
    loadingMore: false,
    hasMoreMessages: false,
    streaming: false,
    error: "",
  }),
}));

vi.mock("@/components/assistant/ai-status-provider", () => ({
  useAIStatus: () => ({ availability: "valid" }),
}));

import { ResearchAssistant } from "./research-assistant";

describe("ResearchAssistant conversations", () => {
  beforeEach(() => {
    HTMLElement.prototype.scrollIntoView = vi.fn();
  });

  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
  });

  it("shows five direct conversation switches and a separate new button", () => {
    render(
      <ResearchAssistant
        desktopOpen
        onDesktopOpenChange={() => undefined}
      />,
    );

    expect(screen.getAllByLabelText(/^切换到会话/)).toHaveLength(5);
    fireEvent.click(screen.getByLabelText("切换到会话 2：会话 2"));
    expect(assistant.selectConversation).toHaveBeenCalledWith("conversation-2");

    fireEvent.click(screen.getByRole("button", { name: "新建对话" }));
    expect(assistant.startNewConversation).toHaveBeenCalledOnce();
    expect(
      document.querySelector('[data-slot="dropdown-menu-trigger"]'),
    ).toBeNull();
  });

  it("keeps the mobile modal backdrop but disables blur for the assistant", () => {
    render(
      <ResearchAssistant
        desktopOpen={false}
        onDesktopOpenChange={() => undefined}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "AI 投研助手" }));
    const overlay = document.querySelector('[data-slot="sheet-overlay"]');
    expect(overlay).not.toBeNull();
    expect(overlay?.className).toContain(
      "supports-backdrop-filter:backdrop-blur-none",
    );
  });
});
