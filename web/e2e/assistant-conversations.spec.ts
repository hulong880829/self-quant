import { expect, type Page, test } from "@playwright/test";

interface StoredMessage {
  id: string;
  conversationId: string;
  turnId: string;
  clientMessageId: string;
  role: "user" | "assistant";
  status: string;
  content: string;
  tool: string;
  inputTokens: number;
  outputTokens: number;
  createdAt: string;
  updatedAt: string;
}

interface StoredConversation {
  id: string;
  title: string;
  lastMessagePreview: string;
  messageCount: number;
  modelAlias: string;
  lastMessageAt: string;
  expiresAt: string;
  createdAt: string;
  updatedAt: string;
}

async function mockAssistantAPI(page: Page) {
  const now = new Date().toISOString();
  const conversations: StoredConversation[] = [];
  const messages = new Map<string, StoredMessage[]>();

  await page.route("**/api/v1/auth/session", (route) =>
    route.fulfill({
      json: { authenticated: true, username: "e2e-user", permission: "user" },
    }),
  );
  await page.route("**/api/v1/ai/credentials/openrouter", (route) =>
    route.fulfill({
      json: {
        data: {
          provider: "openrouter",
          apiKeyMasked: "sk-o****test",
          status: "valid",
          lastError: "",
          lastTestedAt: now,
        },
      },
    }),
  );
  await page.route("**/api/v1/ai/conversations", async (route) => {
    if (route.request().method() === "POST") {
      const id = `conversation-${conversations.length + 1}`;
      const conversation = {
        id,
        title: "",
        lastMessagePreview: "",
        messageCount: 0,
        modelAlias: "free-general",
        lastMessageAt: now,
        expiresAt: now,
        createdAt: now,
        updatedAt: now,
      };
      conversations.unshift(conversation);
      messages.set(id, []);
      await route.fulfill({ status: 201, json: { data: conversation } });
      return;
    }
    await route.fulfill({ json: { data: conversations, nextCursor: "" } });
  });
  await page.route("**/api/v1/ai/conversations/*", async (route) => {
    const id = route.request().url().split("/conversations/")[1].split("/")[0];
    if (route.request().url().includes("/messages")) {
      await route.fulfill({
        json: { data: [...(messages.get(id) ?? [])].reverse(), nextCursor: "" },
      });
      return;
    }
    if (route.request().method() === "DELETE") {
      const index = conversations.findIndex((item) => item.id === id);
      if (index >= 0) conversations.splice(index, 1);
      messages.delete(id);
      await route.fulfill({ status: 204 });
      return;
    }
    await route.continue();
  });
  await page.route("**/api/v1/ai/chat", async (route) => {
    const body = route.request().postDataJSON() as {
      conversationId: string;
      prompt: string;
      clientMessageId: string;
    };
    const turnId = `turn-${Date.now()}`;
    const userId = `user-${Date.now()}`;
    const assistantId = `assistant-${Date.now()}`;
    const conversation = conversations.find(
      (item) => item.id === body.conversationId,
    );
    if (conversation) {
      conversation.title ||= body.prompt.slice(0, 32);
      conversation.lastMessagePreview = body.prompt;
      conversation.messageCount += 2;
    }
    const stored = messages.get(body.conversationId) ?? [];
    stored.push(
      {
        id: userId,
        conversationId: body.conversationId,
        turnId,
        clientMessageId: body.clientMessageId,
        role: "user",
        status: "completed",
        content: body.prompt,
        tool: "",
        inputTokens: 0,
        outputTokens: 0,
        createdAt: now,
        updatedAt: now,
      },
      {
        id: assistantId,
        conversationId: body.conversationId,
        turnId,
        clientMessageId: "",
        role: "assistant",
        status: "completed",
        content: "持久化回答",
        tool: "funding",
        inputTokens: 10,
        outputTokens: 4,
        createdAt: now,
        updatedAt: now,
      },
    );
    messages.set(body.conversationId, stored);
    const frame = (event: string, data: object) =>
      `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`;
    await route.fulfill({
      contentType: "text/event-stream",
      body:
        frame("turn", {
          conversationId: body.conversationId,
          userMessageId: userId,
          assistantMessageId: assistantId,
          title: conversation?.title,
          status: "pending",
        }) +
        frame("tool", { tool: "funding" }) +
        frame("delta", { delta: "持久化回答" }) +
        frame("done", {
          conversationId: body.conversationId,
          userMessageId: userId,
          assistantMessageId: assistantId,
          title: conversation?.title,
          status: "completed",
        }),
    });
  });
  await page.route("**/api/v1/funding-rates*", (route) =>
    route.fulfill({ json: { data: [], updatedAt: now } }),
  );
}

test("desktop assistant creates and restores a persisted conversation", async ({
  page,
}) => {
  await mockAssistantAPI(page);
  await page.setViewportSize({ width: 1366, height: 768 });
  await page.goto("/funding");
  const prompt = page.getByPlaceholder("询问资金费、市场机会或风险...");
  await expect(prompt).toBeVisible();
  await prompt.fill("分析 BTC");
  await page.getByRole("button", { name: "发送消息" }).click();
  await expect(page.getByText("持久化回答")).toBeVisible();
  await expect(page.getByLabel("切换到会话 1：分析 BTC")).toBeVisible();
  await page.reload();
  await expect(page.getByText("分析 BTC")).toBeVisible();
  await expect(page.getByText("持久化回答")).toBeVisible();
});

test("mobile assistant uses the same persisted state", async ({ page }) => {
  await mockAssistantAPI(page);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/funding");
  await page.getByRole("button", { name: "AI 投研助手" }).click();
  await expect(
    page.getByPlaceholder("询问资金费、市场机会或风险..."),
  ).toBeVisible();
  const overlayStyle = await page
    .locator('[data-slot="sheet-overlay"]')
    .evaluate((element) => {
      const style = window.getComputedStyle(element);
      return {
        backdropFilter: style.backdropFilter,
        pointerEvents: style.pointerEvents,
      };
    });
  expect(overlayStyle.backdropFilter).toBe("none");
  expect(overlayStyle.pointerEvents).not.toBe("none");
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflow).toBe(0);
});
