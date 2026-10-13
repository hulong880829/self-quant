// @vitest-environment jsdom

import * as React from "react";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { JevSnapshot } from "@/lib/api/jev-arb";
import {
  fetchJevDecisions,
  fetchJevEquity,
  fetchJevPositions,
  fetchJevSnapshot,
  fetchJevTrades,
} from "@/lib/api/jev-arb";

vi.mock("@/lib/api/jev-arb", () => ({
  fetchJevSnapshot: vi.fn(),
  fetchJevPositions: vi.fn(),
  fetchJevDecisions: vi.fn(),
  fetchJevTrades: vi.fn(),
  fetchJevEquity: vi.fn(),
}));

vi.mock("next/link", () => ({
  default: React.forwardRef<
    HTMLAnchorElement,
    React.AnchorHTMLAttributes<HTMLAnchorElement> & { href: string }
  >(function MockLink({ href, children, ...props }, ref) {
    return (
      <a ref={ref} href={href} {...props}>
        {children}
      </a>
    );
  }),
}));

import {
  decisionEmptyCopy,
  executionStatusLabel,
  formatAnnualizedReturn,
  formatCandidateLabel,
  formatOptionalCount,
  formatOptionalMetric,
  formatPercentReturn,
  JevArbDashboard,
  orderJevDecisions,
} from "./dashboard";

function emptyFilter() {
  return {
    input: 0,
    contractPassed: 0,
    freshPassed: 0,
    turnoverPassed: 0,
    bboFreshPassed: 0,
    depthPassed: 0,
    basisForecastPassed: 0,
    final: 0,
  };
}

function emptyDecision(overrides: Partial<import("@/lib/api/jev-arb").JevDecision> = {}): import("@/lib/api/jev-arb").JevDecision {
  return {
    id: "1",
    positionId: null,
    candidateKey: "single:binance:BTCUSDT:BTCUSDT",
    kind: "single",
    decision: "enter",
    policyStatus: "accepted",
    executionStatus: "filled",
    enterProbability: "0.8",
    exitProbability: "0",
    confidence: "0.7",
    expectedNetReturn24h: "0.001",
    fundingReturn24h: null,
    holdingHorizonNetReturn: null,
    expectedPaybackMinutes: null,
    currentMidBasisBps: null,
    entryMidBasisBps: null,
    basisChangeSinceEntryBps: null,
    expectedBasisReturn24h: null,
    basisReturnP10: null,
    basisProfitProbability: null,
    basisForecastConfidence: null,
    basisSampleCount: null,
    basisModelVersion: null,
    rationale: "",
    promptVersion: "DecisionPromptV4",
    policyVersion: "JEVPolicyV3",
    modelRequested: "gpt-5.4",
    modelActual: "jev-live",
    errorMessage: null,
    decisionAt: "2026-09-18T12:00:00Z",
    executionStartedAt: "2026-09-18T12:00:00Z",
    fillDeadline: "2026-09-18T12:00:05Z",
    strategyVersion: "JEVStrategyV6",
    parseErrorType: null,
    ...overrides,
  };
}

function snapshotFixture(overrides: Partial<JevSnapshot> = {}): JevSnapshot {
  return {
    status: "running",
    degradedReasons: [],
    lastSuccessfulAt: "2026-09-18T12:00:00Z",
    lastDecisionAt: "",
    lastMarketDataAt: "2026-09-18T12:00:00Z",
    serverTime: "2026-09-18T12:00:00Z",
    model: "",
    modelRequested: "jev-latest",
    promptVersion: "DecisionPromptV2",
    policyVersion: "JEVPolicyV1",
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
    singleEntryEmptyReason: "no_rates",
    crossEntryEmptyReason: "no_spreads",
    strategySingleStatus: "running",
    strategyCrossStatus: "running",
    openPositionCount: 0,
    entryCandidateCount: 0,
    typesafeConfigured: true,
    snapshotVersion: "3",
    singleFilter: { ...emptyFilter(), positive24hPassed: 0, spotMatched: 0 },
    crossFilter: { ...emptyFilter(), positiveEdgePassed: 0 },
    decisionJobs: 0,
    typesafeRequests: 0,
    typesafeSuccess: 0,
    typesafeError: 0,
    modelStatus: "not_called",
    lastModelCallAt: "",
    strategyVersion: "JEVStrategyV6",
    lifecycleEquity: "10000",
    lifecycleNetPnl: "0",
    lifecycleFundingPnl: "0",
    lifecycleRealizedSpreadPnl: "0",
    lifecycleUnrealizedSpreadPnl: "0",
    lifecycleCumulativeCost: "0",
    availableStrategyVersions: ["JEVStrategyV6"],
    ...overrides,
  };
}

function mockLists() {
  vi.mocked(fetchJevPositions).mockResolvedValue({ status: "updated", data: [], etag: "1" });
  vi.mocked(fetchJevDecisions).mockResolvedValue({ status: "updated", data: [], etag: "1" });
  vi.mocked(fetchJevTrades).mockResolvedValue({ status: "updated", data: [], etag: "1" });
  vi.mocked(fetchJevEquity).mockResolvedValue({
    status: "updated",
    data: { range: "24h", points: [] },
    etag: "1",
  });
}

describe("JevArbDashboard", () => {
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
  });

  it("links back to the funding workspace", async () => {
    mockLists();
    vi.mocked(fetchJevSnapshot).mockResolvedValue({
      status: "updated",
      data: snapshotFixture(),
      etag: "1",
    });
    render(<JevArbDashboard />);
    const back = await screen.findByRole("link", { name: "返回套利机会" });
    expect(back.getAttribute("href")).toBe("/funding");
    expect(back.getAttribute("aria-label")).toBe("返回套利机会");
  });

  it("shows compact funnel copy and paper mode", async () => {
    mockLists();
    vi.mocked(fetchJevSnapshot).mockResolvedValue({
      status: "updated",
      data: snapshotFixture({
        singleFilter: {
          input: 12,
          contractPassed: 10,
          positive24hPassed: 4,
          freshPassed: 4,
          turnoverPassed: 3,
          spotMatched: 2,
          bboFreshPassed: 1,
          depthPassed: 0,
          basisForecastPassed: 0,
          final: 0,
        },
        crossFilter: {
          input: 8,
          contractPassed: 8,
          positiveEdgePassed: 5,
          freshPassed: 5,
          turnoverPassed: 4,
          bboFreshPassed: 2,
          depthPassed: 1,
          basisForecastPassed: 0,
          final: 0,
        },
      }),
      etag: "1",
    });
    render(<JevArbDashboard />);
    expect(await screen.findByText("本轮没有通过行情规则的候选")).toBeTruthy();
    expect(screen.getByText("周期正常")).toBeTruthy();
    expect(screen.getByText("模型未调用")).toBeTruthy();
    expect(screen.getByText("纸面模式")).toBeTruthy();
    expect(screen.getByText("本轮筛选")).toBeTruthy();
    expect(screen.getByText("单所：12 输入 → 10 普通永续 → 0 最终候选")).toBeTruthy();
    expect(screen.getByText("跨所：8 输入 → 8 普通永续组合 → 0 最终候选")).toBeTruthy();
    expect(screen.queryByText("Positive24h / PositiveEdge")).toBeNull();
    expect(screen.queryByText("Spot")).toBeNull();
  });

  it("shows model-not-called copy when candidates exist", async () => {
    mockLists();
    vi.mocked(fetchJevSnapshot).mockResolvedValue({
      status: "updated",
      data: snapshotFixture({
        entryCandidateCount: 4,
        modelStatus: "not_called",
        singleFilter: { ...emptyFilter(), positive24hPassed: 4, spotMatched: 4, final: 4 },
      }),
      etag: "1",
    });
    render(<JevArbDashboard />);
    expect(await screen.findByText("发现候选，模型尚未调用")).toBeTruthy();
  });

  it("shows model failure copy", async () => {
    mockLists();
    vi.mocked(fetchJevSnapshot).mockResolvedValue({
      status: "updated",
      data: snapshotFixture({
        status: "degraded",
        entryCandidateCount: 2,
        modelStatus: "error",
        typesafeError: 1,
        degradedReasons: ["typesafe_error"],
      }),
      etag: "1",
    });
    render(<JevArbDashboard />);
    expect(await screen.findByText("模型调用失败")).toBeTruthy();
    expect(screen.getByText("周期降级")).toBeTruthy();
    expect(screen.getByText("模型 0/0")).toBeTruthy();
  });

  it("renders grouped decisions, chinese statuses, and hides a flat equity line", async () => {
    vi.mocked(fetchJevPositions).mockResolvedValue({ status: "updated", data: [], etag: "1" });
    vi.mocked(fetchJevTrades).mockResolvedValue({ status: "updated", data: [], etag: "1" });
    vi.mocked(fetchJevEquity).mockResolvedValue({
      status: "updated",
      data: {
        range: "24h",
        points: [
          { ts: "2026-09-18T12:00:00Z", equity: "10000", netPnl: "0" },
          { ts: "2026-09-18T12:01:00Z", equity: "10000", netPnl: "0" },
        ],
      },
      etag: "1",
    });
    vi.mocked(fetchJevDecisions).mockResolvedValue({
      status: "updated",
      data: [
        emptyDecision({
          id: "1",
          candidateKey: "single:binance:BTCUSDT:BTCUSDT",
          kind: "single",
          executionStatus: "fill_failed",
          enterProbability: "0.8",
          expectedNetReturn24h: "0.001",
          errorMessage: "missing fresh two-leg bbo",
        }),
        emptyDecision({
          id: "2",
          candidateKey: "cross:okx:ETHUSDT:binance:ETHUSDT",
          kind: "cross",
          enterProbability: "0.6",
          confidence: "0.5",
          expectedNetReturn24h: "0.004",
        }),
      ],
      etag: "1",
    });
    vi.mocked(fetchJevSnapshot).mockResolvedValue({
      status: "updated",
      data: snapshotFixture({
        modelStatus: "success",
        typesafeRequests: 2,
        typesafeSuccess: 2,
        entryCandidateCount: 2,
      }),
      etag: "1",
    });
    render(<JevArbDashboard />);
    expect(await screen.findByText("成交失败")).toBeTruthy();
    expect(screen.getByText("已成交")).toBeTruthy();
    expect(screen.getByText("单所 binance BTCUSDT")).toBeTruthy();
    expect(screen.getByText("跨所 okx ETHUSDT / binance ETHUSDT")).toBeTruthy();
    expect(screen.getByText("模型 2/2")).toBeTruthy();
    expect(screen.getByText("暂无权益变化")).toBeTruthy();
    const rows = screen.getAllByRole("row");
    const decisionRows = rows.filter((row) => row.textContent?.includes("ETHUSDT") || row.textContent?.includes("BTCUSDT"));
    expect(decisionRows[0]?.textContent).toContain("跨所");
    expect(decisionRows[1]?.textContent).toContain("单所");
  });

  it("renders unset new metrics as -- and does not show sample count 0", async () => {
    mockLists();
    vi.mocked(fetchJevDecisions).mockResolvedValue({
      status: "updated",
      data: [emptyDecision({ expectedNetReturn24h: null, basisSampleCount: null, currentMidBasisBps: null })],
      etag: "1",
    });
    vi.mocked(fetchJevSnapshot).mockResolvedValue({
      status: "updated",
      data: snapshotFixture({ entryCandidateCount: 1, modelStatus: "success" }),
      etag: "1",
    });
    render(<JevArbDashboard />);
    expect(await screen.findByText("样本")).toBeTruthy();
    const dashes = await screen.findAllByText("--");
    expect(dashes.length).toBeGreaterThan(0);
    expect(screen.queryByText("样本 0")).toBeNull();
  });
});

describe("decisionEmptyCopy", () => {
  it("covers the four empty states", () => {
    expect(decisionEmptyCopy(snapshotFixture({ entryCandidateCount: 0 }), [])).toBe(
      "本轮没有通过行情规则的候选",
    );
    expect(decisionEmptyCopy(snapshotFixture({ entryCandidateCount: 3, modelStatus: "not_called" }), [])).toBe(
      "发现候选，模型尚未调用",
    );
    expect(decisionEmptyCopy(snapshotFixture({ entryCandidateCount: 3, modelStatus: "unauthorized" }), [])).toBe(
      "模型调用失败",
    );
    expect(decisionEmptyCopy(snapshotFixture({ entryCandidateCount: 3, modelStatus: "success" }), [])).toBe(
      "模型已完成决策，暂无建仓信号",
    );
  });
});

describe("jev dashboard formatters", () => {
  it("orders decisions by time group then net return", () => {
    const ordered = orderJevDecisions([
      emptyDecision({
        id: "old-high",
        candidateKey: "single:binance:BTCUSDT:BTCUSDT",
        expectedNetReturn24h: "0.009",
        decisionAt: "2026-09-18T11:00:00Z",
      }),
      emptyDecision({
        id: "new-low",
        candidateKey: "single:okx:SOLUSDT:SOLUSDT",
        expectedNetReturn24h: "0.001",
        decisionAt: "2026-09-18T12:00:00Z",
      }),
      emptyDecision({
        id: "new-high",
        candidateKey: "cross:binance:ETHUSDT:okx:ETHUSDT",
        kind: "cross",
        expectedNetReturn24h: "0.008",
        decisionAt: "2026-09-18T12:00:00Z",
      }),
    ]);
    expect(ordered.map((item) => item.id)).toEqual(["new-high", "new-low", "old-high"]);
  });

  it("formats candidate, percent, annualized return, and execution status", () => {
    expect(
      formatCandidateLabel({ kind: "single", candidateKey: "single:binance:BTCUSDT:BTCUSDT" }),
    ).toBe("单所 binance BTCUSDT");
    expect(
      formatCandidateLabel({ kind: "cross", candidateKey: "cross:binance:ETHUSDT:okx:ETHUSDT" }),
    ).toBe("跨所 binance ETHUSDT / okx ETHUSDT");
    expect(formatPercentReturn("0.0008")).toBe("0.08%");
    expect(formatPercentReturn(null)).toBe("--");
    expect(formatAnnualizedReturn("0.0008")).toBe("29.2%");
    expect(formatAnnualizedReturn("")).toBe("--");
    expect(formatOptionalMetric(null)).toBe("--");
    expect(formatOptionalCount(null)).toBe("--");
    expect(formatOptionalCount(undefined)).toBe("--");
    expect(formatOptionalCount(0)).toBe("0");
    expect(formatOptionalCount(18)).toBe("18");
    expect(executionStatusLabel("pending")).toBe("待成交");
    expect(executionStatusLabel("filled")).toBe("已成交");
    expect(executionStatusLabel("expired")).toBe("已过期");
    expect(executionStatusLabel("fill_failed")).toBe("成交失败");
    expect(executionStatusLabel("portfolio_rejected")).toBe("仓位已满");
    expect(executionStatusLabel("not_applicable")).toBe("无需执行/继续持有");
  });
});
