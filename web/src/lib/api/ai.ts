export type AICredentialStatus = "unknown" | "valid" | "invalid";

export interface AICredential {
  provider: string;
  apiKeyMasked: string;
  status: AICredentialStatus;
  lastError: string;
  lastTestedAt: string;
}

export interface AIConversation {
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

export interface AIConversationMessage {
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

export interface AIPaginated<T> {
  data: T[];
  nextCursor: string;
}

export interface AIChatRequest {
  conversationId: string;
  prompt: string;
  clientMessageId: string;
  modelAlias?: string;
}

export interface AIStreamEvent {
  type: "turn" | "tool" | "delta" | "usage" | "done" | "error";
  delta?: string;
  tool?: string;
  error?: string;
  inputTokens?: number;
  outputTokens?: number;
  conversationId?: string;
  userMessageId?: string;
  assistantMessageId?: string;
  title?: string;
  status?: string;
}

export class AIAPIError extends Error {
  constructor(
    message: string,
    public readonly status: number,
  ) {
    super(message);
    this.name = "AIAPIError";
  }
}

function apiBaseUrl(): string {
  return (process.env.NEXT_PUBLIC_API_BASE_URL ?? "").replace(/\/+$/, "");
}

function aiUrl(path: string): string {
  return `${apiBaseUrl()}/api/v1/ai${path}`;
}

async function errorMessage(
  response: Response,
  fallback: string,
): Promise<string> {
  const body = (await response.json().catch(() => ({}))) as { error?: string };
  if (response.status === 401) return "请先登录";
  return body.error || fallback;
}

async function requireOK(response: Response, fallback: string): Promise<void> {
  if (!response.ok) {
    throw new AIAPIError(await errorMessage(response, fallback), response.status);
  }
}

function pageURL(path: string, limit: number, cursor = ""): string {
  const query = new URLSearchParams({ limit: String(limit) });
  if (cursor) query.set("cursor", cursor);
  return `${aiUrl(path)}?${query}`;
}

function mapCredential(value: unknown): AICredential {
  const item = value as Partial<AICredential> | null;
  return {
    provider: item?.provider || "openrouter",
    apiKeyMasked: item?.apiKeyMasked || "",
    status:
      item?.status === "valid" || item?.status === "invalid"
        ? item.status
        : "unknown",
    lastError: item?.lastError || "",
    lastTestedAt: item?.lastTestedAt || "",
  };
}

export async function fetchAICredential(): Promise<AICredential | null> {
  const response = await fetch(aiUrl("/credentials/openrouter"), {
    method: "GET",
    credentials: "include",
    headers: { Accept: "application/json" },
    cache: "no-store",
  });
  if (response.status === 404) return null;
  if (!response.ok) {
    throw new Error(await errorMessage(response, "无法读取 AI Key 状态"));
  }
  const body = (await response.json()) as { data?: unknown };
  return mapCredential(body.data);
}

export async function saveAICredential(
  apiKey: string,
): Promise<AICredential> {
  const response = await fetch(aiUrl("/credentials/openrouter"), {
    method: "POST",
    credentials: "include",
    headers: {
      Accept: "application/json",
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ apiKey }),
  });
  if (!response.ok) {
    throw new Error(await errorMessage(response, "保存 AI Key 失败"));
  }
  const body = (await response.json()) as { data?: unknown };
  return mapCredential(body.data);
}

export async function testAICredential(): Promise<AICredential> {
  const response = await fetch(aiUrl("/credentials/openrouter/test"), {
    method: "POST",
    credentials: "include",
    headers: { Accept: "application/json" },
  });
  if (!response.ok) {
    throw new Error(await errorMessage(response, "检测 AI Key 失败"));
  }
  const body = (await response.json()) as { data?: unknown };
  return mapCredential(body.data);
}

export async function deleteAICredential(): Promise<void> {
  const response = await fetch(aiUrl("/credentials/openrouter"), {
    method: "DELETE",
    credentials: "include",
    headers: { Accept: "application/json" },
  });
  if (!response.ok) {
    throw new Error(await errorMessage(response, "删除 AI Key 失败"));
  }
}

export async function fetchAIConversations(
  limit = 30,
  cursor = "",
): Promise<AIPaginated<AIConversation>> {
  const response = await fetch(pageURL("/conversations", limit, cursor), {
    method: "GET",
    credentials: "include",
    headers: { Accept: "application/json" },
    cache: "no-store",
  });
  await requireOK(response, "无法读取对话列表");
  return response.json() as Promise<AIPaginated<AIConversation>>;
}

export async function createAIConversation(
  modelAlias?: string,
): Promise<AIConversation> {
  const response = await fetch(aiUrl("/conversations"), {
    method: "POST",
    credentials: "include",
    headers: {
      Accept: "application/json",
      "Content-Type": "application/json",
    },
    body: JSON.stringify(modelAlias ? { modelAlias } : {}),
  });
  await requireOK(response, "创建对话失败");
  const body = (await response.json()) as { data: AIConversation };
  return body.data;
}

export async function renameAIConversation(
  conversationId: string,
  title: string,
): Promise<AIConversation> {
  const response = await fetch(
    aiUrl(`/conversations/${encodeURIComponent(conversationId)}/rename`),
    {
      method: "POST",
      credentials: "include",
      headers: {
        Accept: "application/json",
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ title }),
    },
  );
  await requireOK(response, "重命名对话失败");
  const body = (await response.json()) as { data: AIConversation };
  return body.data;
}

export async function deleteAIConversation(
  conversationId: string,
): Promise<void> {
  const response = await fetch(
    aiUrl(`/conversations/${encodeURIComponent(conversationId)}`),
    {
      method: "DELETE",
      credentials: "include",
      headers: { Accept: "application/json" },
    },
  );
  await requireOK(response, "删除对话失败");
}

export async function fetchAIConversationMessages(
  conversationId: string,
  limit = 30,
  cursor = "",
): Promise<AIPaginated<AIConversationMessage>> {
  const response = await fetch(
    pageURL(
      `/conversations/${encodeURIComponent(conversationId)}/messages`,
      limit,
      cursor,
    ),
    {
      method: "GET",
      credentials: "include",
      headers: { Accept: "application/json" },
      cache: "no-store",
    },
  );
  await requireOK(response, "无法读取对话消息");
  return response.json() as Promise<AIPaginated<AIConversationMessage>>;
}

export async function streamAIChat(
  request: AIChatRequest,
  onEvent: (event: AIStreamEvent) => void,
  signal?: AbortSignal,
): Promise<void> {
  const response = await fetch(aiUrl("/chat"), {
    method: "POST",
    credentials: "include",
    headers: {
      Accept: "text/event-stream",
      "Content-Type": "application/json",
    },
    body: JSON.stringify(request),
    signal,
  });
  if (!response.ok) {
    throw new AIAPIError(
      await errorMessage(response, "AI 对话请求失败"),
      response.status,
    );
  }
  if (!response.body) {
    throw new Error("浏览器不支持流式响应");
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let completed = false;
  try {
    while (true) {
      const { value, done } = await reader.read();
      buffer += decoder.decode(value, { stream: !done }).replace(/\r\n/g, "\n");
      let separator = buffer.indexOf("\n\n");
      while (separator >= 0) {
        const frame = buffer.slice(0, separator);
        buffer = buffer.slice(separator + 2);
        const event = parseSSEFrame(frame);
        if (event) {
          onEvent(event);
          if (event.type === "error") {
            throw new Error(event.error || "AI 流式响应失败");
          }
          if (event.type === "done") completed = true;
        }
        separator = buffer.indexOf("\n\n");
      }
      if (done) break;
    }
  } finally {
    reader.releaseLock();
  }
  if (!completed && !signal?.aborted) {
    throw new Error("AI 流式响应意外结束");
  }
}

export function parseSSEFrame(frame: string): AIStreamEvent | null {
  let eventType = "";
  const dataLines: string[] = [];
  for (const line of frame.split("\n")) {
    if (line.startsWith(":")) continue;
    if (line.startsWith("event:")) {
      eventType = line.slice(6).trim();
    } else if (line.startsWith("data:")) {
      dataLines.push(line.slice(5).trimStart());
    }
  }
  if (!eventType || dataLines.length === 0) return null;
  const data = JSON.parse(dataLines.join("\n")) as Partial<AIStreamEvent>;
  return {
    type: eventType as AIStreamEvent["type"],
    delta: data.delta,
    tool: data.tool,
    error: data.error,
    inputTokens: data.inputTokens,
    outputTokens: data.outputTokens,
    conversationId: data.conversationId,
    userMessageId: data.userMessageId,
    assistantMessageId: data.assistantMessageId,
    title: data.title,
    status: data.status,
  };
}
