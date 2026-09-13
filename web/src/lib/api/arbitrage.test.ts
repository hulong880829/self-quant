import { afterEach, describe, expect, it, vi } from "vitest";

import {
  ArbitrageCreateFailure,
  annualizedPercentToRatio,
  annualizedRatioToPercent,
  closeArbitrageCombination,
  createArbitrageCombination,
  fetchArbitrageCombination,
  fetchArbitrageCombinations,
  formatAnnualizedRatioAsPercent,
  formatSignedAnnualizedRatioAsPercent,
  mapArbitrageCombination,
  signedAnnualizedPercentToRatio,
  updateArbitrageCombination,
} from "./arbitrage";

const combination = {
  id: "arb-1",
  productName: "核心账户",
  status: "running",
  legA: {
    tradingAccountId: 3,
    accountName: "Binance Main",
    exchange: "binance",
    contractType: "perpetual",
    instrumentId: 7,
    exchangeSymbol: "BTCUSDT",
    baseAsset: "BTC",
    quoteAsset: "USDT",
  },
  legB: {
    tradingAccountId: 4,
    accountName: "OKX Main",
    exchange: "okx",
    contractType: "perpetual",
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
  grossTurnoverNotional: "5000.00",
  consecutiveFailures: 0,
  nextRetryAt: "",
  positionUncertain: false,
  runtimeState: "monitoring",
  runMode: "spread",
  entryDirection: "",
  legALeverage: "4",
  legBLeverage: "4",
  exitPolicy: "",
  exitAnnualizedRate: "",
  exitAfterSeconds: 0,
  targetReachedAt: "",
  scheduledExitAt: "",
  oneShotPhase: "",
  earlyExitFunding8hAnnualizedFloor: "",
  legABasePosition: "25",
  legBBasePosition: "-24.99",
  carryBaseQuantity: "0.01",
  legAAverageEntryPrice: "100",
  legBAverageEntryPrice: "101",
  averageEntrySpreadBps: "100",
  legAUnrealizedPnl: "2.5",
  legBUnrealizedPnl: "-1.5",
  realizedSpreadPnl: "1",
  estimatedFundingPnl: "0.5",
  combinedPositionAnnualized: "0.1842",
  fundingHistoryComplete: false,
  legAVenueBaselineBasePosition: "10",
  legBVenueBaselineBasePosition: "-10",
  venueBaselineCapturedAt: "2026-08-22T09:59:00Z",
  legAExpectedBasePosition: "35",
  legBExpectedBasePosition: "-34.99",
  legAVenueNotional: "2510",
  legBVenueNotional: "-2525",
  legAVenueValuationPrice: "100",
  legBVenueValuationPrice: "101",
  legAVenueValuationAt: "2026-08-22T10:01:00Z",
  legBVenueValuationAt: "2026-08-22T10:01:01Z",
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
  it.each(["exiting", "exited"] as const)(
    "preserves the %s one-shot phase",
    (phase) => {
      expect(
        mapArbitrageCombination({
          ...combination,
          runMode: "one_shot",
          entryDirection: "ask",
          exitPolicy: "time",
          exitAfterSeconds: 3600,
          oneShotPhase: phase,
        }).oneShotPhase,
      ).toBe(phase);
    },
  );

  it("strictly requires decimal strings", () => {
    expect(mapArbitrageCombination(combination).askThresholdBps).toBe("12.50");
    expect(mapArbitrageCombination(combination).legA.contractType).toBe(
      "perpetual",
    );
    expect(mapArbitrageCombination(combination).averageEntrySpreadBps).toBe(
      "100",
    );
    expect(
      mapArbitrageCombination(combination).combinedPositionAnnualized,
    ).toBe("0.1842");
    expect(mapArbitrageCombination(combination).fundingHistoryComplete).toBe(
      false,
    );
    expect(
      mapArbitrageCombination(combination).legAExpectedBasePosition,
    ).toBe("35");
    expect(mapArbitrageCombination(combination).legAVenueNotional).toBe(
      "2510",
    );
    expect(mapArbitrageCombination(combination).legBVenueValuationPrice).toBe(
      "101",
    );
    expect(() =>
      mapArbitrageCombination({ ...combination, targetNotional: 10000 }),
    ).toThrow("response.data.targetNotional 必须是 decimal string");
    expect(() =>
      mapArbitrageCombination({ ...combination, status: "paused" }),
    ).toThrow("response.data.status 不是支持的值");
  });

  it("accepts recovery runtime states and position audit fields", () => {
    const mapped = mapArbitrageCombination({
      ...combination,
      runtimeState: "position_uncertain",
      legAVenueBasePosition: "25.1",
      legBVenueBasePosition: "-25",
      legAPositionDifference: "0.1",
      legBPositionDifference: "-0.01",
      lastPositionReconciledAt: "2026-08-25T05:00:00Z",
    });
    expect(mapped.runtimeState).toBe("position_uncertain");
    expect(mapped.legAPositionDifference).toBe("0.1");
  });

  it("creates with the planned route, body and idempotency header", async () => {
    const fetchMock = vi.fn(
      async (input: RequestInfo | URL, init?: RequestInit) => {
        void input;
        void init;
        return new Response(JSON.stringify({ data: combination }), {
          status: 202,
          headers: { "Content-Type": "application/json" },
        });
      },
    );
    vi.stubGlobal("fetch", fetchMock);

    await createArbitrageCombination(
      {
        productName: "核心账户",
        legAAccountId: 3,
        legAInstrumentId: 7,
        legBAccountId: 4,
        legBInstrumentId: 8,
        askThresholdBps: "12",
        bidThresholdBps: "-8",
        targetNotional: "10000",
        preferredLeg: "a",
        executionMode: "maker_then_hedge",
        runMode: "spread",
        entryDirection: "",
        legALeverage: "4",
        legBLeverage: "4",
        exitPolicy: "",
        exitAnnualizedRate: "",
        exitAfterSeconds: 0,
        earlyExitFunding8hAnnualizedFloor: "",
      },
      "fixed-key",
    );

    const [url, init] = fetchMock.mock.calls[0] ?? [];
    expect(String(url)).toBe("/api/v1/trader/arbitrage-combinations");
    expect(init?.method).toBe("POST");
    expect((init?.headers as Record<string, string>)["Idempotency-Key"]).toBe(
      "fixed-key",
    );
    expect(JSON.parse(String(init?.body))).toMatchObject({
      legAAccountId: 3,
      legBAccountId: 4,
      askThresholdBps: "12",
      bidThresholdBps: "-8",
    });
    expect(JSON.parse(String(init?.body))).not.toHaveProperty(
      "maxDeltaNotional",
    );
    expect(JSON.parse(String(init?.body))).not.toHaveProperty("orderNotional");
    expect(JSON.parse(String(init?.body))).toMatchObject({
      runMode: "spread",
      legALeverage: "4",
      legBLeverage: "4",
    });
  });

  it("surfaces the active account instrument conflict reason", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(
          JSON.stringify({
            code: "active_arbitrage_instrument_conflict",
            error:
              "账户 OKX Main 的 okx BTC-USDT-SWAP 已被运行中套利组合 combo-1 占用，请先关闭该组合",
          }),
          {
            status: 409,
            headers: { "Content-Type": "application/json" },
          },
        ),
      ),
    );
    await expect(
      createArbitrageCombination({
        productName: "核心账户",
        legAAccountId: 3,
        legAInstrumentId: 7,
        legBAccountId: 4,
        legBInstrumentId: 8,
        askThresholdBps: "12.5",
        bidThresholdBps: "-8.25",
        targetNotional: "10000",
        preferredLeg: "a",
        executionMode: "maker_then_hedge",
        runMode: "spread",
        entryDirection: "",
        legALeverage: "4",
        legBLeverage: "4",
        exitPolicy: "",
        exitAnnualizedRate: "",
        exitAfterSeconds: 0,
        earlyExitFunding8hAnnualizedFloor: "",
      }),
    ).rejects.toThrow("BTC-USDT-SWAP 已被运行中套利组合 combo-1 占用");
  });

  it("lists, gets and closes combinations", async () => {
    const fetchMock = vi.fn(
      async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (url.includes("?")) {
          return new Response(
            JSON.stringify({
              data: [combination],
              meta: { nextCursor: "next", total: 1 },
            }),
            { status: 200, headers: { "Content-Type": "application/json" } },
          );
        }
        if (init?.method === "DELETE") {
          return new Response(
            JSON.stringify({
              data: { ...combination, status: "closing" },
            }),
            { status: 202, headers: { "Content-Type": "application/json" } },
          );
        }
        return new Response(
          JSON.stringify({
            data: {
              ...combination,
              orders: [
                {
                  id: "order-1",
                  exchange: "okx",
                  errorCode: "51008",
                  errorMessage: "Insufficient balance",
                  arbitrageExecutionId: "execution-1",
                  arbitrageLeg: "b",
                  arbitrageRole: "hedge",
                },
              ],
              recentExecutions: [
                {
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
                },
              ],
              recentEvents: [
                {
                  id: "event-1",
                  type: "created",
                  message: "",
                  createdAt: "2026-08-22T10:00:00Z",
                },
              ],
            },
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        );
      },
    );
    vi.stubGlobal("fetch", fetchMock);

    const page = await fetchArbitrageCombinations("running", {
      cursor: "before",
      limit: 20,
    });
    expect(page.total).toBe(1);
    expect(page.items[0]?.askSpreadBps).toBe("13.20");
    expect(String(fetchMock.mock.calls[0]?.[0])).toContain(
      "/api/v1/trader/arbitrage-combinations?view=running&limit=20&cursor=before",
    );
    const detail = await fetchArbitrageCombination("arb/1");
    expect(detail.recentExecutions[0]?.direction).toBe("ask");
    expect(detail.orders[0]?.errorCode).toBe("51008");
    expect(detail.orders[0]?.arbitrageExecutionId).toBe("execution-1");
    expect(detail.orders[0]?.arbitrageLeg).toBe("b");
    expect(detail.orders[0]?.arbitrageRole).toBe("hedge");
    expect(String(fetchMock.mock.calls[1]?.[0])).toContain("/arb%2F1");
    expect((await closeArbitrageCombination("arb-1")).status).toBe("closing");
    expect(fetchMock.mock.calls[2]?.[1]?.method).toBe("DELETE");
  });

  it("patches one editable parameter and preserves backend errors", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            data: { ...combination, targetNotional: "12000.00" },
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            error:
              "target notional cannot be below current absolute position 2500",
          }),
          { status: 422, headers: { "Content-Type": "application/json" } },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);

    const updated = await updateArbitrageCombination("arb/1", {
      targetNotional: "12000",
    });
    expect(updated.targetNotional).toBe("12000.00");
    expect(String(fetchMock.mock.calls[0]?.[0])).toContain("/arb%2F1");
    expect(fetchMock.mock.calls[0]?.[1]?.method).toBe("PATCH");
    expect(JSON.parse(String(fetchMock.mock.calls[0]?.[1]?.body))).toEqual({
      targetNotional: "12000",
    });
    await expect(
      updateArbitrageCombination("arb-1", { targetNotional: "1000" }),
    ).rejects.toThrow(
      "target notional cannot be below current absolute position 2500",
    );
  });

  it("surfaces structured create failures with code and leg", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(
          JSON.stringify({
            code: "insufficient_margin",
            error: "okx available margin is insufficient",
            leg: "b",
            details: { exchange: "okx" },
          }),
          { status: 422, headers: { "Content-Type": "application/json" } },
        ),
      ),
    );
    try {
      await createArbitrageCombination({
        productName: "核心账户",
        legAAccountId: 3,
        legAInstrumentId: 7,
        legBAccountId: 4,
        legBInstrumentId: 8,
        askThresholdBps: "12",
        bidThresholdBps: "-8",
        targetNotional: "10000",
        preferredLeg: "a",
        executionMode: "maker_then_hedge",
        runMode: "spread",
        entryDirection: "",
        legALeverage: "4",
        legBLeverage: "4",
        exitPolicy: "",
        exitAnnualizedRate: "",
        exitAfterSeconds: 0,
        earlyExitFunding8hAnnualizedFloor: "",
      });
      throw new Error("expected create to fail");
    } catch (error) {
      expect(error).toBeInstanceOf(ArbitrageCreateFailure);
      expect(error).toMatchObject({
        code: "insufficient_margin",
        leg: "b",
        details: { exchange: "okx" },
      });
    }
  });

  it("maps missing runMode to spread and ignores orderNotional", () => {
    const mapped = mapArbitrageCombination({
      ...combination,
      runMode: undefined,
    });
    expect(mapped.runMode).toBe("spread");
    expect(mapped).not.toHaveProperty("orderNotional");
    expect(mapped.earlyExitFunding8hAnnualizedFloor).toBe("");
  });
});

describe("annualized percent conversion", () => {
  it("shifts percent strings to ratios without float division", () => {
    expect(annualizedPercentToRatio("15")).toBe("0.15");
    expect(annualizedPercentToRatio("12.5")).toBe("0.125");
    expect(annualizedPercentToRatio("0.5")).toBe("0.005");
    expect(annualizedPercentToRatio("100")).toBe("1");
    expect(annualizedPercentToRatio("0")).toBeNull();
    expect(annualizedPercentToRatio("")).toBeNull();
    expect(annualizedPercentToRatio("abc")).toBeNull();
  });

  it("formats stored ratios as percents", () => {
    expect(annualizedRatioToPercent("0.15")).toBe("15");
    expect(annualizedRatioToPercent("0.125")).toBe("12.5");
    expect(formatAnnualizedRatioAsPercent("0.15")).toBe("15.00%");
    expect(formatAnnualizedRatioAsPercent("0.125")).toBe("12.50%");
  });

  it("converts signed 8h funding percents including zero", () => {
    expect(signedAnnualizedPercentToRatio("5")).toBe("0.05");
    expect(signedAnnualizedPercentToRatio("0")).toBe("0");
    expect(signedAnnualizedPercentToRatio("-10")).toBe("-0.1");
    expect(signedAnnualizedPercentToRatio("12.5")).toBe("0.125");
    expect(signedAnnualizedPercentToRatio("")).toBeNull();
    expect(signedAnnualizedPercentToRatio("abc")).toBeNull();
    expect(formatSignedAnnualizedRatioAsPercent("0.05")).toBe("5.00%");
    expect(formatSignedAnnualizedRatioAsPercent("-0.1")).toBe("-10.00%");
    expect(formatSignedAnnualizedRatioAsPercent("0")).toBe("0.00%");
  });
});
