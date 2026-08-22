import { afterEach, describe, expect, it, vi } from "vitest";

import { createIdempotencyKey, isUUID } from "../idempotency-key";

import {
  applySnapshotParts,
  cancelPolymarketOrder,
  fetchPolymarketOpenOrders,
  mapPolymarketAccountEvent,
  mapPolymarketOpenOrder,
  mapSnapshot,
  mergeSnapshot,
  placePolymarketOrder,
} from "./polymarket";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("createIdempotencyKey", () => {
  it("creates a UUID when randomUUID is unavailable", () => {
    vi.stubGlobal("crypto", {
      getRandomValues(bytes: Uint8Array) {
        bytes.fill(0xab);
        return bytes;
      },
    });

    expect(createIdempotencyKey()).toBe("abababab-abab-4bab-abab-abababababab");
  });

  it("creates a UUID when crypto is unavailable", () => {
    vi.stubGlobal("crypto", undefined);

    expect(isUUID(createIdempotencyKey())).toBe(true);
  });
});

describe("polymarket API mapper", () => {
  it("maps decimal strings without inventing missing prices", () => {
    const snapshot = mapSnapshot({
      market: {
        id: "m1",
        asset: "BTC",
        period: "5m",
        title: "BTC Up or Down",
        windowStart: "2026-08-08T08:00:00Z",
        windowEnd: "2026-08-08T08:05:00Z",
        active: true,
      },
      openPrice: "",
      chainlinkPrice: "65432.125",
      upAsk: "0.51",
      priceSeries: [
        {
          timestamp: "2026-08-08T08:01:00Z",
          openPrice: "",
          chainlinkPrice: "65432.125",
        },
      ],
      stale: true,
      version: "v1",
    });

    expect(snapshot.openPrice).toBeNull();
    expect(snapshot.chainlinkPrice).toBe(65432.125);
    expect(snapshot.upAsk).toBe(0.51);
    expect(snapshot.priceSeries[0].openPrice).toBeNull();
    expect(snapshot.stale).toBe(true);
  });

  it("merges incremental price points and keeps quote updates", () => {
    const base = mapSnapshot({
      market: { id: "m1", asset: "BTC", period: "5m", active: true },
      chainlinkPrice: "100",
      priceSeries: [
        {
          timestamp: "2026-08-08T08:00:00Z",
          chainlinkPrice: "100",
        },
      ],
      version: "v1",
    });
    const delta = mapSnapshot({
      market: { id: "m1", asset: "BTC", period: "5m", active: true },
      chainlinkPrice: "101",
      upAsk: "0.52",
      priceSeries: [
        {
          timestamp: "2026-08-08T08:00:05Z",
          chainlinkPrice: "101",
        },
      ],
      version: "v2",
    });
    const merged = mergeSnapshot(base, delta);
    expect(merged.priceSeries).toHaveLength(2);
    expect(merged.chainlinkPrice).toBe(101);
    expect(merged.upAsk).toBe(0.52);
  });

  it("replaces price series when market id changes", () => {
    const oldWindow = mapSnapshot({
      market: { id: "m-old", asset: "BTC", period: "5m", active: true },
      chainlinkPrice: "100",
      priceSeries: Array.from({ length: 10 }, (_, index) => ({
        timestamp: `2026-08-08T08:0${index}:00Z`,
        chainlinkPrice: String(100 + index),
      })),
      version: "v1",
    });
    const newWindow = mapSnapshot({
      market: { id: "m-new", asset: "BTC", period: "5m", active: true },
      chainlinkPrice: "200",
      priceSeries: [],
      version: "v1",
    });
    const merged = mergeSnapshot(oldWindow, newWindow);
    expect(merged.market.id).toBe("m-new");
    expect(merged.priceSeries).toHaveLength(0);
    expect(merged.chainlinkPrice).toBe(200);
  });

  it("does not append old points when market id changes with a single delta", () => {
    const oldWindow = mapSnapshot({
      market: { id: "m-old", asset: "BTC", period: "5m", active: true },
      chainlinkPrice: "100",
      priceSeries: [
        {
          timestamp: "2026-08-08T08:00:00Z",
          chainlinkPrice: "100",
        },
      ],
      version: "v1",
    });
    const newWindow = mapSnapshot({
      market: { id: "m-new", asset: "BTC", period: "5m", active: true },
      chainlinkPrice: "201",
      priceSeries: [
        {
          timestamp: "2026-08-08T08:05:01Z",
          chainlinkPrice: "201",
        },
      ],
      version: "v1",
    });
    const merged = mergeSnapshot(oldWindow, newWindow);
    expect(merged.priceSeries).toHaveLength(1);
    expect(merged.priceSeries[0].chainlinkPrice).toBe(201);
    expect(merged.priceSeries[0].timestamp).toBe("2026-08-08T08:05:01Z");
  });

  it("keeps chart points when incoming snapshot has no price series", () => {
    const base = mapSnapshot({
      market: { id: "m1", asset: "BTC", period: "5m", active: true },
      chainlinkPrice: "100",
      priceSeries: [
        {
          timestamp: "2026-08-08T08:00:00Z",
          chainlinkPrice: "100",
        },
      ],
      version: "v1",
    });
    const quoteOnly = mapSnapshot({
      market: { id: "m1", asset: "BTC", period: "5m", active: true },
      chainlinkPrice: "101",
      upAsk: "0.52",
      priceSeries: [],
      version: "v2",
    });
    const { liveSnapshot, chartPoints } = applySnapshotParts(base, base.priceSeries, quoteOnly);
    expect(chartPoints).toHaveLength(1);
    expect(chartPoints[0].chainlinkPrice).toBe(100);
    expect(liveSnapshot.chainlinkPrice).toBe(101);
    expect(liveSnapshot.upAsk).toBe(0.52);
  });

  it("maps open-order decimal fields", () => {
    expect(
      mapPolymarketOpenOrder({
        id: "order-1",
        tokenId: "token-1",
        conditionId: "condition-1",
        marketTitle: "BTC Up or Down",
        outcome: "Up",
        side: "buy",
        price: "0.51",
        originalSize: "12.5",
        matchedSize: "2.25",
        remainingSize: "10.25",
        status: "LIVE",
        orderType: "GTC",
        createdAt: "2026-08-09T08:00:00Z",
      }),
    ).toEqual({
      id: "order-1",
      tokenId: "token-1",
      conditionId: "condition-1",
      marketTitle: "BTC Up or Down",
      outcome: "Up",
      side: "buy",
      price: 0.51,
      originalSize: 12.5,
      matchedSize: 2.25,
      remainingSize: 10.25,
      status: "LIVE",
      orderType: "GTC",
      createdAt: "2026-08-09T08:00:00Z",
    });
  });

  it("fetches account open orders and maps cache metadata", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          data: [
            {
              id: "order-1",
              side: "sell",
              price: "0.48",
              originalSize: "5",
              matchedSize: "1",
              remainingSize: "4",
            },
          ],
          stale: true,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    await expect(fetchPolymarketOpenOrders(42)).resolves.toEqual({
      openOrders: [
        expect.objectContaining({
          id: "order-1",
          side: "sell",
          price: 0.48,
          remainingSize: 4,
        }),
      ],
      stale: true,
    });
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/polymarket/accounts/42/open-orders",
      expect.objectContaining({
        credentials: "include",
        cache: "no-store",
      }),
    );
  });

  it("maps account order events", () => {
    const event = mapPolymarketAccountEvent({
      type: "order",
      openOrders: [{ id: "order-1", price: "0.51", originalSize: "10" }],
      portfolioChanged: false,
    });
    expect(event.type).toBe("order");
    expect(event.openOrders[0]).toEqual(
      expect.objectContaining({ id: "order-1", price: 0.51, originalSize: 10 }),
    );
  });

  it("cancels an account order", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({ data: { orderId: "order/1", status: "canceled" } }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);
    await expect(cancelPolymarketOrder(42, "order/1")).resolves.toEqual({
      orderId: "order/1",
      status: "canceled",
      message: "",
    });
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/polymarket/accounts/42/orders/order%2F1",
      expect.objectContaining({ method: "DELETE", credentials: "include" }),
    );
  });

  it.each(["up", "down"] as const)(
    "submits a buy %s order when randomUUID is unavailable",
    async (outcome) => {
      vi.stubGlobal("crypto", {
        getRandomValues(bytes: Uint8Array) {
          bytes.fill(outcome === "up" ? 0x11 : 0x22);
          return bytes;
        },
      });
      const fetchMock = vi.fn().mockImplementation(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              data: {
                id: `order-${outcome}`,
                tradingAccountId: 42,
                marketId: "market-1",
                outcome,
                side: "buy",
                requestedAmount: "10",
                amountUnit: "usd",
                status: "submitted",
              },
            }),
            { status: 200, headers: { "Content-Type": "application/json" } },
          ),
        ),
      );
      vi.stubGlobal("fetch", fetchMock);

      await placePolymarketOrder({
        tradingAccountId: 42,
        marketId: "market-1",
        outcome,
        side: "buy",
        amount: "10",
        amountUnit: "usd",
      });

      expect(fetchMock).toHaveBeenCalledOnce();
      const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
      const body = JSON.parse(String(init.body)) as Record<string, unknown>;
      const key = String(body.idempotencyKey);
      expect(isUUID(key)).toBe(true);
      expect(body.outcome).toBe(outcome);
      expect(new Headers(init.headers).get("Idempotency-Key")).toBe(key);
    },
  );

  it("submits a GTC limit order with shares and limit price", async () => {
    vi.stubGlobal("crypto", {
      randomUUID: () => "11111111-1111-4111-8111-111111111111",
    });
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          data: {
            id: "order-limit",
            tradingAccountId: 42,
            marketId: "market-1",
            outcome: "down",
            side: "buy",
            requestedAmount: "5",
            amountUnit: "shares",
            executionType: "limit",
            limitPrice: "0.47",
            clobOrderType: "GTC",
            status: "open",
          },
        }),
        { status: 202, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    await expect(
      placePolymarketOrder({
        tradingAccountId: 42,
        marketId: "market-1",
        outcome: "down",
        side: "buy",
        amount: "5",
        amountUnit: "shares",
        executionType: "limit",
        limitPrice: "0.47",
      }),
    ).resolves.toMatchObject({
      executionType: "limit",
      limitPrice: 0.47,
      clobOrderType: "GTC",
    });

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(JSON.parse(String(init.body))).toMatchObject({
      outcome: "down",
      amount: "5",
      amountUnit: "shares",
      executionType: "limit",
      limitPrice: "0.47",
    });
  });
});
