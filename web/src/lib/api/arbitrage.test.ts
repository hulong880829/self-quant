import { afterEach, describe, expect, it, vi } from "vitest";

import {
  closeArbitrageCombination,
  createArbitrageCombination,
  fetchArbitrageCombination,
  fetchArbitrageCombinations,
  mapArbitrageCombination,
} from "./arbitrage";

const combination = {
  id: "arb-1",
  productName: "核心账户",
  status: "running",
  legA: {
    tradingAccountId: 3,
    accountName: "Binance Main",
    exchange: "binance",
    instrumentId: 7,
    exchangeSymbol: "BTCUSDT",
    baseAsset: "BTC",
    quoteAsset: "USDT",
  },
  legB: {
    tradingAccountId: 4,
    accountName: "OKX Main",
    exchange: "okx",
    instrumentId: 8,
    exchangeSymbol: "BTC-USDT-SWAP",
    baseAsset: "BTC",
    quoteAsset: "USDT",
  },
  askThresholdBps: "12.50",
  bidThresholdBps: "-8.25",
  targetNotional: "10000.00",
  positionNotional: "2500.00",
  cumulativeTurnoverNotional: "2500.00",
  consecutiveFailures: 0,
  nextRetryAt: "",
  positionUncertain: false,
  orderNotional: "500.00",
  maxDeltaNotional: "100.00",
  preferredLeg: "a",
  executionMode: "maker_then_hedge",
  askSpreadBps: "13.20",
  bidSpreadBps: "-6.80",
  marketDataStale: false,
  errorMessage: "",
  createdAt: "2026-08-22T10:00:00Z",
  updatedAt: "2026-08-22T10:01:00Z",
  closedAt: "",
};

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("arbitrage api client", () => {
  it("strictly requires decimal strings", () => {
    expect(mapArbitrageCombination(combination).askThresholdBps).toBe("12.50");
    expect(() => mapArbitrageCombination({ ...combination, targetNotional: 10000 }))
      .toThrow("response.data.targetNotional 必须是 decimal string");
    expect(() => mapArbitrageCombination({ ...combination, status: "paused" }))
      .toThrow("response.data.status 不是支持的值");
  });

  it("creates with the planned route, body and idempotency header", async () => {
    const fetchMock = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) =>
      new Response(JSON.stringify({ data: combination }), {
        status: 202,
        headers: { "Content-Type": "application/json" },
      }));
    vi.stubGlobal("fetch", fetchMock);

    await createArbitrageCombination({
      productName: "核心账户",
      legAAccountId: 3,
      legAInstrumentId: 7,
      legBAccountId: 4,
      legBInstrumentId: 8,
      askThresholdBps: "12",
      bidThresholdBps: "-8",
      targetNotional: "10000",
      orderNotional: "500",
      maxDeltaNotional: "100",
      preferredLeg: "a",
      executionMode: "maker_then_hedge",
    }, "fixed-key");

    const [url, init] = fetchMock.mock.calls[0] ?? [];
    expect(String(url)).toBe("/api/v1/trader/arbitrage-combinations");
    expect(init?.method).toBe("POST");
    expect((init?.headers as Record<string, string>)["Idempotency-Key"]).toBe("fixed-key");
    expect(JSON.parse(String(init?.body))).toMatchObject({
      legAAccountId: 3,
      legBAccountId: 4,
      askThresholdBps: "12",
      bidThresholdBps: "-8",
    });
  });

  it("lists, gets and closes combinations", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.includes("?")) {
        return new Response(JSON.stringify({
          data: [combination],
          meta: { nextCursor: "next", total: 1 },
        }), { status: 200, headers: { "Content-Type": "application/json" } });
      }
      if (init?.method === "DELETE") {
        return new Response(JSON.stringify({
          data: { ...combination, status: "closing" },
        }), { status: 202, headers: { "Content-Type": "application/json" } });
      }
      return new Response(JSON.stringify({
        data: {
          ...combination,
          recentExecutions: [{
            id: "execution-1",
            direction: "ask",
            status: "hedged",
            triggerSpreadBps: "13.2",
            targetBaseQuantity: "0.01",
            filledBaseQuantity: "0.01",
            deltaNotional: "0",
            errorMessage: "",
            createdAt: "2026-08-22T10:00:00Z",
            updatedAt: "2026-08-22T10:00:01Z",
          }],
          recentEvents: [{
            id: "event-1",
            type: "created",
            message: "",
            createdAt: "2026-08-22T10:00:00Z",
          }],
        },
      }), { status: 200, headers: { "Content-Type": "application/json" } });
    });
    vi.stubGlobal("fetch", fetchMock);

    const page = await fetchArbitrageCombinations("running", { cursor: "before", limit: 20 });
    expect(page.total).toBe(1);
    expect(page.items[0]?.askSpreadBps).toBe("13.20");
    expect(String(fetchMock.mock.calls[0]?.[0])).toContain(
      "/api/v1/trader/arbitrage-combinations?view=running&limit=20&cursor=before",
    );
    expect((await fetchArbitrageCombination("arb/1")).recentExecutions[0]?.direction).toBe("ask");
    expect(String(fetchMock.mock.calls[1]?.[0])).toContain("/arb%2F1");
    expect((await closeArbitrageCombination("arb-1")).status).toBe("closing");
    expect(fetchMock.mock.calls[2]?.[1]?.method).toBe("DELETE");
  });
});
