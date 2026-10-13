import { describe, expect, it } from "vitest";

import { mapJevDecision, mapJevPosition, mapJevSnapshot } from "./jev-arb";

describe("mapJevDecision", () => {
  it("keeps historical metrics unset instead of coercing sample count to 0", () => {
    const got = mapJevDecision(
      {
        id: "9",
        positionId: null,
        candidateKey: "single:binance:BTCUSDT:BTCUSDT",
        kind: "single",
        decision: "hold",
        policyStatus: "accepted",
        executionStatus: "not_applicable",
        enterProbability: "0",
        exitProbability: "0",
        confidence: "0",
        expectedNetReturn24h: null,
        rationale: "",
        promptVersion: "DecisionPromptV4",
        policyVersion: "JEVPolicyV3",
        modelRequested: "",
        modelActual: "",
        errorMessage: null,
        decisionAt: "2026-09-18T12:00:00Z",
        executionStartedAt: "",
        fillDeadline: "",
      },
      0,
    );
    expect(got.expectedNetReturn24h).toBeNull();
    expect(got.currentMidBasisBps).toBeNull();
    expect(got.basisSampleCount).toBeNull();
    expect(got.basisModelVersion).toBeNull();
  });

  it("maps a populated sample count", () => {
    const got = mapJevDecision(
      {
        id: "10",
        candidateKey: "single:binance:BTCUSDT:BTCUSDT",
        kind: "single",
        decision: "enter",
        policyStatus: "accepted",
        executionStatus: "filled",
        enterProbability: "0.8",
        exitProbability: "0",
        confidence: "0.7",
        expectedNetReturn24h: "0.002",
        currentMidBasisBps: "12.5",
        basisSampleCount: 18,
        basisModelVersion: "conditional_mid_replay_v1",
        rationale: "",
        promptVersion: "DecisionPromptV4",
        policyVersion: "JEVPolicyV3",
        modelRequested: "jev-latest",
        modelActual: "jev-live",
        decisionAt: "2026-09-18T12:00:00Z",
        executionStartedAt: "",
        fillDeadline: "",
      },
      0,
    );
    expect(got.basisSampleCount).toBe(18);
    expect(got.basisModelVersion).toBe("conditional_mid_replay_v1");
    expect(got.currentMidBasisBps).toBe("12.5");
  });
});

describe("mapJevSnapshot", () => {
  it("maps basisForecastPassed on both funnels", () => {
    const got = mapJevSnapshot({
      status: "running",
      degradedReasons: [],
      lastSuccessfulAt: "",
      lastDecisionAt: "",
      lastMarketDataAt: "",
      serverTime: "",
      model: "",
      modelRequested: "jev-latest",
      promptVersion: "DecisionPromptV4",
      policyVersion: "JEVPolicyV3",
      simulated: true,
      equity: "10000",
      netPnl: "0",
      fundingPnl: "0",
      realizedSpreadPnl: "0",
      unrealizedSpreadPnl: "0",
      cumulativeCost: "0",
      maxDrawdown: "0",
      initialEquity: "10000",
      paperRoundTripCostBps: "12",
      usdtUsdcOneToOne: true,
      singleEntryEmptyReason: "basis_history_insufficient",
      crossEntryEmptyReason: null,
      strategySingleStatus: "running",
      strategyCrossStatus: "running",
      openPositionCount: 0,
      entryCandidateCount: 0,
      typesafeConfigured: true,
      snapshotVersion: "1",
      singleFilter: { input: 10, contractPassed: 8, basisForecastPassed: 2, final: 1 },
      crossFilter: { input: 6, contractPassed: 6, basisForecastPassed: 0, final: 0 },
      decisionJobs: 0,
      typesafeRequests: 0,
      typesafeSuccess: 0,
      typesafeError: 0,
      modelStatus: "not_called",
      lastModelCallAt: "",
    });
    expect(got.singleFilter.basisForecastPassed).toBe(2);
    expect(got.crossFilter.basisForecastPassed).toBe(0);
    expect(got.singleEntryEmptyReason).toBe("basis_history_insufficient");
  });
});

describe("mapJevPosition", () => {
  it("keeps missing entry mid basis null", () => {
    const got = mapJevPosition(
      {
        id: "1",
        kind: "single",
        status: "open",
        candidateKey: "single:binance:BTCUSDT:BTCUSDT",
        baseAsset: "BTC",
        quoteAsset: "USDT",
        globalSymbol: "BTCUSDT",
        longExchange: "binance",
        longSymbol: "BTCUSDT",
        longProduct: "spot",
        shortExchange: "binance",
        shortSymbol: "BTCUSDT",
        shortProduct: "perpetual",
        sharedBaseQty: "1",
        longQty: "1",
        shortQty: "-1",
        longEntryPrice: "100",
        shortEntryPrice: "101",
        longMarkPrice: "100",
        shortMarkPrice: "101",
        realizedSpreadPnl: "0",
        unrealizedSpreadPnl: "0",
        fundingPnl: "0",
        costPaid: "0",
        netPnl: "0",
        openedAt: "2026-09-18T12:00:00Z",
        closedAt: "",
        lastMarkAt: "",
      },
      0,
    );
    expect(got.entryMidBasisBps).toBeNull();
  });
});
