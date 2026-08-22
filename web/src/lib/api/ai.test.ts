import { afterEach, describe, expect, it, vi } from "vitest";

import {
  createAIConversation,
  fetchAIConversationMessages,
  fetchAIConversations,
  fetchAICredential,
  parseSSEFrame,
  saveAICredential,
  streamAIChat,
} from "./ai";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("AI credential API", () => {
  it("maps configured credentials and treats 404 as unconfigured", async () => {
    const fetchMock = vi
      .fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            data: {
              provider: "openrouter",
              apiKeyMasked: "sk-o****test",
              status: "valid",
              lastTestedAt: "2026-08-18T00:00:00Z",
            },
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(new Response(null, { status: 404 }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(fetchAICredential()).resolves.toMatchObject({
      provider: "openrouter",
      status: "valid",
      apiKeyMasked: "sk-o****test",
    });
    await expect(fetchAICredential()).resolves.toBeNull();
  });

  it("sends API keys only in the bind request body", async () => {
    const fetchMock = vi
      .fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>()
      .mockResolvedValue(
        new Response(
          JSON.stringify({
            data: { provider: "openrouter", status: "unknown" },
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);
    await saveAICredential("secret-key");
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/ai/credentials/openrouter",
      expect.objectContaining({
        method: "POST",
        credentials: "include",
        body: JSON.stringify({ apiKey: "secret-key" }),
      }),
    );
  });
});

describe("AI SSE client", () => {
  it("parses named events and streams deltas", async () => {
    expect(
      parseSSEFrame(
        'event: turn\ndata: {"conversationId":"conversation-1","userMessageId":"user-1","assistantMessageId":"assistant-1","title":"BTC"}',
      ),
    ).toMatchObject({
      type: "turn",
      conversationId: "conversation-1",
      userMessageId: "user-1",
      assistantMessageId: "assistant-1",
      title: "BTC",
    });
    expect(
      parseSSEFrame('event: delta\ndata: {"delta":"你\\n好"}'),
    ).toMatchObject({ type: "delta", delta: "你\n好" });
    expect(parseSSEFrame(": keepalive")).toBeNull();

    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        const encoder = new TextEncoder();
        controller.enqueue(
          encoder.encode(
            'event: tool\ndata: {"tool":"funding"}\n\n' +
              'event: delta\ndata: {"delta":"hello"}\n\n' +
              "event: done\ndata: {}\n\n",
          ),
        );
        controller.close();
      },
    });
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(stream, {
          status: 200,
          headers: { "Content-Type": "text/event-stream" },
        }),
      ),
    );
    const events: string[] = [];
    await streamAIChat(
      {
        conversationId: "conversation-1",
        prompt: "question",
        clientMessageId: "client-1",
        modelAlias: "free-general",
      },
      (event) => events.push(event.type),
    );
    expect(events).toEqual(["tool", "delta", "done"]);
  });
});

describe("AI conversation API", () => {
  it("uses cursor pagination and the stored-chat request shapes", async () => {
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
    const fetchMock = vi
      .fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>()
      .mockResolvedValueOnce(
        Response.json({ data: [conversation], nextCursor: "next" }),
      )
      .mockResolvedValueOnce(Response.json({ data: conversation }, { status: 201 }))
      .mockResolvedValueOnce(
        Response.json({ data: [], nextCursor: "older" }),
      );
    vi.stubGlobal("fetch", fetchMock);

    await fetchAIConversations(30, "cursor value");
    await createAIConversation("free-general");
    await fetchAIConversationMessages("conversation/1", 30);

    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/api/v1/ai/conversations?limit=30&cursor=cursor+value",
    );
    expect(fetchMock.mock.calls[1]?.[1]).toMatchObject({
      method: "POST",
      body: JSON.stringify({ modelAlias: "free-general" }),
    });
    expect(fetchMock.mock.calls[2]?.[0]).toBe(
      "/api/v1/ai/conversations/conversation%2F1/messages?limit=30",
    );
  });
});
