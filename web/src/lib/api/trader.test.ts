import { afterEach, describe, expect, it, vi } from "vitest";

import {
  cancelTraderTwap,
  cancelTraderOrder,
  createTraderTwap,
  fetchTraderInstruments,
  fetchTraderOrder,
  fetchTraderOrderPage,
  fetchTraderTwap,
  fetchTraderTwapOrders,
  fetchTraderTwapPage,
  placeTraderOrder,
} from "./trader";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("trader api client", () => {
  it("loads instruments and places an order with an idempotency key", async () => {
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(async (input) => {
      const url = String(input);
      if (url.includes("/instruments")) {
        return new Response(JSON.stringify({
          data: [{
            id: 7, exchange: "binance", contractType: "perpetual",
            exchangeSymbol: "BTCUSDT", baseAsset: "BTC", quoteAsset: "USDT",
            settleAsset: "USDT", contractSize: "1", priceTick: "0.1", quantityStep: "0.001",
          }],
        }), { status: 200, headers: { "Content-Type": "application/json" } });
      }
      return new Response(JSON.stringify({
        data: { id: "ord-1", tradingAccountId: 3, side: "buy", orderType: "limit", status: "open" },
      }), { status: 202, headers: { "Content-Type": "application/json" } });
    });
    vi.stubGlobal("fetch", fetchMock);
    const instruments = await fetchTraderInstruments(3, "perpetual");
    expect(instruments[0]?.exchangeSymbol).toBe("BTCUSDT");
    const order = await placeTraderOrder({
      tradingAccountId: 3, instrumentId: 7, side: "buy", orderType: "limit",
      quantity: "0.001", price: "100",
    });
    expect(order.id).toBe("ord-1");
    const headers = (fetchMock.mock.calls[1]?.[1] as RequestInit | undefined)?.headers as Record<string, string>;
    expect(headers["Idempotency-Key"]).toMatch(/^[0-9a-f-]{36}$/i);
  });

  it("loads a single order", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({
      data: { id: "ord-1", status: "open", filledQuantity: "0.001" },
    }), { status: 200, headers: { "Content-Type": "application/json" } })));
    const order = await fetchTraderOrder("ord-1");
    expect(order.filledQuantity).toBe("0.001");
  });

  it("cancels an open order", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({
      data: { id: "ord-1", status: "canceled" },
    }), { status: 200, headers: { "Content-Type": "application/json" } })));
    const order = await cancelTraderOrder("ord-1");
    expect(order.status).toBe("canceled");
  });

  it("loads a paginated history snapshot", async () => {
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(async () => new Response(JSON.stringify({
      data: [{
        id: "ord-1", status: "filled", quantity: "1.2000",
        baseAsset: "BTC", syncState: "synced", lastReconciledAt: "2026-08-17T00:00:00Z",
      }],
      meta: { nextCursor: "ord-1" },
    }), { status: 200, headers: { "Content-Type": "application/json" } }));
    vi.stubGlobal("fetch", fetchMock);
    const page = await fetchTraderOrderPage(3, {
      view: "history", limit: 25, cursor: "previous",
    });
    expect(page.nextCursor).toBe("ord-1");
    expect(page.items[0]?.baseAsset).toBe("BTC");
    expect(String(fetchMock.mock.calls[0]?.[0])).toContain("view=history");
    expect(String(fetchMock.mock.calls[0]?.[0])).toContain("cursor=previous");
  });

  it("creates a TWAP with camelCase fields and an idempotency key", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      void input;
      void init;
      return new Response(JSON.stringify({
        data: {
          id: "twap-1",
          totalQty: "2",
          filledQty: "0",
          orderType: "maker",
          status: "running",
        },
      }), { status: 202, headers: { "Content-Type": "application/json" } });
    });
    vi.stubGlobal("fetch", fetchMock);
    const twap = await createTraderTwap({
      tradingAccountId: 3,
      instrumentId: 7,
      side: "buy",
      totalQty: "2",
      startAt: "2026-08-19T00:00:00Z",
      endAt: "2026-08-19T01:00:00Z",
      intervalSeconds: 60,
      maxQty: "0.1",
      orderType: "maker",
      orderTimeoutSeconds: 30,
    });
    expect(twap.id).toBe("twap-1");
    const init = fetchMock.mock.calls[0]?.[1] as RequestInit;
    const headers = init.headers as Record<string, string>;
    const body = JSON.parse(String(init.body)) as Record<string, unknown>;
    expect(headers["Idempotency-Key"]).toMatch(/^[0-9a-f-]{36}$/i);
    expect(body).toMatchObject({
      totalQuantity: "2",
      intervalSeconds: 60,
      maxQuantity: "0.1",
      executionType: "maker",
      orderTimeoutSeconds: 30,
    });
    expect(body.idempotencyKey).toBeUndefined();
  });

  it("loads TWAP pages, details and child orders, then cancels", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/orders")) {
        return new Response(JSON.stringify({ data: [{ id: "child-1", filledQuantity: "0.2" }] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      if (init?.method === "DELETE") {
        return new Response(JSON.stringify({ data: { id: "twap-1", status: "canceled" } }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      if (url.includes("?")) {
        return new Response(JSON.stringify({
          data: [{ id: "twap-1", totalQty: "1", executedQty: "0.25" }],
          meta: { nextCursor: "next" },
        }), { status: 200, headers: { "Content-Type": "application/json" } });
      }
      return new Response(JSON.stringify({ data: { id: "twap-1", status: "running" } }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    });
    vi.stubGlobal("fetch", fetchMock);

    const page = await fetchTraderTwapPage(3, { view: "running", limit: 25, cursor: "before" });
    expect(page.items[0]?.filledQty).toBe("0.25");
    expect(page.nextCursor).toBe("next");
    expect(String(fetchMock.mock.calls[0]?.[0])).toContain("accountId=3");
    expect(String(fetchMock.mock.calls[0]?.[0])).toContain("view=running");
    await fetchTraderTwapPage(undefined, { view: "closed" });
    expect(String(fetchMock.mock.calls[1]?.[0])).not.toContain("accountId=");
    expect(String(fetchMock.mock.calls[1]?.[0])).toContain("view=closed");
    expect((await fetchTraderTwap("twap-1")).status).toBe("running");
    expect((await fetchTraderTwapOrders("twap-1"))[0]?.id).toBe("child-1");
    expect((await cancelTraderTwap("twap-1")).status).toBe("canceled");
  });
});
