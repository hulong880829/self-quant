// @vitest-environment jsdom

import * as React from "react";
import { Activity } from "react";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  fetchTradingAccounts: vi.fn(),
  fetchCachedTradingAccountSnapshot: vi.fn(),
  fetchLiveTradingAccountSnapshot: vi.fn(),
  inspectTradingReadiness: vi.fn(),
  fetchTraderInstruments: vi.fn(),
  fetchArbitrageCombinations: vi.fn(),
  fetchArbitrageCombination: vi.fn(),
  createArbitrageCombination: vi.fn(),
  updateArbitrageCombination: vi.fn(),
  closeArbitrageCombination: vi.fn(),
  fetchBasisSpreadHistory: vi.fn(),
  fetchFundingRates: vi.fn(),
  fetchFundingRatesLookup: vi.fn(),
}));

vi.mock("@/lib/api/accounts", async () => {
  const actual =
    await vi.importActual<typeof import("@/lib/api/accounts")>(
      "@/lib/api/accounts",
    );
  return {
    ...actual,
    fetchTradingAccounts: mocks.fetchTradingAccounts,
    fetchCachedTradingAccountSnapshot: mocks.fetchCachedTradingAccountSnapshot,
    fetchLiveTradingAccountSnapshot: mocks.fetchLiveTradingAccountSnapshot,
    inspectTradingReadiness: mocks.inspectTradingReadiness,
  };
});
vi.mock("@/lib/api/trader", async () => {
  const actual =
    await vi.importActual<typeof import("@/lib/api/trader")>(
      "@/lib/api/trader",
    );
  return { ...actual, fetchTraderInstruments: mocks.fetchTraderInstruments };
});
vi.mock("@/lib/api/arbitrage", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api/arbitrage")>(
    "@/lib/api/arbitrage",
  );
  return {
    ...actual,
    fetchArbitrageCombinations: mocks.fetchArbitrageCombinations,
    fetchArbitrageCombination: mocks.fetchArbitrageCombination,
    createArbitrageCombination: mocks.createArbitrageCombination,
    updateArbitrageCombination: mocks.updateArbitrageCombination,
    closeArbitrageCombination: mocks.closeArbitrageCombination,
  };
});
vi.mock("@/lib/api/spread", async () => {
  const actual =
    await vi.importActual<typeof import("@/lib/api/spread")>(
      "@/lib/api/spread",
    );
  return { ...actual, fetchBasisSpreadHistory: mocks.fetchBasisSpreadHistory };
});
vi.mock("@/lib/api/funding", async () => {
  const actual =
    await vi.importActual<typeof import("@/lib/api/funding")>(
      "@/lib/api/funding",
    );
  return {
    ...actual,
    fetchFundingRates: mocks.fetchFundingRates,
    fetchFundingRatesLookup: mocks.fetchFundingRatesLookup,
  };
});

import {
  annualizeBasisAvgBps,
  ArbitrageTradingView,
  combinationFundingAnnualized24h,
  crossExchangeWindowYield,
  fundingResolverFromItems,
  matchFundingOpportunity,
  resetLiveAccountFundsInflight,
  resolveArbitrageYieldMode,
  resolveBasisSpreadQuery,
  sortArbitrageCombinationsByCreatedAt,
} from "./arbitrage-trading";
import { fundingRequestKey } from "@/lib/api/funding";
import type { ArbitrageCombination } from "@/lib/api/arbitrage";
import { ArbitrageCreateFailure } from "@/lib/api/arbitrage";
import type { FundingOpportunity } from "@/types/market";

const accounts = [
  {
    id: 3,
    productName: "核心账户",
    exchange: "Binance",
    exchangeSlug: "binance",
    accountName: "Binance Main",
    hasPassphrase: false,
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
  },
  {
    id: 4,
    productName: "核心账户",
    exchange: "OKX",
    exchangeSlug: "okx",
    accountName: "OKX Main",
    hasPassphrase: true,
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
  },
  {
    id: 5,
    productName: "核心账户",
    exchange: "Binance",
    exchangeSlug: "binance",
    accountName: "Binance Sub",
    hasPassphrase: false,
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
  },
];

const instrument = {
  id: 7,
  exchange: "binance",
  contractType: "perpetual" as const,
  exchangeSymbol: "BTCUSDT",
  baseAsset: "BTC",
  quoteAsset: "USDT",
  settleAsset: "USDT",
  contractSize: "1",
  priceTick: "0.1",
  quantityStep: "0.001",
};

const spreadHistory = {
  venue: "okx",
  compareVenue: "binance",
  baseAsset: "BTC",
  quoteAsset: "USDT",
  canonicalSymbol: "BTCUSDT",
  range: "24h" as const,
  resolutionSeconds: 60,
  availability: "available" as const,
  asOf: "2026-08-22T10:01:00Z",
  points: [
    {
      ts: "2026-08-22T10:00:00Z",
      spreadBps: 10,
      spotAsk: 60_000,
      perpetualAsk: 60_060,
      samples: 2,
    },
    {
      ts: "2026-08-22T10:01:00Z",
      spreadBps: 12.5,
      spotAsk: 60_000,
      perpetualAsk: 60_075,
      samples: 2,
    },
  ],
  summary: {
    currentBps: 12.5,
    minBps: 10,
    maxBps: 12.5,
    avgBps: 11.25,
    coverage: 0.95,
  },
};

function fundingRate(
  exchange: FundingOpportunity["exchange"],
  exchangeSymbol: string,
  cumulative24h: number,
  cumulative7d: number,
  metrics: {
    positionNotional?: number;
    dailyVolume?: number;
    settlementIntervalHours?: number;
  } = {},
): FundingOpportunity {
  return {
    id: `${exchange}-${exchangeSymbol}`,
    exchange,
    exchangeSymbol,
    symbol: "BTCUSDT",
    baseAsset: "BTC",
    quoteAsset: "USDT",
    positionQuantity: 1,
    positionNotional: metrics.positionNotional ?? 1000,
    dailyVolume: metrics.dailyVolume ?? 1_000_000,
    annualizedRate: 10,
    currentFundingRate: 0.01,
    nextFundingRate: 0.01,
    settlementIntervalHours: metrics.settlementIntervalHours ?? 8,
    nextSettlementAt: "2026-08-22T16:00:00Z",
    cumulative24h,
    cumulative7d,
    latestPrice: 60_000,
    priceChange24h: 0.01,
    updatedAt: "2026-08-22T10:01:00Z",
    stale: false,
    fundingHistory: [],
    index: { name: "INDEX", value: 0, weight: 0 },
  };
}

function defaultFundingItems(): FundingOpportunity[] {
  return [
    fundingRate("Binance", "BTCUSDT", 0.02, 0.1, {
      positionNotional: 1_500_000,
      dailyVolume: 20_000_000,
      settlementIntervalHours: 8,
    }),
    fundingRate("OKX", "BTC-USDT-SWAP", 0.05, 0.21, {
      positionNotional: 900_000,
      dailyVolume: 30_000_000,
      settlementIntervalHours: 4,
    }),
  ];
}

function captureThirtySecondIntervals() {
  const callbacks: Array<() => void> = [];
  const native = window.setInterval.bind(window);
  const spy = vi.spyOn(window, "setInterval").mockImplementation(
    ((
      handler: TimerHandler,
      timeout?: number,
      ...args: unknown[]
    ) => {
      if (timeout === 30_000 && typeof handler === "function") {
        callbacks.push(() => {
          (handler as (...rest: unknown[]) => void)(...args);
        });
      }
      return native(handler, timeout, ...(args as []));
    }) as typeof window.setInterval,
  );
  return {
    flush() {
      for (const callback of [...callbacks]) {
        callback();
      }
    },
    restore() {
      spy.mockRestore();
    },
  };
}

function fundingLookupResponse(
  keys: Array<{ exchange: string; exchangeSymbol: string }>,
  items: FundingOpportunity[],
  snapshotVersion = "1",
) {
  const byKey = new Map(
    items.map((item) => [fundingRequestKey(item), item]),
  );
  return {
    snapshotVersion,
    serverTime: "2026-08-22T10:01:00Z",
    results: keys.map((key) => {
      const requestKey = fundingRequestKey(key);
      const item = byKey.get(requestKey);
      return item
        ? { key: requestKey, status: "hit" as const, item }
        : { key: requestKey, status: "missing" as const };
    }),
  };
}

const combination: ArbitrageCombination = {
  id: "arb-1",
  productName: "核心账户",
  status: "running",
  legA: {
    tradingAccountId: 3,
    accountName: "Binance Main",
    exchange: "binance",
    contractType: "perpetual" as const,
    instrumentId: 7,
    exchangeSymbol: "BTCUSDT",
    baseAsset: "BTC",
    quoteAsset: "USDT",
  },
  legB: {
    tradingAccountId: 4,
    accountName: "OKX Main",
    exchange: "okx",
    contractType: "perpetual" as const,
    instrumentId: 7,
    exchangeSymbol: "BTC-USDT-SWAP",
    baseAsset: "BTC",
    quoteAsset: "USDT",
  },
  askThresholdBps: "12",
  bidThresholdBps: "-8",
  targetNotional: "10000",
  positionNotional: "2500",
  cumulativeTurnoverNotional: "2500",
  grossTurnoverNotional: "5000",
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
  legABasePosition: "5",
  legBBasePosition: "-5",
  carryBaseQuantity: "0",
  legAAverageEntryPrice: "100",
  legBAverageEntryPrice: "101",
  averageEntrySpreadBps: "100",
  legAUnrealizedPnl: "2.5",
  legBUnrealizedPnl: "-1.5",
  realizedSpreadPnl: "0",
  estimatedFundingPnl: "0.5",
  combinedPositionAnnualized: "0.1842",
  fundingHistoryComplete: true,
  legAVenueBaselineBasePosition: "0",
  legBVenueBaselineBasePosition: "0",
  venueBaselineCapturedAt: "2026-08-22T09:59:00Z",
  legAExpectedBasePosition: "5",
  legBExpectedBasePosition: "-5",
  legAVenueBasePosition: "5",
  legBVenueBasePosition: "-5",
  legAVenueNotional: "500",
  legBVenueNotional: "-505",
  legAVenueValuationPrice: "100",
  legBVenueValuationPrice: "101",
  legAVenueValuationAt: "2026-08-22T10:01:00Z",
  legBVenueValuationAt: "2026-08-22T10:01:00Z",
  legAPositionDifference: "0",
  legBPositionDifference: "0",
  lastPositionReconciledAt: "2026-08-22T10:01:00Z",
  preferredLeg: "a",
  executionMode: "maker_then_hedge",
  askSpreadBps: "13.25",
  bidSpreadBps: "-6.80",
  marketDataStale: false,
  errorMessage: "",
  createdAt: "2026-08-22T10:00:00Z",
  updatedAt: "2026-08-22T10:01:00Z",
  closedAt: "",
};

async function waitForCreateReady() {
  await screen.findByText(/BTC\/USDT 配对有效/, {}, { timeout: 15000 });
  await waitFor(
    () =>
      expect(
        (
          screen.getByRole("button", {
            name: "创建套利组合",
          }) as HTMLButtonElement
        ).disabled,
      ).toBe(false),
    { timeout: 15000 },
  );
}

function selectRunMode(mode: "spread" | "one_shot") {
  fireEvent.change(screen.getByRole("combobox", { name: "运行模式" }), {
    target: { value: mode },
  });
}

function cachedSnapshot(
  id: number,
  availableFundsUsd = "1234.56",
  options: {
    stale?: boolean;
    sourceUpdatedAt?: string;
    serverTime?: string;
  } = {},
) {
  return {
    status: "ok" as const,
    snapshot: {
      tradingAccountId: id,
      productName: "核心账户",
      exchange: "binance",
      accountName: "main",
      accountEquityUsd: "2000",
      availableFundsUsd,
      riskPercent: "0",
      positions: [],
      sourceUpdatedAt: options.sourceUpdatedAt ?? "2026-01-01T00:00:00.000Z",
      serverTime: options.serverTime ?? "2026-01-01T00:01:00.000Z",
      stale: options.stale ?? false,
      lastError: "",
    },
  };
}

function setup() {
  mocks.fetchTradingAccounts.mockResolvedValue(accounts);
  mocks.fetchCachedTradingAccountSnapshot.mockImplementation(async (id: number) =>
    cachedSnapshot(id, id === 4 ? "80" : "1234.56"),
  );
  mocks.fetchLiveTradingAccountSnapshot.mockImplementation(async (id: number) => {
    throw new Error(`unexpected live snapshot for ${id}`);
  });
  mocks.inspectTradingReadiness.mockResolvedValue({
    tradingReady: true,
    tradingStatus: "ready",
  });
  mocks.fetchTraderInstruments.mockImplementation(
    async (accountID: number, contractType: "spot" | "perpetual") => {
      const binance = accountID === 3 || accountID === 5;
      return [
        {
          ...instrument,
          id:
            accountID === 3
              ? contractType === "spot"
                ? 6
                : 7
              : accountID === 5
                ? 9
                : 8,
          exchange: binance ? "binance" : "okx",
          contractType,
          exchangeSymbol: binance
            ? "BTCUSDT"
            : contractType === "spot"
              ? "BTC-USDT"
              : "BTC-USDT-SWAP",
        },
      ];
    },
  );
  mocks.fetchBasisSpreadHistory.mockImplementation(
    async (_venue, _base, _quote, range) => ({
      status: "updated",
      history: { ...spreadHistory, range },
      etag: `"spread-${range}"`,
    }),
  );
  mocks.fetchFundingRatesLookup.mockImplementation(async (keys) =>
    fundingLookupResponse(keys, defaultFundingItems()),
  );
  mocks.fetchArbitrageCombinations.mockImplementation(async (view: string) => ({
    items: view === "running" ? [combination] : [],
    total: view === "running" ? 1 : 0,
    nextCursor: "",
  }));
  mocks.fetchArbitrageCombination.mockResolvedValue({
    ...combination,
    orders: [],
    recentExecutions: [],
    recentEvents: [],
  });
  mocks.createArbitrageCombination.mockResolvedValue(combination);
  mocks.updateArbitrageCombination.mockImplementation(
    async (_id: string, input: Partial<ArbitrageCombination>) => ({
      ...combination,
      ...input,
    }),
  );
  mocks.closeArbitrageCombination.mockResolvedValue({
    ...combination,
    status: "closing",
    updatedAt: "2026-08-22T10:02:00Z",
  });
}

const sophCombination: ArbitrageCombination = {
  ...combination,
  id: "arb-soph",
  legA: {
    ...combination.legA,
    instrumentId: 7,
    exchangeSymbol: "SOPHUSDT",
    baseAsset: "SOPH",
  },
  legB: {
    ...combination.legB,
    instrumentId: 8,
    exchangeSymbol: "SOPH-USDT-SWAP",
    baseAsset: "SOPH",
  },
};

function makeListedInstrument(
  id: number,
  exchange: string,
  contractType: "spot" | "perpetual",
  base: "SOPH" | "VVV",
) {
  const binance = exchange === "binance";
  return {
    ...instrument,
    id,
    exchange,
    contractType,
    exchangeSymbol: binance
      ? `${base}USDT`
      : contractType === "spot"
        ? `${base}-USDT`
        : `${base}-USDT-SWAP`,
    baseAsset: base,
    quoteAsset: "USDT",
  };
}

function matchingSpreadHistory(
  venue: string,
  base: string,
  quote: string,
  range: string,
  compare?: string | null,
) {
  return {
    ...spreadHistory,
    venue,
    compareVenue: compare ?? "",
    baseAsset: base,
    quoteAsset: quote,
    canonicalSymbol: `${base}${quote}`,
    range,
  };
}

function setupSophChart(options?: { delaySophHistory?: boolean }) {
  setup();
  let releaseSophHistory: (() => void) | undefined;
  const sophHistoryGate = options?.delaySophHistory
    ? new Promise<void>((resolve) => {
        releaseSophHistory = resolve;
      })
    : null;
  mocks.fetchTradingAccounts.mockResolvedValue([
    ...accounts,
    {
      ...accounts[0],
      id: 13,
      productName: "VVV套利",
      accountName: "Binance VVV",
    },
    {
      ...accounts[1],
      id: 14,
      productName: "VVV套利",
      accountName: "OKX VVV",
    },
    {
      ...accounts[1],
      id: 15,
      productName: "核心账户",
      accountName: "OKX Sub",
    },
  ]);
  mocks.fetchTraderInstruments.mockImplementation(
    async (accountID: number, contractType: "spot" | "perpetual") => {
      if (accountID === 13) {
        return [makeListedInstrument(27, "binance", contractType, "VVV")];
      }
      if (accountID === 14) {
        return [makeListedInstrument(28, "okx", contractType, "VVV")];
      }
      if (accountID === 5) {
        return [
          makeListedInstrument(19, "binance", contractType, "VVV"),
          makeListedInstrument(9, "binance", contractType, "SOPH"),
        ];
      }
      if (accountID === 15) {
        return [
          makeListedInstrument(20, "okx", contractType, "VVV"),
          makeListedInstrument(21, "okx", contractType, "SOPH"),
        ];
      }
      const binance = accountID === 3;
      const exchange = binance ? "binance" : "okx";
      const sophId = binance
        ? contractType === "spot"
          ? 6
          : 7
        : contractType === "spot"
          ? 80
          : 8;
      const vvvId = binance
        ? contractType === "spot"
          ? 16
          : 17
        : contractType === "spot"
          ? 81
          : 18;
      if (contractType === "spot") {
        return [
          makeListedInstrument(vvvId, exchange, "spot", "VVV"),
          makeListedInstrument(sophId, exchange, "spot", "SOPH"),
        ];
      }
      return [
        makeListedInstrument(sophId, exchange, "perpetual", "SOPH"),
        makeListedInstrument(vvvId, exchange, "perpetual", "VVV"),
      ];
    },
  );
  mocks.fetchArbitrageCombinations.mockImplementation(async (view: string) => ({
    items: view === "running" ? [sophCombination] : [],
    total: view === "running" ? 1 : 0,
    nextCursor: "",
  }));
  mocks.fetchArbitrageCombination.mockResolvedValue({
    ...sophCombination,
    orders: [],
    recentExecutions: [],
    recentEvents: [],
  });
  mocks.fetchBasisSpreadHistory.mockImplementation(
    async (venue, base, quote, range, _etag, _signal, compare) => {
      if (base === "SOPH" && sophHistoryGate) {
        await sophHistoryGate;
      }
      return {
        status: "updated" as const,
        history: matchingSpreadHistory(venue, base, quote, range, compare),
        etag: `"${base}-${range}"`,
      };
    },
  );
  return {
    releaseSophHistory: () => releaseSophHistory?.(),
  };
}

function combinationRow(id: string) {
  const row = screen
    .getAllByText(id)
    .map((node) => node.closest("tr"))
    .find((tr) => tr?.className.includes("cursor-pointer"));
  if (!row) {
    throw new Error(`combination row ${id} not found`);
  }
  return row;
}

function formLeverageInputs() {
  return [...document.querySelectorAll("label")].flatMap((label) => {
    const caption = [...label.querySelectorAll("span")].find(
      (node) => node.textContent === "杠杆",
    );
    if (!caption) return [];
    const input = label.querySelector("input");
    return input ? [input] : [];
  });
}

function expectChartHighlight(id: string, selected: boolean) {
  if (selected) {
    expect(combinationRow(id).className).toContain("ring-primary/30");
    return;
  }
  expect(combinationRow(id).className).not.toContain("ring-primary/30");
}

async function selectSophChartRow() {
  expect(await screen.findByText(/SOPH\/USDT 配对有效/, {}, { timeout: 15000 })).toBeTruthy();
  fireEvent.click(combinationRow("arb-soph"));
  await waitFor(() => expectChartHighlight("arb-soph", true));
  expect(
    screen.getByText("OKX Ask / BINANCE Ask - 1 · SOPH/USDT"),
  ).toBeTruthy();
}

async function expectVvvChartAndHistory() {
  expectChartHighlight("arb-soph", false);
  expect(screen.queryByText(/SOPH\/USDT 配对有效/)).toBeNull();
  expect(await screen.findByText(/VVV\/USDT 配对有效/)).toBeTruthy();
  expect(
    screen.getByText("OKX Ask / BINANCE Ask - 1 · VVV/USDT"),
  ).toBeTruthy();
  await waitFor(() =>
    expect(mocks.fetchBasisSpreadHistory).toHaveBeenCalledWith(
      "okx",
      "VVV",
      "USDT",
      "24h",
      null,
      expect.any(AbortSignal),
      "binance",
      "VVVUSDT",
      "VVVUSDT",
    ),
  );
}

async function flushMicrotasks(times = 8) {
  for (let i = 0; i < times; i += 1) {
    await act(async () => {
      await Promise.resolve();
    });
  }
}

async function advanceTimers(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

function isLiveBidAskCell(
  node: Element | null,
  text = "-6.80 / +13.25 bps",
) {
  return node?.tagName === "TD" && node.textContent === text;
}

async function waitForCombinationList() {
  return screen.findByText((_, node) => isLiveBidAskCell(node as Element), {}, {
    timeout: 15000,
  });
}

describe("ArbitrageTradingView", () => {
  afterEach(() => {
    cleanup();
    resetLiveAccountFundsInflight();
    vi.resetAllMocks();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it("polls both views and expands detail directly below the selected row", async () => {
    setup();
    render(<ArbitrageTradingView />);
    expect(await waitForCombinationList()).toBeTruthy();
    await waitFor(() =>
      expect(mocks.fetchBasisSpreadHistory).toHaveBeenCalled(),
    );
    expect(
      await screen.findByRole("img", { name: "Best Ask 价差走势" }),
    ).toBeTruthy();
    expect(
      screen.getByText("OKX Ask / BINANCE Ask - 1 · BTC/USDT"),
    ).toBeTruthy();
    expect(screen.getByText("+12.50 bps")).toBeTruthy();
    expect(screen.getByText("+11.25 bps")).toBeTruthy();
    expect(screen.getByText("95.0%")).toBeTruthy();
    expect(await screen.findByText("24H 窗口年化")).toBeTruthy();
    expect(screen.getByText("7D 窗口年化")).toBeTruthy();
    expect(screen.getByText("+11.0%")).toBeTruthy();
    expect(screen.getByText("+5.7%")).toBeTruthy();
    expect(screen.getByText("$900.00K")).toBeTruthy();
    expect(screen.getByText("$20.00M")).toBeTruthy();
    expect(screen.getByText("较小腿 OI")).toBeTruthy();
    expect(screen.getByText("较小腿 24H 成交额")).toBeTruthy();
    expect(screen.getByText("A 8h / B 4h")).toBeTruthy();
    expect(screen.getByText("双腿取较小流动性值")).toBeTruthy();
    expect(
      screen
        .getByLabelText("图表流动性指标")
        .getAttribute("title"),
    ).toContain("A BINANCE BTCUSDT：OI $1.50M");
    expect(mocks.fetchFundingRatesLookup).toHaveBeenCalled();
    expect(mocks.fetchFundingRates).not.toHaveBeenCalled();
    expect(screen.queryByText("开仓 12 bps")).toBeNull();
    expect(screen.queryByText("平仓 -8 bps")).toBeNull();
    selectRunMode("spread");
    expect(screen.getByText("开仓 12 bps")).toBeTruthy();
    expect(screen.getByText("平仓 -8 bps")).toBeTruthy();
    expect(screen.getByText("+100.00 bps")).toBeTruthy();
    expect(screen.getByText("24H 年化")).toBeTruthy();
    expect(await screen.findByText("+10.95%")).toBeTruthy();
    expect(screen.getByText("已实现净年化")).toBeTruthy();
    expect(screen.getByText("+18.42%")).toBeTruthy();
    expect(screen.getByText("目标仓位/当前仓位")).toBeTruthy();
    expect(screen.queryByText("组合成交净仓")).toBeNull();
    expect(screen.getByText("arb-1").closest("tr")?.textContent).toContain(
      "10,000 USDT / 505 USDT",
    );
    expect(mocks.fetchBasisSpreadHistory).toHaveBeenCalledWith(
      "okx",
      "BTC",
      "USDT",
      "24h",
      null,
      expect.any(AbortSignal),
      "binance",
      "BTCUSDT",
      "BTCUSDT",
    );
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledWith(
      "running",
      expect.objectContaining({ limit: 50 }),
    );
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledWith(
      "closed",
      expect.objectContaining({ limit: 50 }),
    );

    fireEvent.click(screen.getByRole("button", { name: "详情" }));
    expect(await screen.findByText("买 Leg A / 卖 Leg B")).toBeTruthy();
    expect(screen.getByText("双腿累计成交额")).toBeTruthy();
    expect(
      screen.getByText(
        (_text, node) => node?.textContent === "+2.5000 / -1.5000 USDT",
      ),
    ).toBeTruthy();
    expect(mocks.fetchArbitrageCombination).toHaveBeenCalledWith("arb-1");
    fireEvent.click(screen.getByRole("button", { name: "收起" }));
    await waitFor(() =>
      expect(screen.queryByText("买 Leg A / 卖 Leg B")).toBeNull(),
    );
  }, 15000);

  it("keeps fixed columns, row height, and numeric widths across refreshes", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await waitForCombinationList();
    const table = screen.getByText("实时 Bid/Ask Spread").closest("table");
    expect(table?.className).toContain("table-fixed");
    const columns = Array.from(table?.querySelectorAll("col") ?? []).map(
      (column) => column.getAttribute("class"),
    );
    expect(columns).toEqual([
      "w-[180px]",
      "w-[110px]",
      "w-[180px]",
      "w-[200px]",
      "w-[130px]",
      "w-[120px]",
      "w-[160px]",
      "w-[150px]",
      "w-[260px]",
      "w-[150px]",
      "w-[120px]",
    ]);
    expect(table?.className).toContain("min-w-[1760px]");
    const headers = Array.from(table?.querySelectorAll("thead th") ?? []).map(
      (header) => header.textContent?.trim(),
    );
    expect(headers).toEqual([
      "交易所",
      "币对",
      "运行模式",
      "实时 Bid/Ask Spread",
      "持仓均价差",
      "24H 年化",
      "累计资金费收入",
      "已实现净年化",
      "目标仓位/当前仓位",
      "运行状态",
      "操作",
    ]);
    const dataRow = screen
      .getByText((_, node) => isLiveBidAskCell(node as Element))
      .closest("tr");
    expect(dataRow?.className).toContain("h-16");
    expect((dataRow?.children[3] as HTMLElement).className).toContain(
      "tabular-nums",
    );
    expect((dataRow?.children[6] as HTMLElement).className).toContain(
      "tabular-nums",
    );
    expect(dataRow?.children).toHaveLength(11);
    fireEvent.click(dataRow!);
    await screen.findByText("组合成交净仓 Leg A / Leg B");
    const expanded = table?.querySelector("td[colspan]");
    expect(expanded?.getAttribute("colspan")).toBe("11");
  });

  it("renders Bid / Ask in one live spread cell including missing and stale values", async () => {
    setup();
    const missingBid = {
      ...combination,
      id: "arb-missing",
      bidSpreadBps: null,
      askSpreadBps: "-3.10",
    };
    mocks.fetchArbitrageCombinations.mockImplementation(async (view: string) => ({
      items: view === "running" ? [missingBid] : [],
      total: view === "running" ? 1 : 0,
      nextCursor: "",
    }));
    render(<ArbitrageTradingView />);
    const missingCell = await screen.findByText(
      (_, node) => isLiveBidAskCell(node as Element, "— / -3.10 bps"),
    );
    expect(missingCell.className).toContain("tabular-nums");
    expect(missingCell.querySelector(".text-negative")?.textContent).toBe("—");
    expect(missingCell.querySelector(".text-positive")?.textContent).toBe(
      "-3.10",
    );
    expect(screen.queryByText("实时 Bid Spread")).toBeNull();
    expect(screen.queryByText("实时 Ask Spread")).toBeNull();
    cleanup();

    setup();
    mocks.fetchArbitrageCombinations.mockImplementation(async (view: string) => ({
      items:
        view === "running"
          ? [{ ...combination, marketDataStale: true, bidSpreadBps: null, askSpreadBps: null }]
          : [],
      total: view === "running" ? 1 : 0,
      nextCursor: "",
    }));
    render(<ArbitrageTradingView />);
    const staleCell = await screen.findByText(
      (_, node) => node?.tagName === "TD" && node.textContent === "行情过期",
    );
    expect(staleCell.textContent).toBe("行情过期");
    expect(staleCell.textContent).not.toContain("bps");
    expect(screen.getAllByText("行情过期")).toHaveLength(1);
  });

  it("shows baseline, ledger, expected, venue value, observed, and drift positions", async () => {
    setup();
    render(<ArbitrageTradingView />);
    fireEvent.click((await screen.findByText("arb-1")).closest("tr")!);

    const baseline = await screen.findByText("创建底仓 Leg A / Leg B");
    const local = screen.getByText("组合成交净仓 Leg A / Leg B");
    const expected = screen.getByText("预期交易所仓位 Leg A / Leg B");
    const venue = screen.getByText("交易所实际仓位 Leg A / Leg B");
    const drift = screen.getByText("仓位漂移 Leg A / Leg B");
    const execution = screen.getByText("执行方式");
    const venueValue = screen.getByText("交易所实际市值");
    expect(screen.queryByText("交易所实际市值 Leg A")).toBeNull();
    expect(screen.queryByText("交易所实际市值 Leg B")).toBeNull();
    expect(venueValue.parentElement?.textContent).toContain("USDT");
    expect(
      baseline.compareDocumentPosition(local) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(
      local.compareDocumentPosition(expected) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(
      expected.compareDocumentPosition(venue) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(
      venue.compareDocumentPosition(drift) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(
      drift.compareDocumentPosition(execution) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(
      execution.compareDocumentPosition(venueValue) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(screen.getAllByText("0 / 0 BTC")).toHaveLength(2);
    expect(screen.getAllByText("5 / -5 BTC")).toHaveLength(3);
    expect(screen.getByText("+500.00")).toBeTruthy();
    expect(screen.getByText("-505.00")).toBeTruthy();
    expect(screen.getByText(/@ 100 ·/)).toBeTruthy();
    expect(screen.getByText(/@ 101 ·/)).toBeTruthy();
    expect(screen.getAllByText("开仓阈值").length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("平仓阈值").length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("累计资金费收入")).toHaveLength(1);
    expect(screen.getByText("+0.5000 USDT")).toBeTruthy();
    const fundingHeader = screen.getByText("累计资金费收入");
    const annualizedHeader = screen.getByText("已实现净年化");
    expect(
      fundingHeader.compareDocumentPosition(annualizedHeader) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(screen.queryByText("底仓记录时间")).toBeNull();
    const detail = screen.getByText("组合成交净仓 Leg A / Leg B").closest("td");
    expect(detail?.textContent).not.toContain("累计资金费收入");
  }, 15000);

  it("labels combinations without a captured baseline as legacy audit", async () => {
    setup();
    mocks.fetchArbitrageCombination.mockResolvedValue({
      ...combination,
      legAVenueBaselineBasePosition: null,
      legBVenueBaselineBasePosition: null,
      venueBaselineCapturedAt: "",
      legAExpectedBasePosition: null,
      legBExpectedBasePosition: null,
      orders: [],
      recentExecutions: [],
      recentEvents: [],
    });
    render(<ArbitrageTradingView />);
    fireEvent.click((await screen.findByText("arb-1")).closest("tr")!);
    expect(
      await screen.findByText("未记录创建底仓（旧组合审计）"),
    ).toBeTruthy();
    expect(screen.getByText("交易所实际仓位 Leg A / Leg B")).toBeTruthy();
    expect(screen.getAllByText("累计资金费收入")).toHaveLength(1);
    expect(screen.getByText("+0.5000 USDT")).toBeTruthy();
    expect(screen.queryByText("旧组合不适用")).toBeNull();
    expect(screen.queryByText("底仓记录时间")).toBeNull();
  });

  it("edits one running parameter at a time with keyboard save and cancel", async () => {
    setup();
    render(<ArbitrageTradingView />);
    fireEvent.click((await screen.findByText("arb-1")).closest("tr")!);
    await screen.findByText("组合成交净仓 Leg A / Leg B");

    fireEvent.click(screen.getByRole("button", { name: "编辑目标仓位" }));
    const targetInput = screen.getByRole("textbox", { name: "编辑目标仓位" });
    fireEvent.change(targetInput, { target: { value: "12000" } });
    fireEvent.blur(targetInput);
    expect(mocks.updateArbitrageCombination).not.toHaveBeenCalled();
    fireEvent.keyDown(targetInput, { key: "Escape" });
    expect(screen.queryByRole("textbox", { name: "编辑目标仓位" })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "编辑开仓阈值" }));
    const askInput = screen.getByRole("textbox", { name: "编辑开仓阈值" });
    fireEvent.change(askInput, { target: { value: "-70" } });
    fireEvent.keyDown(askInput, { key: "Enter" });
    await waitFor(() =>
      expect(mocks.updateArbitrageCombination).toHaveBeenCalledWith("arb-1", {
        askThresholdBps: "-70",
      }),
    );
    await waitFor(() =>
      expect(
        screen.queryByRole("textbox", { name: "编辑开仓阈值" }),
      ).toBeNull(),
    );
    expect(screen.getByText("-70 bps")).toBeTruthy();
    expect(await screen.findByText("开仓 -70 bps")).toBeTruthy();
  }, 15000);

  it("hides strategy edits for one_shot combinations", async () => {
    setup();
    const oneShot: ArbitrageCombination = {
      ...combination,
      runMode: "one_shot",
      entryDirection: "ask",
      oneShotPhase: "waiting_exit",
      exitPolicy: "time",
      exitAfterSeconds: 3600,
      scheduledExitAt: "2026-08-22T11:00:00Z",
    };
    mocks.fetchArbitrageCombinations.mockImplementation(async (view: string) => ({
      items: view === "running" ? [oneShot] : [],
      total: view === "running" ? 1 : 0,
      nextCursor: "",
    }));
    mocks.fetchArbitrageCombination.mockResolvedValue({
      ...oneShot,
      orders: [],
      recentExecutions: [],
      recentEvents: [],
    });
    render(<ArbitrageTradingView />);
    expect((await screen.findAllByText("一次性建仓")).length).toBeGreaterThan(0);
    expect(screen.getByText("等待退出")).toBeTruthy();
    fireEvent.click(screen.getByText("arb-1").closest("tr")!);
    expect(await screen.findByText("预计退出时间")).toBeTruthy();
    expect(screen.getByText("持仓 1 小时")).toBeTruthy();
    expect(screen.getAllByText("一次性建仓").length).toBeGreaterThan(0);
    expect(screen.queryByText("入场方向")).toBeNull();
    expect(screen.queryByRole("button", { name: "编辑目标仓位" })).toBeNull();
    expect(screen.queryByRole("button", { name: "编辑开仓阈值" })).toBeNull();
    expect(screen.queryByText("开仓阈值")).toBeNull();
    expect(screen.getByText("未启用")).toBeTruthy();
  }, 15000);

  it.each([
    ["exiting", "退出平仓中"],
    ["exited", "已退出 · 待关闭"],
  ] as const)("shows one_shot %s in the running list", async (phase, label) => {
    setup();
    const oneShot: ArbitrageCombination = {
      ...combination,
      runMode: "one_shot",
      entryDirection: "ask",
      oneShotPhase: phase,
      exitPolicy: "time",
      exitAfterSeconds: 3600,
    };
    mocks.fetchArbitrageCombinations.mockImplementation(async (view: string) => ({
      items: view === "running" ? [oneShot] : [],
      total: view === "running" ? 1 : 0,
      nextCursor: "",
    }));
    mocks.fetchArbitrageCombination.mockResolvedValue({
      ...oneShot,
      orders: [],
      recentExecutions: [],
      recentEvents: [],
    });

    render(<ArbitrageTradingView />);
    expect((await screen.findAllByText(label)).length).toBeGreaterThan(0);
    expect(screen.getByRole("button", { name: "关闭" })).toBeTruthy();
    fireEvent.click(screen.getByText("arb-1").closest("tr")!);
    await waitFor(() =>
      expect(screen.getAllByText(label).length).toBeGreaterThan(1),
    );
  }, 15000);

  it("formats stored annualized exit rates as percents in detail", async () => {
    setup();
    const oneShot: ArbitrageCombination = {
      ...combination,
      runMode: "one_shot",
      entryDirection: "bid",
      oneShotPhase: "waiting_exit",
      exitPolicy: "annualized",
      exitAnnualizedRate: "0.15",
    };
    mocks.fetchArbitrageCombinations.mockImplementation(async (view: string) => ({
      items: view === "running" ? [oneShot] : [],
      total: view === "running" ? 1 : 0,
      nextCursor: "",
    }));
    mocks.fetchArbitrageCombination.mockResolvedValue({
      ...oneShot,
      orders: [],
      recentExecutions: [],
      recentEvents: [],
    });
    render(<ArbitrageTradingView />);
    fireEvent.click((await screen.findByText("arb-1")).closest("tr")!);
    expect(await screen.findByText("年化达到 ≥ 15.00%")).toBeTruthy();
    expect(screen.queryByText("入场方向")).toBeNull();
    expect(screen.queryByText("BID")).toBeNull();
  }, 15000);

  it("creates one_shot combinations with a 7-day hold and shows the label", async () => {
    setup();
    const oneShot: ArbitrageCombination = {
      ...combination,
      runMode: "one_shot",
      entryDirection: "ask",
      oneShotPhase: "waiting_exit",
      exitPolicy: "time",
      exitAfterSeconds: 604800,
    };
    mocks.fetchArbitrageCombinations.mockImplementation(async (view: string) => ({
      items: view === "running" ? [oneShot] : [],
      total: view === "running" ? 1 : 0,
      nextCursor: "",
    }));
    mocks.fetchArbitrageCombination.mockResolvedValue({
      ...oneShot,
      orders: [],
      recentExecutions: [],
      recentEvents: [],
    });
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    fireEvent.change(screen.getByRole("combobox", { name: "运行模式" }), {
      target: { value: "one_shot" },
    });
    fireEvent.change(screen.getByRole("combobox", { name: "退出方式" }), {
      target: { value: "time" },
    });
    expect(screen.getByText("7 天")).toBeTruthy();
    fireEvent.change(screen.getByRole("combobox", { name: "持仓时间" }), {
      target: { value: "604800" },
    });
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    await waitFor(() =>
      expect(mocks.createArbitrageCombination).toHaveBeenCalledWith(
        expect.objectContaining({
          runMode: "one_shot",
          entryDirection: "ask",
          exitPolicy: "time",
          exitAfterSeconds: 604800,
        }),
      ),
    );
    expect((await screen.findAllByText("一次性建仓")).length).toBeGreaterThan(0);
    fireEvent.click((await screen.findByText("arb-1")).closest("tr")!);
    expect(await screen.findByText("持仓 7 天")).toBeTruthy();
    expect(screen.queryByText("持仓 604800 秒")).toBeNull();
  }, 15000);

  it("keeps failed parameter edits open", async () => {
    setup();
    mocks.updateArbitrageCombination.mockRejectedValueOnce(
      new Error(
        "target notional cannot be below current absolute position 2500",
      ),
    );
    render(<ArbitrageTradingView />);
    fireEvent.click((await screen.findByText("arb-1")).closest("tr")!);
    await screen.findByText("组合成交净仓 Leg A / Leg B");
    fireEvent.click(screen.getByRole("button", { name: "编辑目标仓位" }));
    const input = screen.getByRole("textbox", { name: "编辑目标仓位" });
    fireEvent.change(input, { target: { value: "1000" } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(
      await screen.findByText(/target notional cannot be below/),
    ).toBeTruthy();
    expect(screen.getByRole("textbox", { name: "编辑目标仓位" })).toBeTruthy();
  });

  it("keeps closed combination parameters read-only", async () => {
    setup();
    const closed = { ...combination, status: "closed" as const };
    mocks.fetchArbitrageCombinations.mockImplementation(
      async (view: string) => ({
        items: view === "closed" ? [closed] : [],
        total: view === "closed" ? 1 : 0,
        nextCursor: "",
      }),
    );
    mocks.fetchArbitrageCombination.mockResolvedValue({
      ...closed,
      orders: [],
      recentExecutions: [],
      recentEvents: [],
    });
    render(<ArbitrageTradingView />);
    fireEvent.click(await screen.findByRole("button", { name: "已关闭 1" }));
    fireEvent.click((await screen.findByText("arb-1")).closest("tr")!);
    await screen.findByText("组合成交净仓 Leg A / Leg B");
    expect(screen.queryByRole("button", { name: "编辑目标仓位" })).toBeNull();
    expect(screen.queryByRole("button", { name: "编辑开仓阈值" })).toBeNull();
  });

  it("keeps closing combination parameters read-only", async () => {
    setup();
    const closing = { ...combination, status: "closing" as const };
    mocks.fetchArbitrageCombinations.mockImplementation(
      async (view: string) => ({
        items: view === "running" ? [closing] : [],
        total: view === "running" ? 1 : 0,
        nextCursor: "",
      }),
    );
    mocks.fetchArbitrageCombination.mockResolvedValue({
      ...closing,
      orders: [],
      recentExecutions: [],
      recentEvents: [],
    });
    render(<ArbitrageTradingView />);
    fireEvent.click((await screen.findByText("arb-1")).closest("tr")!);
    await screen.findByText("组合成交净仓 Leg A / Leg B");
    expect(screen.queryByRole("button", { name: "编辑目标仓位" })).toBeNull();
    expect(screen.queryByRole("button", { name: "编辑开仓阈值" })).toBeNull();
  });

  it("shows dust positions by direction while risk states stay dominant", async () => {
    setup();
    mocks.fetchArbitrageCombinations.mockImplementation(
      async (view: string) => ({
        items:
          view === "running"
            ? [{ ...combination, runtimeState: "hedge_deferred_dust" as const }]
            : [],
        total: view === "running" ? 1 : 0,
        nextCursor: "",
      }),
    );
    render(<ArbitrageTradingView />);
    expect(await screen.findByText("持有可平仓位")).toBeTruthy();
    cleanup();

    setup();
    mocks.fetchArbitrageCombinations.mockImplementation(
      async (view: string) => ({
        items:
          view === "running"
            ? [
                {
                  ...combination,
                  runtimeState: "hedge_deferred_dust" as const,
                  positionUncertain: true,
                  errorMessage:
                    "order state remained uncertain after bounded reconciliation",
                },
              ]
            : [],
        total: view === "running" ? 1 : 0,
        nextCursor: "",
      }),
    );
    render(<ArbitrageTradingView />);
    expect(await screen.findByText("订单状态待核对")).toBeTruthy();
    cleanup();

    setup();
    mocks.fetchArbitrageCombinations.mockImplementation(
      async (view: string) => ({
        items:
          view === "running"
            ? [
                {
                  ...combination,
                  runtimeState: "position_uncertain" as const,
                  positionUncertain: true,
                  errorMessage:
                    "account position differs from combination ledger: leg_a_difference=1",
                },
              ]
            : [],
        total: view === "running" ? 1 : 0,
        nextCursor: "",
      }),
    );
    render(<ArbitrageTradingView />);
    expect(await screen.findByText("仓位待核对")).toBeTruthy();
  });

  it("labels baseline-only, partial baseline close, and flat-to-zero without inventing reverse exposure", async () => {
    setup();
    const baselineOnly: ArbitrageCombination = {
      ...combination,
      id: "arb-baseline-only",
      positionNotional: "0",
      legABasePosition: "0",
      legBBasePosition: "0",
      legAVenueBaselineBasePosition: "2",
      legBVenueBaselineBasePosition: "-2",
      legAExpectedBasePosition: "2",
      legBExpectedBasePosition: "-2",
      legAVenueBasePosition: "2",
      legBVenueBasePosition: "-2",
      legAVenueNotional: "200",
      legBVenueNotional: "-202",
    };
    const partialBaselineClose: ArbitrageCombination = {
      ...baselineOnly,
      id: "arb-baseline-partial",
      positionNotional: "-100",
      legABasePosition: "-1",
      legBBasePosition: "1",
      legAExpectedBasePosition: "1",
      legBExpectedBasePosition: "-1",
      legAVenueBasePosition: "1",
      legBVenueBasePosition: "-1",
      legAVenueNotional: "100",
      legBVenueNotional: "-101",
      legAUnrealizedPnl: "0",
      legBUnrealizedPnl: "0",
    };
    const fullyClosedBaseline: ArbitrageCombination = {
      ...baselineOnly,
      id: "arb-baseline-flat",
      positionNotional: "-200",
      legABasePosition: "-2",
      legBBasePosition: "2",
      legAExpectedBasePosition: "0",
      legBExpectedBasePosition: "0",
      legAVenueBasePosition: "0",
      legBVenueBasePosition: "0",
      legAVenueNotional: "0",
      legBVenueNotional: "0",
      legAUnrealizedPnl: "0",
      legBUnrealizedPnl: "0",
    };
    const rows = [
      baselineOnly,
      partialBaselineClose,
      fullyClosedBaseline,
    ];
    mocks.fetchArbitrageCombinations.mockImplementation(
      async (view: string) => ({
        items: view === "running" ? rows : [],
        total: view === "running" ? rows.length : 0,
        nextCursor: "",
      }),
    );
    mocks.fetchArbitrageCombination.mockImplementation(async (id: string) => ({
      ...rows.find((item) => item.id === id)!,
      orders: [],
      recentExecutions: [],
      recentEvents: [],
    }));

    render(<ArbitrageTradingView />);
    const baselineRow = (await screen.findByText("arb-baseline-only", {}, { timeout: 15000 })).closest(
      "tr",
    );
    const partialRow = screen.getByText("arb-baseline-partial").closest("tr");
    const flatRow = screen.getByText("arb-baseline-flat").closest("tr");

    expect(baselineRow?.textContent).toContain("持有可平仓位");
    expect(partialRow?.textContent).toContain("底仓减仓中");
    expect(partialRow?.textContent).toContain("101 USDT");
    expect(flatRow?.textContent).toContain("已平至零");
    expect(screen.queryByText("旧反向仓位")).toBeNull();
    expect(screen.queryByText("持有反套")).toBeNull();

    fireEvent.click(partialRow!);
    expect(await screen.findByText("预期交易所仓位 Leg A / Leg B")).toBeTruthy();
    expect(screen.getAllByText("1 / -1 BTC")).toHaveLength(2);
    expect(screen.getByText("交易所实际仓位 Leg A / Leg B")).toBeTruthy();
    expect(screen.getByText("最后仓位对账")).toBeTruthy();
    expect(
      screen.getByText(
        (_text, node) => node?.textContent === "0.0000 / 0.0000 USDT",
      ),
    ).toBeTruthy();
  }, 15000);

  it("keeps combinations ordered by creation time and id", async () => {
    setup();
    mocks.fetchArbitrageCombinations.mockImplementation(
      async (view: string) => ({
        items:
          view === "running"
            ? [
                {
                  ...combination,
                  id: "arb-late",
                  createdAt: "2026-08-22T12:00:00Z",
                },
                {
                  ...combination,
                  id: "arb-b",
                  createdAt: "2026-08-22T11:00:00Z",
                },
                {
                  ...combination,
                  id: "arb-early",
                  createdAt: "2026-08-22T09:00:00Z",
                },
                {
                  ...combination,
                  id: "arb-a",
                  createdAt: "2026-08-22T11:00:00Z",
                },
              ]
            : [],
        total: view === "running" ? 4 : 0,
        nextCursor: "",
      }),
    );
    render(<ArbitrageTradingView />);
    await screen.findByText("arb-early");
    expect(
      screen
        .getAllByText(/^arb-(early|a|b|late)$/)
        .map((node) => node.textContent),
    ).toEqual(["arb-early", "arb-a", "arb-b", "arb-late"]);
  });

  it("keeps the clicked combination selected for the spread chart", async () => {
    setup();
    const ethCombination: ArbitrageCombination = {
      ...combination,
      id: "arb-eth",
      legA: {
        ...combination.legA,
        exchangeSymbol: "ETHUSDT",
        baseAsset: "ETH",
      },
      legB: {
        ...combination.legB,
        exchange: "gate",
        exchangeSymbol: "ETH_USDT",
        baseAsset: "ETH",
      },
      createdAt: "2026-08-22T11:00:00Z",
    };
    mocks.fetchArbitrageCombinations.mockImplementation(
      async (view: string) => ({
        items: view === "running" ? [combination, ethCombination] : [],
        total: view === "running" ? 2 : 0,
        nextCursor: "",
      }),
    );
    render(<ArbitrageTradingView />);
    const ethRow = (await screen.findByText("arb-eth")).closest("tr");
    expect(ethRow).toBeTruthy();
    fireEvent.click(ethRow!);
    expect(
      await screen.findByText("GATE Ask / BINANCE Ask - 1 · ETH/USDT"),
    ).toBeTruthy();
    await waitFor(() =>
      expect(mocks.fetchBasisSpreadHistory).toHaveBeenCalledWith(
        "gate",
        "ETH",
        "USDT",
        "24h",
        null,
        expect.any(AbortSignal),
        "binance",
        "ETHUSDT",
        "ETHUSDT",
      ),
    );
    fireEvent.click(ethRow!);
    expect(
      screen.getByText("GATE Ask / BINANCE Ask - 1 · ETH/USDT"),
    ).toBeTruthy();
  }, 15000);

  it("clears the selected chart when both symbols change to VVV and ignores late SOPH history", async () => {
    const { releaseSophHistory } = setupSophChart({ delaySophHistory: true });
    render(<ArbitrageTradingView />);
    await selectSophChartRow();
    const symbols = screen.getAllByRole("combobox", { name: "Symbol" });
    fireEvent.change(symbols[0]!, { target: { value: "17" } });
    fireEvent.change(symbols[1]!, { target: { value: "18" } });
    await expectVvvChartAndHistory();
    await act(async () => {
      releaseSophHistory();
      await Promise.resolve();
    });
    expect(
      screen.getByText("OKX Ask / BINANCE Ask - 1 · VVV/USDT"),
    ).toBeTruthy();
    expect(screen.queryByText(/SOPH\/USDT 配对有效/)).toBeNull();
    expect(
      screen.queryByText("OKX Ask / BINANCE Ask - 1 · SOPH/USDT"),
    ).toBeNull();
  }, 15000);

  it("clears the selected chart when the product changes to a VVV pair", async () => {
    setupSophChart();
    render(<ArbitrageTradingView />);
    await selectSophChartRow();
    fireEvent.change(screen.getByRole("combobox", { name: "产品" }), {
      target: { value: "VVV套利" },
    });
    await expectVvvChartAndHistory();
  }, 15000);

  it("clears the selected chart when either exchange changes", async () => {
    setupSophChart();
    render(<ArbitrageTradingView />);
    await selectSophChartRow();
    fireEvent.change(screen.getAllByRole("combobox", { name: "交易所" })[0]!, {
      target: { value: "okx" },
    });
    expectChartHighlight("arb-soph", false);
    expect(screen.queryByText(/SOPH\/USDT 配对有效/)).toBeNull();
    expect(
      screen.queryByText("OKX Ask / BINANCE Ask - 1 · SOPH/USDT"),
    ).toBeNull();

    cleanup();
    setupSophChart();
    render(<ArbitrageTradingView />);
    await selectSophChartRow();
    fireEvent.change(screen.getAllByRole("combobox", { name: "交易所" })[1]!, {
      target: { value: "binance" },
    });
    expectChartHighlight("arb-soph", false);
    expect(screen.queryByText(/SOPH\/USDT 配对有效/)).toBeNull();
    expect(
      screen.queryByText("OKX Ask / BINANCE Ask - 1 · SOPH/USDT"),
    ).toBeNull();
  }, 15000);

  it("clears the selected chart when either account changes", async () => {
    setupSophChart();
    render(<ArbitrageTradingView />);
    await selectSophChartRow();
    fireEvent.change(screen.getAllByRole("combobox", { name: "账户" })[0]!, {
      target: { value: "5" },
    });
    expectChartHighlight("arb-soph", false);
    expect(screen.queryByText(/SOPH\/USDT 配对有效/)).toBeNull();

    cleanup();
    setupSophChart();
    render(<ArbitrageTradingView />);
    await selectSophChartRow();
    fireEvent.change(screen.getAllByRole("combobox", { name: "账户" })[1]!, {
      target: { value: "15" },
    });
    expectChartHighlight("arb-soph", false);
    expect(screen.queryByText(/SOPH\/USDT 配对有效/)).toBeNull();
  }, 15000);

  it("clears the selected chart when either contract type changes", async () => {
    setupSophChart();
    render(<ArbitrageTradingView />);
    await selectSophChartRow();
    fireEvent.change(
      screen.getAllByRole("combobox", { name: "产品类型" })[0]!,
      { target: { value: "spot" } },
    );
    expectChartHighlight("arb-soph", false);
    expect(screen.queryByText(/SOPH\/USDT 配对有效/)).toBeNull();

    cleanup();
    setupSophChart();
    render(<ArbitrageTradingView />);
    await selectSophChartRow();
    fireEvent.change(
      screen.getAllByRole("combobox", { name: "产品类型" })[1]!,
      { target: { value: "spot" } },
    );
    expectChartHighlight("arb-soph", false);
    expect(screen.queryByText(/SOPH\/USDT 配对有效/)).toBeNull();
  }, 15000);

  it("clears the selected chart when a new prefill signature is applied", async () => {
    setupSophChart();
    const view = render(<ArbitrageTradingView />);
    await selectSophChartRow();
    view.rerender(
      <ArbitrageTradingView
        searchParams={
          new URLSearchParams(
            "mode=arbitrage&legAExchange=binance&legAContract=perpetual&legABase=VVV&legAQuote=USDT&legAExchangeSymbol=VVVUSDT&legBExchange=okx&legBContract=perpetual&legBBase=VVV&legBQuote=USDT&legBExchangeSymbol=VVV-USDT-SWAP",
          )
        }
      />,
    );
    await expectVvvChartAndHistory();
  }, 15000);

  it("keeps the selected chart when leverage changes", async () => {
    setupSophChart();
    render(<ArbitrageTradingView />);
    await selectSophChartRow();
    fireEvent.change(formLeverageInputs()[0]!, {
      target: { value: "8" },
    });
    expectChartHighlight("arb-soph", true);
    expect(screen.getByText(/SOPH\/USDT 配对有效/)).toBeTruthy();
    expect(
      screen.getByText("OKX Ask / BINANCE Ask - 1 · SOPH/USDT"),
    ).toBeTruthy();
  }, 15000);

  it("shows polling failures as temporary toasts", async () => {
    setup();
    mocks.fetchArbitrageCombinations.mockRejectedValue(
      new Error("trader service unavailable"),
    );
    render(<ArbitrageTradingView />);
    expect((await screen.findByRole("alert")).textContent).toContain(
      "trader service unavailable",
    );
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull(), {
      timeout: 1300,
    });
    expect(
      (await screen.findByRole("alert", {}, { timeout: 4000 })).textContent,
    ).toContain("trader service unavailable");
  }, 10000);

  it("does not toast when combination polling is aborted", async () => {
    setup();
    const aborted = new Error("signal is aborted without reason");
    aborted.name = "AbortError";
    mocks.fetchArbitrageCombinations.mockRejectedValue(aborted);
    render(<ArbitrageTradingView />);
    await waitFor(() =>
      expect(mocks.fetchArbitrageCombinations).toHaveBeenCalled(),
    );
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("does not start the next combination poll until the current one finishes", async () => {
    setup();
    mocks.fetchArbitrageCombinations.mockImplementation(
      async (
        _view: string,
        options: { signal?: AbortSignal } = {},
      ) => {
        await new Promise<void>((_resolve, reject) => {
          const fail = () => {
            const aborted = new Error("Aborted");
            aborted.name = "AbortError";
            reject(aborted);
          };
          if (options.signal?.aborted) {
            fail();
            return;
          }
          options.signal?.addEventListener("abort", fail, { once: true });
        });
        return { items: [], total: 0, nextCursor: "" };
      },
    );
    render(<ArbitrageTradingView />);
    await waitFor(() =>
      expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(2),
    );
    await new Promise((resolve) => setTimeout(resolve, 2_000));
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(2);
  }, 8000);

  it("does not let an older toast timer clear a newer error", async () => {
    setup();
    mocks.createArbitrageCombination
      .mockRejectedValueOnce(new Error("first create failure"))
      .mockRejectedValueOnce(new Error("second create failure"));
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    const createButton = screen.getByRole("button", { name: "创建套利组合" });
    await waitFor(() =>
      expect(mocks.fetchFundingRatesLookup).toHaveBeenCalled(),
    );

    fireEvent.click(createButton);
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toContain(
        "first create failure",
      ),
    );
    await new Promise((resolve) => window.setTimeout(resolve, 500));

    fireEvent.click(createButton);
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toContain(
        "second create failure",
      ),
    );
    await new Promise((resolve) => window.setTimeout(resolve, 600));
    expect(screen.getByRole("alert").textContent).toContain(
      "second create failure",
    );
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull(), {
      timeout: 600,
    });
  }, 15000);

  it("requires confirmation before closing", async () => {
    setup();
    const confirm = vi.fn(() => false);
    vi.stubGlobal("confirm", confirm);
    render(<ArbitrageTradingView />);
    await waitForCombinationList();
    fireEvent.click(screen.getByRole("button", { name: "关闭" }));
    expect(confirm).toHaveBeenCalledWith(
      "确认关闭此套利组合？系统将立即停止新交易、撤销活动挂单，并开始分批平掉该组合产生的已配对仓位。全部可执行配对仓位平完后，组合才会关闭；未配平、无法成交或状态无法确认的敞口可能需要人工处理。",
    );
    expect(mocks.closeArbitrageCombination).not.toHaveBeenCalled();

    confirm.mockReturnValue(true);
    fireEvent.click(screen.getByRole("button", { name: "关闭" }));
    await waitFor(() =>
      expect(mocks.closeArbitrageCombination).toHaveBeenCalledWith("arb-1"),
    );
  }, 15000);

  it("shows runtime state and original venue rejection details", async () => {
    setup();
    const failed = {
      ...combination,
      runtimeState: "backoff" as const,
      errorMessage: "venue rejected order",
    };
    mocks.fetchArbitrageCombinations.mockImplementation(
      async (view: string) => ({
        items: view === "running" ? [failed] : [],
        total: view === "running" ? 1 : 0,
        nextCursor: "",
      }),
    );
    mocks.fetchArbitrageCombination.mockResolvedValue({
      ...failed,
      orders: [
        {
          id: "order-1",
          idempotencyKey: "order-key",
          tradingAccountId: 4,
          productName: "核心账户",
          exchange: "okx",
          instrumentId: 8,
          contractType: "perpetual",
          exchangeSymbol: "BTC-USDT-SWAP",
          baseAsset: "BTC",
          quoteAsset: "USDT",
          clientOrderId: "sq-order-1",
          venueOrderId: "",
          side: "sell",
          orderType: "limit",
          quantity: "1",
          price: "100",
          filledQuantity: "0",
          averagePrice: "0",
          status: "rejected",
          errorCode: "51008",
          errorMessage: "Insufficient balance",
          createdAt: "2026-08-22T10:00:00Z",
          updatedAt: "2026-08-22T10:00:01Z",
          lastReconciledAt: "",
          syncState: "terminal",
          twapSliceIndex: 0,
          twapAttemptIndex: 0,
          arbitrageExecutionId: "execution-1",
          arbitrageLeg: "b",
          arbitrageRole: "hedge",
        },
        {
          id: "order-benign",
          idempotencyKey: "order-benign-key",
          tradingAccountId: 3,
          productName: "核心账户",
          exchange: "bybit",
          instrumentId: 7,
          contractType: "perpetual",
          exchangeSymbol: "BTCUSDT",
          baseAsset: "BTC",
          quoteAsset: "USDT",
          clientOrderId: "sq-order-benign",
          venueOrderId: "venue-benign",
          side: "buy",
          orderType: "limit",
          quantity: "1",
          price: "100",
          filledQuantity: "0",
          averagePrice: "0",
          status: "canceled",
          errorCode: "EC_NoImmediateQtyToFill",
          errorMessage: "",
          createdAt: "2026-08-22T10:00:00Z",
          updatedAt: "2026-08-22T10:00:01Z",
          lastReconciledAt: "",
          syncState: "terminal",
          twapSliceIndex: 0,
          twapAttemptIndex: 0,
          arbitrageExecutionId: "execution-1",
          arbitrageLeg: "a",
          arbitrageRole: "maker",
        },
      ],
      recentExecutions: [],
      recentEvents: [],
    });
    render(<ArbitrageTradingView />);
    expect(await screen.findByText("退避重试")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "详情" }));
    expect(
      await screen.findByText(/okx · 51008 · Insufficient balance/),
    ).toBeTruthy();
    expect(screen.getByText("最近订单")).toBeTruthy();
    expect(screen.getByText("委托量")).toBeTruthy();
    expect(screen.getByText("成交量")).toBeTruthy();
    expect(screen.getByText("51008 · Insufficient balance")).toBeTruthy();
    const benignRow = screen.getByText("bybit").closest("tr");
    expect(benignRow?.textContent).toContain("未即时成交（已取消）");
    expect(benignRow?.textContent).not.toContain("EC_NoImmediateQtyToFill");
    expect(
      benignRow?.querySelector("td:last-child")?.className,
    ).not.toContain("text-destructive");
  }, 15000);

  it("shows bounded close retry and closed uncertain feedback", async () => {
    setup();
    const closing = {
      ...combination,
      status: "closing" as const,
      runtimeState: "closing" as const,
      errorMessage: "close order state uncertain",
      nextRetryAt: "2026-08-22T10:02:00Z",
    };
    mocks.fetchArbitrageCombinations.mockImplementation(
      async (view: string) => ({
        items: view === "running" ? [closing] : [],
        total: view === "running" ? 1 : 0,
        nextCursor: "",
      }),
    );
    mocks.fetchArbitrageCombination.mockResolvedValue({
      ...closing,
      orders: [],
      recentExecutions: [],
      recentEvents: [],
    });
    const { unmount } = render(<ArbitrageTradingView />);
    fireEvent.click(await screen.findByRole("button", { name: "详情" }));
    expect(
      await screen.findByText("关闭未完成，系统将自动重试"),
    ).toBeTruthy();
    expect(screen.getByText(/下次重试：/)).toBeTruthy();
    expect(screen.getByText("close order state uncertain")).toBeTruthy();
    expect(
      screen.queryByText("撤单确认失败，系统将自动重试"),
    ).toBeNull();

    unmount();
    const closingCapacity = {
      ...closing,
      errorMessage: "Aster code -5018: ReduceOnly Order is rejected.",
    };
    mocks.fetchArbitrageCombinations.mockImplementation(
      async (view: string) => ({
        items: view === "running" ? [closingCapacity] : [],
        total: view === "running" ? 1 : 0,
        nextCursor: "",
      }),
    );
    mocks.fetchArbitrageCombination.mockResolvedValue({
      ...closingCapacity,
      orders: [],
      recentExecutions: [],
      recentEvents: [],
    });
    const capacityView = render(<ArbitrageTradingView />);
    fireEvent.click(await screen.findByRole("button", { name: "详情" }));
    expect(
      await screen.findByText("关闭未完成，系统将自动重试"),
    ).toBeTruthy();
    expect(screen.getByText(/下次重试：/)).toBeTruthy();
    expect(
      screen.getByText("Aster code -5018: ReduceOnly Order is rejected."),
    ).toBeTruthy();
    expect(
      screen.queryByText("撤单确认失败，系统将自动重试"),
    ).toBeNull();

    capacityView.unmount();
    const closed = {
      ...closing,
      status: "closed" as const,
      runtimeState: "position_uncertain" as const,
      positionUncertain: true,
    };
    mocks.fetchArbitrageCombinations.mockImplementation(
      async (view: string) => ({
        items: view === "closed" ? [closed] : [],
        total: view === "closed" ? 1 : 0,
        nextCursor: "",
      }),
    );
    mocks.fetchArbitrageCombination.mockResolvedValue({
      ...closed,
      orders: [],
      recentExecutions: [],
      recentEvents: [],
    });
    render(<ArbitrageTradingView />);
    fireEvent.click(await screen.findByRole("button", { name: "已关闭 1" }));
    fireEvent.click(await screen.findByRole("button", { name: "详情" }));
    expect(
      await screen.findByText("已关闭（订单状态待核对）"),
    ).toBeTruthy();
  }, 15000);

  it("keeps DEX accounts visible but disabled until readiness passes", async () => {
    setup();
    let resolveReadiness!: (value: Record<string, unknown>) => void;
    mocks.inspectTradingReadiness.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveReadiness = resolve;
        }),
    );
    mocks.fetchTradingAccounts.mockResolvedValue([
      ...accounts,
      {
        ...accounts[0],
        id: 11,
        exchange: "Hyperliquid",
        exchangeSlug: "hyperliquid",
        accountName: "Hyperliquid Wallet",
        tradingReady: true,
        tradingStatus: "ready",
      },
    ]);

    render(<ArbitrageTradingView />);
    const dexOption = (await screen.findAllByRole("option", {
      name: "Hyperliquid Wallet（交易能力检查中）",
    }))[0] as HTMLOptionElement;
    expect(dexOption.disabled).toBe(true);
    fireEvent.change(screen.getAllByRole("combobox", { name: "交易所" })[0]!, {
      target: { value: "hyperliquid" },
    });
    expect((await screen.findAllByText("交易能力检查中")).length).toBeGreaterThan(
      0,
    );
    const productTypes = screen.getAllByRole("combobox", { name: "产品类型" });
    expect((productTypes[0] as HTMLSelectElement).disabled).toBe(true);
    expect(
      (
        screen.getByRole("button", { name: "创建套利组合" }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);

    resolveReadiness({
      tradingReady: false,
      tradingStatus: "wallet_unauthorized",
      tradingUnavailableReason: "代理钱包未授权",
    });
    expect((await screen.findAllByText("代理钱包未授权")).length).toBeGreaterThan(
      0,
    );
    expect(
      (
        screen.getAllByRole("option", {
          name: "Hyperliquid Wallet（不可交易）",
        })[0] as HTMLOptionElement
      ).disabled,
    ).toBe(true);
  });

  it("keeps create enabled when Ask threshold is negative", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    selectRunMode("spread");
    const askInput = screen
      .getByText("开仓阈值")
      .closest("label")
      ?.querySelector("input");
    expect(askInput).toBeTruthy();
    fireEvent.change(askInput!, { target: { value: "-70" } });
    expect(
      (
        screen.getByRole("button", {
          name: "创建套利组合",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    await waitFor(() =>
      expect(mocks.createArbitrageCombination).toHaveBeenCalledWith(
        expect.objectContaining({
          askThresholdBps: "-70",
          bidThresholdBps: "-8",
        }),
      ),
    );
  }, 15000);

  it("creates a bidirectional combination without fixed leg sides", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    selectRunMode("spread");
    expect(screen.queryByText("交易方向")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    await waitFor(() =>
      expect(mocks.createArbitrageCombination).toHaveBeenCalledWith(
        expect.objectContaining({
          legAAccountId: 3,
          legBAccountId: 4,
          askThresholdBps: "12",
          bidThresholdBps: "-8",
          preferredLeg: "a",
          executionMode: "maker_then_hedge",
          runMode: "spread",
          legALeverage: "4",
          legBLeverage: "4",
        }),
      ),
    );
    expect(mocks.createArbitrageCombination.mock.calls[0]?.[0]).not.toHaveProperty(
      "orderNotional",
    );
  }, 15000);

  it("resolves cross-venue history for the same spot pair", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await waitForCombinationList();
    await waitFor(() =>
      expect(mocks.fetchBasisSpreadHistory).toHaveBeenCalled(),
    );
    expect(
      await screen.findByRole("img", { name: "Best Ask 价差走势" }),
    ).toBeTruthy();
    mocks.fetchBasisSpreadHistory.mockClear();
    const productTypes = screen.getAllByRole("combobox", { name: "产品类型" });
    fireEvent.change(productTypes[0]!, { target: { value: "spot" } });
    fireEvent.change(productTypes[1]!, { target: { value: "spot" } });
    expect(
      await screen.findByText("OKX Ask / BINANCE Ask - 1 · BTC/USDT"),
    ).toBeTruthy();
    expect(
      await screen.findByRole("img", { name: "Best Ask 价差走势" }),
    ).toBeTruthy();
  }, 15000);

  it("shows basis annualized yield for same-venue spot/perpetual pairs", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await waitForCombinationList();
    await screen.findByText("$900.00K");
    mocks.fetchFundingRatesLookup.mockClear();
    const exchanges = screen.getAllByRole("combobox", { name: "交易所" });
    fireEvent.change(exchanges[1]!, { target: { value: "binance" } });
    const productTypes = screen.getAllByRole("combobox", { name: "产品类型" });
    fireEvent.change(productTypes[0]!, { target: { value: "spot" } });
    expect(
      await screen.findByText("Perpetual Ask / Spot Ask - 1 · BTC/USDT"),
    ).toBeTruthy();
    expect(await screen.findByText("24H 差值年化")).toBeTruthy();
    expect(screen.getByText("7D 差值年化")).toBeTruthy();
    expect(await screen.findByText("+41.1%")).toBeTruthy();
    expect(await screen.findByText("+5.9%")).toBeTruthy();
    expect(await screen.findByText("$1.50M")).toBeTruthy();
    expect(screen.getByText("$20.00M")).toBeTruthy();
    expect(screen.getByText("永续腿 OI")).toBeTruthy();
    expect(screen.getByText("永续腿 24H 成交额")).toBeTruthy();
    expect(screen.getByText("A — / B 8h")).toBeTruthy();
    expect(
      screen.getByText("仅展示永续腿；Spot 无资金费流动性指标"),
    ).toBeTruthy();
    expect(mocks.fetchFundingRates).not.toHaveBeenCalled();
  }, 15000);

  it("does not present stale or missing liquidity values as live", async () => {
    setup();
    mocks.fetchFundingRatesLookup.mockImplementation(async (keys) =>
      fundingLookupResponse(keys, [
        {
          ...fundingRate("Binance", "BTCUSDT", 0.02, 0.1, {
            positionNotional: 1_500_000,
            dailyVolume: 20_000_000,
          }),
          stale: true,
        },
      ]),
    );
    render(<ArbitrageTradingView />);
    expect(await screen.findByText("A 已过期 / B 暂无")).toBeTruthy();
    expect(screen.getAllByText("已过期").length).toBeGreaterThan(0);
    expect(screen.queryByText("$1.50M")).toBeNull();
    expect(screen.queryByText("$20.00M")).toBeNull();
  }, 15000);

  it("uses the server lookup row for each requestKey when the same venue has multiple contracts", async () => {
    setup();
    const exact = fundingRate("Binance", "BTCUSDT", 0.02, 0.1, {
      positionNotional: 1_500_000,
      dailyVolume: 20_000_000,
      settlementIntervalHours: 8,
    });
    const assigned = fundingRate("Binance", "BTCUSDT_QM", 0.5, 0.5, {
      positionNotional: 9_000_000,
      dailyVolume: 90_000_000,
      settlementIntervalHours: 1,
    });
    const okx = fundingRate("OKX", "BTC-USDT-SWAP", 0.05, 0.21, {
      positionNotional: 900_000,
      dailyVolume: 30_000_000,
      settlementIntervalHours: 4,
    });
    const altCombo = {
      ...combination,
      id: "arb-2",
      legA: { ...combination.legA, exchangeSymbol: "BTCUSDT_MISSING" },
    };
    mocks.fetchArbitrageCombinations.mockImplementation(async (view: string) => ({
      items: view === "running" ? [combination, altCombo] : [],
      total: view === "running" ? 2 : 0,
      nextCursor: "",
    }));
    mocks.fetchFundingRatesLookup.mockImplementation(async (keys) => ({
      snapshotVersion: "1",
      serverTime: "2026-08-22T10:01:00Z",
      results: keys.map((key: { exchange: string; exchangeSymbol: string }) => {
        const requestKey = fundingRequestKey(key);
        const item =
          requestKey === "binance|btcusdt_missing"
            ? assigned
            : requestKey === "binance|btcusdt"
              ? exact
              : requestKey === "okx|btc-usdt-swap"
                ? okx
                : undefined;
        return item
          ? { key: requestKey, status: "hit" as const, item }
          : { key: requestKey, status: "missing" as const };
      }),
    }));
    render(<ArbitrageTradingView />);
    expect(await screen.findByText("arb-2")).toBeTruthy();
    await waitFor(() => {
      expect(screen.getByText("arb-1").closest("tr")?.textContent).toContain(
        "+10.95%",
      );
      expect(screen.getByText("arb-2").closest("tr")?.textContent).toContain(
        "-164.25%",
      );
    });
    expect(mocks.fetchFundingRates).not.toHaveBeenCalled();
  }, 15000);

  it("keeps last-good 24H annualized after a lookup network error", async () => {
    const intervals = captureThirtySecondIntervals();
    try {
      setup();
      let calls = 0;
      mocks.fetchFundingRatesLookup.mockImplementation(async (keys) => {
        calls += 1;
        if (calls > 1) {
          throw new Error("network");
        }
        return fundingLookupResponse(keys, defaultFundingItems());
      });
      render(<ArbitrageTradingView />);
      expect(
        await screen.findByText("+10.95%", {}, { timeout: 15000 }),
      ).toBeTruthy();
      intervals.flush();
      await waitFor(() =>
        expect(mocks.fetchFundingRatesLookup.mock.calls.length).toBeGreaterThan(
          1,
        ),
      );
      expect(screen.getByText("arb-1").closest("tr")?.textContent).toContain(
        "+10.95%",
      );
    } finally {
      intervals.restore();
    }
  }, 15000);

  it("clears stale cumulative when lookup returns missing", async () => {
    const intervals = captureThirtySecondIntervals();
    try {
      setup();
      let calls = 0;
      mocks.fetchFundingRatesLookup.mockImplementation(async (keys) => {
        calls += 1;
        if (calls > 1) {
          return {
            snapshotVersion: "2",
            serverTime: "2026-08-22T10:02:00Z",
            results: keys.map((key: { exchange: string; exchangeSymbol: string }) => ({
              key: fundingRequestKey(key),
              status: "missing" as const,
            })),
          };
        }
        return fundingLookupResponse(keys, defaultFundingItems());
      });
      render(<ArbitrageTradingView />);
      expect(
        await screen.findByText("+10.95%", {}, { timeout: 15000 }),
      ).toBeTruthy();
      intervals.flush();
      await waitFor(() => {
        expect(screen.getByText("arb-1").closest("tr")?.textContent).not.toContain(
          "+10.95%",
        );
      });
      expect(screen.getByText("arb-1").closest("tr")?.textContent).toContain("--");
    } finally {
      intervals.restore();
    }
  }, 15000);

  it("prefills same-venue spot and perpetual legs from search params", async () => {
    setup();
    render(
      <ArbitrageTradingView
        searchParams={
          new URLSearchParams(
            "mode=arbitrage&legAExchange=binance&legAContract=spot&legABase=BTC&legAQuote=USDT&legBExchange=binance&legBContract=perpetual&legBBase=BTC&legBQuote=USDT&legBExchangeSymbol=BTCUSDT",
          )
        }
      />,
    );
    await waitFor(() => {
      const types = screen.getAllByRole("combobox", { name: "产品类型" });
      expect((types[0] as HTMLSelectElement).value).toBe("spot");
      expect((types[1] as HTMLSelectElement).value).toBe("perpetual");
    });
    const exchanges = screen.getAllByRole("combobox", { name: "交易所" });
    const selectedAccounts = screen.getAllByRole("combobox", { name: "账户" });
    expect((exchanges[0] as HTMLSelectElement).value).toBe("binance");
    expect((exchanges[1] as HTMLSelectElement).value).toBe("binance");
    expect((selectedAccounts[0] as HTMLSelectElement).value).toBe("3");
    expect((selectedAccounts[1] as HTMLSelectElement).value).toBe("3");
    expect(
      await screen.findByText("Perpetual Ask / Spot Ask - 1 · BTC/USDT"),
    ).toBeTruthy();
    await waitForCreateReady();
  }, 15000);

  it("prefills cross-venue perpetual legs from search params", async () => {
    setup();
    render(
      <ArbitrageTradingView
        searchParams={
          new URLSearchParams(
            "mode=arbitrage&legAExchange=binance&legAContract=perpetual&legABase=BTC&legAQuote=USDT&legAExchangeSymbol=BTCUSDT&legBExchange=okx&legBContract=perpetual&legBBase=BTC&legBQuote=USDT&legBExchangeSymbol=BTC-USDT-SWAP",
          )
        }
      />,
    );
    await waitFor(() => {
      const exchanges = screen.getAllByRole("combobox", { name: "交易所" });
      expect((exchanges[0] as HTMLSelectElement).value).toBe("binance");
      expect((exchanges[1] as HTMLSelectElement).value).toBe("okx");
    });
    const types = screen.getAllByRole("combobox", { name: "产品类型" });
    expect((types[0] as HTMLSelectElement).value).toBe("perpetual");
    expect((types[1] as HTMLSelectElement).value).toBe("perpetual");
    expect(
      await screen.findByText("OKX Ask / BINANCE Ask - 1 · BTC/USDT"),
    ).toBeTruthy();
  }, 15000);

  it("reapplies both legs when prefill query changes without remounting", async () => {
    setup();
    const view = render(
      <ArbitrageTradingView
        searchParams={
          new URLSearchParams(
            "mode=arbitrage&legAExchange=binance&legAContract=spot&legABase=BTC&legAQuote=USDT&legBExchange=binance&legBContract=perpetual&legBBase=BTC&legBQuote=USDT",
          )
        }
      />,
    );
    await waitFor(
      () => {
        const symbols = screen.getAllByRole("combobox", { name: "Symbol" });
        expect((symbols[0] as HTMLSelectElement).value).toBe("6");
        expect((symbols[1] as HTMLSelectElement).value).toBe("7");
      },
      { timeout: 5000 },
    );

    view.rerender(
      <ArbitrageTradingView
        searchParams={
          new URLSearchParams(
            "mode=arbitrage&legAExchange=binance&legAContract=perpetual&legABase=BTC&legAQuote=USDT&legAExchangeSymbol=BTCUSDT&legBExchange=okx&legBContract=perpetual&legBBase=BTC&legBQuote=USDT&legBExchangeSymbol=BTC-USDT-SWAP",
          )
        }
      />,
    );
    await waitFor(
      () => {
        expect(mocks.fetchTraderInstruments).toHaveBeenCalledWith(
          3,
          "perpetual",
        );
        expect(mocks.fetchTraderInstruments).toHaveBeenCalledWith(
          4,
          "perpetual",
        );
      },
      { timeout: 5000 },
    );
    await waitFor(
      () => {
        const exchanges = screen.getAllByRole("combobox", { name: "交易所" });
        const symbols = screen.getAllByRole("combobox", { name: "Symbol" });
        expect((exchanges[0] as HTMLSelectElement).value).toBe("binance");
        expect((exchanges[1] as HTMLSelectElement).value).toBe("okx");
        expect((symbols[0] as HTMLSelectElement).value).toBe("7");
        expect((symbols[1] as HTMLSelectElement).value).toBe("8");
      },
      { timeout: 5000 },
    );
  }, 15000);

  it("prefills Gate and OKX BEAT legs from a product containing both exchanges", async () => {
    setup();
    mocks.fetchTradingAccounts.mockResolvedValue([
      {
        ...accounts[0],
        id: 10,
        productName: "A-only",
        exchange: "Gate",
        exchangeSlug: "gate",
      },
      {
        ...accounts[0],
        id: 11,
        productName: "BEAT套利",
        exchange: "Gate",
        exchangeSlug: "gate",
      },
      {
        ...accounts[1],
        id: 12,
        productName: "BEAT套利",
        exchange: "OKX",
        exchangeSlug: "okx",
      },
    ]);
    mocks.fetchTraderInstruments.mockImplementation(
      async (accountID: number) => [
        {
          ...instrument,
          id: accountID === 11 ? 21 : 22,
          exchange: accountID === 11 ? "gate" : "okx",
          exchangeSymbol: accountID === 11 ? "BEATUSDT" : "BEAT-USDT-SWAP",
          baseAsset: "BEAT",
          quoteAsset: "USDT",
        },
      ],
    );
    render(
      <ArbitrageTradingView
        searchParams={
          new URLSearchParams(
            "mode=arbitrage&legAExchange=gate&legAContract=perpetual&legABase=BEAT&legAQuote=USDT&legAExchangeSymbol=BEAT_USDT&legBExchange=okx&legBContract=perpetual&legBBase=BEAT&legBQuote=USDT&legBExchangeSymbol=BEAT-USDT-SWAP",
          )
        }
      />,
    );
    await waitFor(() => {
      expect(mocks.fetchTraderInstruments).toHaveBeenCalledWith(
        11,
        "perpetual",
      );
      expect(mocks.fetchTraderInstruments).toHaveBeenCalledWith(
        12,
        "perpetual",
      );
    });
    await waitFor(() => {
      const exchanges = screen.getAllByRole("combobox", { name: "交易所" });
      const selectedAccounts = screen.getAllByRole("combobox", {
        name: "账户",
      });
      const symbols = screen.getAllByRole("combobox", { name: "Symbol" });
      expect((exchanges[0] as HTMLSelectElement).value).toBe("gate");
      expect((exchanges[1] as HTMLSelectElement).value).toBe("okx");
      expect((selectedAccounts[0] as HTMLSelectElement).value).toBe("11");
      expect((selectedAccounts[1] as HTMLSelectElement).value).toBe("12");
      expect((symbols[0] as HTMLSelectElement).value).toBe("21");
      expect((symbols[1] as HTMLSelectElement).value).toBe("22");
    });
  }, 15000);

  it("prefills Bitget and Aster Chinese symbols without selecting an earlier listing", async () => {
    setup();
    mocks.fetchTradingAccounts.mockResolvedValue([
      {
        ...accounts[0],
        id: 31,
        productName: "龙虾套利",
        exchange: "Bitget",
        exchangeSlug: "bitget",
        accountName: "Bitget Main",
      },
      {
        ...accounts[0],
        id: 32,
        productName: "龙虾套利",
        exchange: "Aster",
        exchangeSlug: "aster",
        accountName: "Aster Wallet",
        tradingReady: true,
        tradingStatus: "ready",
      },
    ]);
    const chineseInstrument = (
      id: number,
      exchange: string,
      base: string,
    ) => ({
      ...instrument,
      id,
      exchange,
      contractType: "perpetual" as const,
      exchangeSymbol: `${base}USDT`,
      baseAsset: base,
      quoteAsset: "USDT",
    });
    mocks.fetchTraderInstruments.mockImplementation(async (accountID: number) =>
      accountID === 31
        ? [
            chineseInstrument(101, "bitget", "哈基米"),
            chineseInstrument(102, "bitget", "龙虾"),
          ]
        : [
            chineseInstrument(201, "aster", "哈基米"),
            chineseInstrument(202, "aster", "龙虾"),
          ],
    );
    render(
      <ArbitrageTradingView
        searchParams={
          new URLSearchParams(
            "mode=arbitrage&legAExchange=bitget&legAContract=perpetual&legABase=龙虾&legAQuote=USDT&legAExchangeSymbol=龙虾USDT&legBExchange=aster&legBContract=perpetual&legBBase=龙虾&legBQuote=USDT&legBExchangeSymbol=龙虾USDT",
          )
        }
      />,
    );
    await waitFor(() => {
      expect(mocks.fetchTraderInstruments).toHaveBeenCalledWith(
        31,
        "perpetual",
      );
      expect(mocks.fetchTraderInstruments).toHaveBeenCalledWith(
        32,
        "perpetual",
      );
    });
    await waitFor(() => {
      const symbols = screen.getAllByRole("combobox", { name: "Symbol" });
      expect((symbols[0] as HTMLSelectElement).value).toBe("102");
      expect((symbols[1] as HTMLSelectElement).value).toBe("202");
    });
    expect(
      await screen.findByText(/龙虾\/USDT 配对有效/, {}, { timeout: 15000 }),
    ).toBeTruthy();
    await waitFor(
      () =>
        expect(
          (
            screen.getByRole("button", {
              name: "创建套利组合",
            }) as HTMLButtonElement
          ).disabled,
        ).toBe(false),
      { timeout: 15000 },
    );
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    await waitFor(() =>
      expect(mocks.createArbitrageCombination).toHaveBeenCalledWith(
        expect.objectContaining({
          legAInstrumentId: 102,
          legBInstrumentId: 202,
        }),
      ),
    );
  }, 15000);

  it("does not display the first instrument when prefill cannot be matched", async () => {
    setup();
    render(
      <ArbitrageTradingView
        searchParams={
          new URLSearchParams(
            "mode=arbitrage&legAExchange=binance&legAContract=perpetual&legABase=ETH&legAQuote=USDT&legAExchangeSymbol=ETHUSDT&legBExchange=okx&legBContract=perpetual&legBBase=ETH&legBQuote=USDT&legBExchangeSymbol=ETH-USDT-SWAP",
          )
        }
      />,
    );
    await waitFor(() => {
      const symbols = screen.getAllByRole("combobox", { name: "Symbol" });
      expect((symbols[0] as HTMLSelectElement).value).toBe("");
      expect((symbols[1] as HTMLSelectElement).value).toBe("");
    });
  }, 15000);

  it("uses a compact two-column create layout without order-size copy", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    expect(screen.queryByText("最大未对冲敞口")).toBeNull();
    expect(screen.queryByText("单笔订单金额")).toBeNull();
    expect(screen.queryByText(/订单金额自动计算/)).toBeNull();
    expect(screen.queryByText(/0～20U 随机值/)).toBeNull();
    expect(screen.getAllByText("可用资金")).toHaveLength(2);
    const fundsFooters = screen.getAllByText("可用资金").map((node) => node.parentElement);
    expect(fundsFooters).toHaveLength(2);
    for (const footer of fundsFooters) {
      expect(footer?.className).toContain("justify-end");
      expect(footer?.className).toContain("mt-auto");
      expect(footer?.parentElement?.lastElementChild).toBe(footer);
    }
    expect(
      Array.from(document.querySelectorAll("section")).some((section) =>
        section.className.includes(
          "xl:grid-cols-[minmax(0,2fr)_minmax(420px,0.95fr)]",
        ),
      ),
    ).toBe(true);
    expect(
      Array.from(document.querySelectorAll("div")).some((node) =>
        node.className.includes("sm:grid-cols-2") &&
        !node.className.includes("2xl:grid-cols-4"),
      ),
    ).toBe(true);
  }, 15000);

  it("defaults to one_shot and hides spread thresholds", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    expect(
      (screen.getByRole("combobox", { name: "运行模式" }) as HTMLSelectElement)
        .value,
    ).toBe("one_shot");
    expect(screen.queryByText("开仓阈值")).toBeNull();
    expect(screen.getByRole("combobox", { name: "退出方式" })).toBeTruthy();
    expect(
      screen.getByRole("checkbox", { name: "启用8h资金费年化提前退出" }),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    await waitFor(() =>
      expect(mocks.createArbitrageCombination).toHaveBeenCalledWith(
        expect.objectContaining({
          runMode: "one_shot",
          entryDirection: "ask",
          earlyExitFunding8hAnnualizedFloor: "",
        }),
      ),
    );
  }, 15000);

  it("shows one_shot create fields and hides spread thresholds", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    expect(screen.queryByRole("combobox", { name: "入场方向" })).toBeNull();
    expect(screen.getByText("固定买 Leg A / 卖 Leg B")).toBeTruthy();
    expect(screen.getByRole("combobox", { name: "退出方式" })).toBeTruthy();
    expect(screen.getByText("退出年化")).toBeTruthy();
    expect(screen.queryByText("开仓阈值")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    await waitFor(() =>
      expect(mocks.createArbitrageCombination).toHaveBeenCalledWith(
        expect.objectContaining({
          runMode: "one_shot",
          entryDirection: "ask",
          exitPolicy: "annualized",
          exitAnnualizedRate: "0.15",
          earlyExitFunding8hAnnualizedFloor: "",
        }),
      ),
    );
  }, 15000);

  it("enables one_shot create without filling hidden spread thresholds", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    expect(
      (
        screen.getByRole("button", {
          name: "创建套利组合",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    await waitFor(() =>
      expect(mocks.createArbitrageCombination).toHaveBeenCalledWith(
        expect.objectContaining({
          runMode: "one_shot",
          entryDirection: "ask",
          askThresholdBps: "",
          bidThresholdBps: "",
          earlyExitFunding8hAnnualizedFloor: "",
        }),
      ),
    );
  }, 15000);

  it("disables spread create when thresholds are empty", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    selectRunMode("spread");
    const askInput = screen
      .getByText("开仓阈值")
      .closest("label")
      ?.querySelector("input");
    expect(askInput).toBeTruthy();
    fireEvent.change(askInput!, { target: { value: "" } });
    expect(
      (
        screen.getByRole("button", {
          name: "创建套利组合",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
  }, 15000);

  it("converts an 80 percent annualized exit to a ratio on create", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    fireEvent.change(screen.getByRole("combobox", { name: "运行模式" }), {
      target: { value: "one_shot" },
    });
    fireEvent.change(screen.getByRole("textbox", { name: /退出年化/ }), {
      target: { value: "80" },
    });
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    await waitFor(() =>
      expect(mocks.createArbitrageCombination).toHaveBeenCalledWith(
        expect.objectContaining({
          entryDirection: "ask",
          exitAnnualizedRate: "0.8",
        }),
      ),
    );
  }, 15000);

  it("converts a 12.5 percent annualized exit to a ratio on create", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    fireEvent.change(screen.getByRole("combobox", { name: "运行模式" }), {
      target: { value: "one_shot" },
    });
    fireEvent.change(screen.getByRole("textbox", { name: /退出年化/ }), {
      target: { value: "12.5" },
    });
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    await waitFor(() =>
      expect(mocks.createArbitrageCombination).toHaveBeenCalledWith(
        expect.objectContaining({
          entryDirection: "ask",
          exitAnnualizedRate: "0.125",
        }),
      ),
    );
  }, 15000);

  it("does not allow creating one_shot with a non-positive annualized percent", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    fireEvent.change(screen.getByRole("combobox", { name: "运行模式" }), {
      target: { value: "one_shot" },
    });
    fireEvent.change(screen.getByRole("textbox", { name: /退出年化/ }), {
      target: { value: "0" },
    });
    expect(
      (
        screen.getByRole("button", {
          name: "创建套利组合",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    expect(mocks.createArbitrageCombination).not.toHaveBeenCalled();
  }, 15000);

  it("sends 8h funding floor ratio when the early-exit checkbox is checked", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    fireEvent.click(
      screen.getByRole("checkbox", { name: "启用8h资金费年化提前退出" }),
    );
    fireEvent.change(screen.getByRole("textbox", { name: "8h资金费年化阈值" }), {
      target: { value: "5" },
    });
    expect(screen.getByText("8h资金费年化<")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    await waitFor(() =>
      expect(mocks.createArbitrageCombination).toHaveBeenCalledWith(
        expect.objectContaining({
          runMode: "one_shot",
          earlyExitFunding8hAnnualizedFloor: "0.05",
        }),
      ),
    );
  }, 15000);

  it("blocks one_shot create when the 8h floor checkbox is checked but empty", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    fireEvent.click(
      screen.getByRole("checkbox", { name: "启用8h资金费年化提前退出" }),
    );
    expect(screen.getByText("请填写有效年化阈值")).toBeTruthy();
    expect(
      (
        screen.getByRole("button", {
          name: "创建套利组合",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    expect(mocks.createArbitrageCombination).not.toHaveBeenCalled();
  }, 15000);

  it("hides 0 bps chart lines and shows the 8h floor on one_shot detail", async () => {
    setup();
    const oneShot: ArbitrageCombination = {
      ...combination,
      runMode: "one_shot",
      entryDirection: "ask",
      oneShotPhase: "waiting_exit",
      exitPolicy: "time",
      exitAfterSeconds: 3600,
      askThresholdBps: "0",
      bidThresholdBps: "0",
      earlyExitFunding8hAnnualizedFloor: "0.05",
    };
    mocks.fetchArbitrageCombinations.mockImplementation(async (view: string) => ({
      items: view === "running" ? [oneShot] : [],
      total: view === "running" ? 1 : 0,
      nextCursor: "",
    }));
    mocks.fetchArbitrageCombination.mockResolvedValue({
      ...oneShot,
      orders: [],
      recentExecutions: [],
      recentEvents: [],
    });
    render(<ArbitrageTradingView />);
    fireEvent.click((await screen.findByText("arb-1")).closest("tr")!);
    expect(await screen.findByText("8h资金费年化< 5.00%")).toBeTruthy();
    expect(screen.queryByText("开仓 0 bps")).toBeNull();
    expect(screen.queryByText("平仓 0 bps")).toBeNull();
    expect(screen.queryByText("开仓 12 bps")).toBeNull();
  }, 15000);

  it("loads cached funds once per account id and ignores symbol or leverage changes", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    await waitFor(() => {
      expect(mocks.fetchCachedTradingAccountSnapshot).toHaveBeenCalledTimes(2);
    });
    expect(mocks.fetchCachedTradingAccountSnapshot).toHaveBeenCalledWith(
      3,
      expect.any(AbortSignal),
    );
    expect(mocks.fetchCachedTradingAccountSnapshot).toHaveBeenCalledWith(
      4,
      expect.any(AbortSignal),
    );
    expect(mocks.fetchLiveTradingAccountSnapshot).not.toHaveBeenCalled();
    expect(await screen.findByText("1,234.56 USD")).toBeTruthy();
    expect(screen.getByText("80.00 USD")).toBeTruthy();
    expect(screen.queryByText(/· 缓存/)).toBeNull();
    fireEvent.change(screen.getAllByRole("combobox", { name: "产品类型" })[0]!, {
      target: { value: "spot" },
    });
    fireEvent.change(screen.getByRole("textbox", { name: /目标仓位/ }), {
      target: { value: "12000" },
    });
    fireEvent.change(screen.getAllByRole("combobox", { name: "Symbol" })[0]!, {
      target: { value: "6" },
    });
    await waitFor(() =>
      expect(mocks.fetchCachedTradingAccountSnapshot).toHaveBeenCalledTimes(2),
    );
    expect(mocks.fetchLiveTradingAccountSnapshot).not.toHaveBeenCalled();
  }, 15000);

  it("aborts the previous funds request when the bound account changes", async () => {
    setup();
    let firstSignal: AbortSignal | undefined;
    mocks.fetchCachedTradingAccountSnapshot.mockImplementation(
      (id: number, signal?: AbortSignal) =>
        new Promise((resolve, reject) => {
          if (id === 3 && firstSignal == null) {
            firstSignal = signal;
          }
          const timer = window.setTimeout(() => {
            resolve(cachedSnapshot(id, id === 5 ? "9.00" : "1234.56"));
          }, 40);
          signal?.addEventListener("abort", () => {
            window.clearTimeout(timer);
            reject(new DOMException("Aborted", "AbortError"));
          });
        }),
    );
    render(<ArbitrageTradingView />);
    await waitFor(() =>
      expect(mocks.fetchCachedTradingAccountSnapshot).toHaveBeenCalled(),
    );
    fireEvent.change(screen.getAllByRole("combobox", { name: "账户" })[0]!, {
      target: { value: "5" },
    });
    await waitFor(() => expect(firstSignal?.aborted).toBe(true));
    expect(await screen.findByText("9.00 USD")).toBeTruthy();
    expect(
      (
        screen.getAllByRole("combobox", { name: "账户" })[0] as HTMLSelectElement
      ).value,
    ).toBe("5");
  }, 15000);

  it("live-fetches once after a cache miss and does not block create", async () => {
    setup();
    mocks.fetchCachedTradingAccountSnapshot.mockResolvedValue({
      status: "missing",
    });
    mocks.fetchLiveTradingAccountSnapshot.mockImplementation(async (id: number) =>
      cachedSnapshot(id, id === 4 ? "80" : "1234.56").snapshot,
    );
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    await waitFor(() =>
      expect(mocks.fetchLiveTradingAccountSnapshot).toHaveBeenCalledTimes(2),
    );
    expect(await screen.findByText("1,234.56 USD")).toBeTruthy();
    expect(screen.getByText("80.00 USD")).toBeTruthy();
    expect(screen.queryByText("暂无快照")).toBeNull();
    expect(
      (
        screen.getByRole("button", {
          name: "创建套利组合",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(false);
  }, 15000);

  it("shows an ellipsis while a cache miss is live-fetching", async () => {
    setup();
    mocks.fetchCachedTradingAccountSnapshot.mockResolvedValue({
      status: "missing",
    });
    const resolvers = new Map<
      number,
      (value: ReturnType<typeof cachedSnapshot>["snapshot"]) => void
    >();
    mocks.fetchLiveTradingAccountSnapshot.mockImplementation(
      (id: number) =>
        new Promise((resolve) => {
          resolvers.set(id, resolve);
        }),
    );
    render(<ArbitrageTradingView />);
    expect((await screen.findAllByText("…")).length).toBeGreaterThan(0);
    resolvers.get(3)?.(cachedSnapshot(3, "1234.56").snapshot);
    resolvers.get(4)?.(cachedSnapshot(4, "80").snapshot);
    expect(await screen.findByText("1,234.56 USD")).toBeTruthy();
    expect(screen.getByText("80.00 USD")).toBeTruthy();
  }, 15000);

  it("live-fetches once when the cached snapshot is older than 10 minutes", async () => {
    setup();
    mocks.fetchCachedTradingAccountSnapshot.mockImplementation(async (id: number) =>
      cachedSnapshot(id, "874.50", {
        sourceUpdatedAt: "2026-01-01T00:00:00.000Z",
        serverTime: "2026-01-01T00:10:01.000Z",
      }),
    );
    const resolvers = new Map<
      number,
      (value: ReturnType<typeof cachedSnapshot>["snapshot"]) => void
    >();
    mocks.fetchLiveTradingAccountSnapshot.mockImplementation(
      (id: number) =>
        new Promise((resolve) => {
          resolvers.set(id, resolve);
        }),
    );
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    expect(await screen.findAllByText("874.50 USD")).toHaveLength(2);
    expect(mocks.fetchLiveTradingAccountSnapshot).toHaveBeenCalledTimes(2);
    resolvers.get(3)?.(cachedSnapshot(3, "900.00").snapshot);
    resolvers.get(4)?.(cachedSnapshot(4, "900.00").snapshot);
    await waitFor(() =>
      expect(screen.getAllByText("900.00 USD")).toHaveLength(2),
    );
    expect(screen.queryByText(/· 缓存/)).toBeNull();
  }, 15000);

  it("does not live-fetch when the cached snapshot is exactly 10 minutes old", async () => {
    setup();
    mocks.fetchCachedTradingAccountSnapshot.mockImplementation(async (id: number) =>
      cachedSnapshot(id, "874.50", {
        sourceUpdatedAt: "2026-01-01T00:00:00.000Z",
        serverTime: "2026-01-01T00:10:00.000Z",
      }),
    );
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    expect(await screen.findAllByText("874.50 USD")).toHaveLength(2);
    expect(mocks.fetchLiveTradingAccountSnapshot).not.toHaveBeenCalled();
  }, 15000);

  it("live-fetches when sourceUpdatedAt cannot be parsed", async () => {
    setup();
    mocks.fetchCachedTradingAccountSnapshot.mockImplementation(async (id: number) =>
      cachedSnapshot(id, "874.50", {
        sourceUpdatedAt: "not-a-date",
        serverTime: "2026-01-01T00:00:00.000Z",
      }),
    );
    const resolvers = new Map<
      number,
      (value: ReturnType<typeof cachedSnapshot>["snapshot"]) => void
    >();
    mocks.fetchLiveTradingAccountSnapshot.mockImplementation(
      (id: number) =>
        new Promise((resolve) => {
          resolvers.set(id, resolve);
        }),
    );
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    expect(await screen.findAllByText("874.50 USD")).toHaveLength(2);
    expect(mocks.fetchLiveTradingAccountSnapshot).toHaveBeenCalledTimes(2);
    resolvers.get(3)?.(cachedSnapshot(3, "900.00").snapshot);
    resolvers.get(4)?.(cachedSnapshot(4, "900.00").snapshot);
    await waitFor(() =>
      expect(screen.getAllByText("900.00 USD")).toHaveLength(2),
    );
    expect(mocks.fetchLiveTradingAccountSnapshot).toHaveBeenCalledTimes(2);
  }, 15000);

  it("does not apply a live snapshot after the selected account changes", async () => {
    setup();
    let resolveAccount3!: (
      value: ReturnType<typeof cachedSnapshot>["snapshot"],
    ) => void;
    mocks.fetchCachedTradingAccountSnapshot.mockImplementation(
      async (id: number) => {
        if (id === 3) {
          return { status: "missing" };
        }
        return cachedSnapshot(id, id === 5 ? "9.00" : "80");
      },
    );
    mocks.fetchLiveTradingAccountSnapshot.mockImplementation(
      (id: number) =>
        new Promise((resolve) => {
          if (id === 3) {
            resolveAccount3 = resolve;
            return;
          }
          resolve(cachedSnapshot(id, id === 5 ? "9.00" : "80").snapshot);
        }),
    );
    render(<ArbitrageTradingView />);
    await waitFor(() =>
      expect(mocks.fetchLiveTradingAccountSnapshot).toHaveBeenCalledWith(3),
    );
    fireEvent.change(screen.getAllByRole("combobox", { name: "账户" })[0]!, {
      target: { value: "5" },
    });
    expect(await screen.findByText("9.00 USD")).toBeTruthy();
    resolveAccount3(cachedSnapshot(3, "9999.00").snapshot);
    await new Promise((resolve) => window.setTimeout(resolve, 50));
    expect(screen.queryByText("9,999.00 USD")).toBeNull();
    expect(screen.getByText("9.00 USD")).toBeTruthy();
  }, 15000);

  it("does not retry after a live snapshot failure", async () => {
    setup();
    mocks.fetchCachedTradingAccountSnapshot.mockResolvedValue({
      status: "missing",
    });
    mocks.fetchLiveTradingAccountSnapshot.mockRejectedValue(new Error("down"));
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    expect(await screen.findAllByText("暂不可用")).toHaveLength(2);
    expect(mocks.fetchLiveTradingAccountSnapshot).toHaveBeenCalledTimes(2);
    await new Promise((resolve) => window.setTimeout(resolve, 50));
    expect(mocks.fetchLiveTradingAccountSnapshot).toHaveBeenCalledTimes(2);
  }, 15000);

  it("keeps the previous amount when a stale cache refresh fails", async () => {
    setup();
    mocks.fetchCachedTradingAccountSnapshot.mockImplementation(async (id: number) =>
      cachedSnapshot(id, "874.50", {
        stale: true,
        sourceUpdatedAt: "2026-01-01T00:00:00.000Z",
        serverTime: "2026-01-01T00:10:01.000Z",
      }),
    );
    mocks.fetchLiveTradingAccountSnapshot.mockRejectedValue(new Error("down"));
    render(<ArbitrageTradingView />);
    expect(await screen.findAllByText("874.50 USD")).toHaveLength(2);
    await waitFor(() =>
      expect(mocks.fetchLiveTradingAccountSnapshot).toHaveBeenCalledTimes(2),
    );
    expect(screen.getAllByText("874.50 USD")).toHaveLength(2);
    expect(screen.queryByText("暂不可用")).toBeNull();
    expect(screen.queryByText(/· 缓存/)).toBeNull();
  }, 15000);

  it("does not live-fetch again when the view rerenders", async () => {
    setup();
    mocks.fetchCachedTradingAccountSnapshot.mockResolvedValue({
      status: "missing",
    });
    mocks.fetchLiveTradingAccountSnapshot.mockImplementation(async (id: number) =>
      cachedSnapshot(id, id === 4 ? "80" : "1234.56").snapshot,
    );
    const view = render(<ArbitrageTradingView />);
    await waitForCreateReady();
    await waitFor(() =>
      expect(mocks.fetchLiveTradingAccountSnapshot).toHaveBeenCalledTimes(2),
    );
    view.rerender(<ArbitrageTradingView />);
    await waitFor(() => {
      expect(screen.getByText("1,234.56 USD")).toBeTruthy();
    });
    expect(mocks.fetchLiveTradingAccountSnapshot).toHaveBeenCalledTimes(2);
  }, 15000);

  it("merges concurrent live requests when both legs share an account", async () => {
    setup();
    mocks.fetchTradingAccounts.mockResolvedValue([accounts[0]]);
    mocks.fetchCachedTradingAccountSnapshot.mockResolvedValue({
      status: "missing",
    });
    mocks.fetchLiveTradingAccountSnapshot.mockImplementation(
      (id: number) =>
        new Promise((resolve) => {
          window.setTimeout(() => {
            resolve(cachedSnapshot(id, "50.00").snapshot);
          }, 40);
        }),
    );
    render(<ArbitrageTradingView />);
    await waitFor(() =>
      expect(mocks.fetchLiveTradingAccountSnapshot).toHaveBeenCalled(),
    );
    expect(await screen.findAllByText("50.00 USD")).toHaveLength(2);
    expect(mocks.fetchLiveTradingAccountSnapshot).toHaveBeenCalledTimes(1);
  }, 15000);

  it("shows stale cached funds without a cache label", async () => {
    setup();
    mocks.fetchCachedTradingAccountSnapshot.mockImplementation(async (id: number) =>
      cachedSnapshot(id, "1234.56", { stale: true }),
    );
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    expect(await screen.findAllByText("1,234.56 USD")).toHaveLength(2);
    expect(screen.queryByText(/· 缓存/)).toBeNull();
    expect(mocks.fetchLiveTradingAccountSnapshot).not.toHaveBeenCalled();
  }, 15000);

  it("shows checking state while create is in flight", async () => {
    setup();
    let finishCreate!: (value: ArbitrageCombination) => void;
    mocks.createArbitrageCombination.mockImplementation(
      () =>
        new Promise((resolve) => {
          finishCreate = resolve;
        }),
    );
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    expect(
      await screen.findByRole("button", { name: "正在检查并创建…" }),
    ).toBeTruthy();
    expect(
      (
        screen.getByRole("button", {
          name: "正在检查并创建…",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    finishCreate(combination);
    expect(
      await screen.findByRole("button", { name: "创建套利组合" }),
    ).toBeTruthy();
  }, 15000);

  it("pins create failures onto the matching leg card", async () => {
    setup();
    mocks.createArbitrageCombination.mockRejectedValueOnce(
      new ArbitrageCreateFailure(
        "okx available margin is insufficient",
        "insufficient_margin",
        "b",
        { exchange: "okx" },
      ),
    );
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    expect(
      await screen.findAllByText("okx available margin is insufficient"),
    ).toHaveLength(2);
  }, 15000);

  it("shows partial leverage apply failures on the failed leg", async () => {
    setup();
    mocks.createArbitrageCombination.mockRejectedValueOnce(
      new ArbitrageCreateFailure(
        "partial exchange leverage already changed",
        "leverage_apply_failed",
        "b",
        { appliedLegs: '["a"]', exchange: "okx" },
      ),
    );
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    expect(
      await screen.findAllByText(/partial exchange leverage already changed/),
    ).toHaveLength(2);
    expect(screen.getByText(/已修改杠杆的腿：\["a"\]/)).toBeTruthy();
  }, 15000);

  it("does not claim leverage was applied when the result is uncertain", async () => {
    setup();
    mocks.createArbitrageCombination.mockRejectedValueOnce(
      new ArbitrageCreateFailure(
        "okx leverage apply result is uncertain; combination was not created",
        "leverage_apply_failed",
        "b",
        { appliedLegs: '["a"]', exchange: "okx", uncertain: "true" },
      ),
    );
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    expect(
      await screen.findByText(/可能已修改杠杆的腿：\["a"\]/),
    ).toBeTruthy();
    expect(screen.getByRole("alert").textContent).not.toContain(
      "（已修改杠杆的腿",
    );
  }, 15000);

  it("shows Aster leverage venue errors on the failed leg and toast", async () => {
    setup();
    mocks.createArbitrageCombination.mockRejectedValueOnce(
      new ArbitrageCreateFailure(
        "aster leverage was not applied; combination was not created",
        "leverage_apply_failed",
        "b",
        {
          exchange: "aster",
          error: "upstream status 400: Signature check failed",
        },
      ),
    );
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    expect(
      await screen.findAllByText(
        /aster leverage was not applied; combination was not created：upstream status 400: Signature check failed/,
      ),
    ).toHaveLength(2);
  }, 15000);

  it("shows Bitget leverage venue errors with the same unified display", async () => {
    setup();
    mocks.createArbitrageCombination.mockRejectedValueOnce(
      new ArbitrageCreateFailure(
        "bitget leverage was not applied; combination was not created",
        "leverage_apply_failed",
        "a",
        {
          exchange: "bitget",
          error: "upstream status 400: leverage cannot be set",
        },
      ),
    );
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    expect(
      await screen.findAllByText(
        /bitget leverage was not applied; combination was not created：upstream status 400: leverage cannot be set/,
      ),
    ).toHaveLength(2);
  }, 15000);

  it("keeps the generic leverage message when details.error is missing", async () => {
    setup();
    mocks.createArbitrageCombination.mockRejectedValueOnce(
      new ArbitrageCreateFailure(
        "aster leverage was not applied; combination was not created",
        "leverage_apply_failed",
        "b",
        { exchange: "aster" },
      ),
    );
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    expect(
      await screen.findAllByText(
        "aster leverage was not applied; combination was not created",
      ),
    ).toHaveLength(2);
    expect(screen.getByRole("alert").textContent).not.toContain("：");
  }, 15000);

  it("keeps uncertain applied-leg wording when a venue error is present", async () => {
    setup();
    mocks.createArbitrageCombination.mockRejectedValueOnce(
      new ArbitrageCreateFailure(
        "aster leverage apply result is uncertain; combination was not created",
        "leverage_apply_failed",
        "b",
        {
          appliedLegs: '["a"]',
          exchange: "aster",
          uncertain: "true",
          error: "upstream status 504: timeout",
        },
      ),
    );
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    expect(
      await screen.findByText(
        /aster leverage apply result is uncertain; combination was not created：upstream status 504: timeout（可能已修改杠杆的腿：\["a"\]）/,
      ),
    ).toBeTruthy();
    expect(screen.getByRole("alert").textContent).not.toContain(
      "（已修改杠杆的腿",
    );
  }, 15000);

  it("does not duplicate a venue error already present in the message", async () => {
    setup();
    const venueError = "upstream status 400: Signature check failed";
    mocks.createArbitrageCombination.mockRejectedValueOnce(
      new ArbitrageCreateFailure(
        `aster leverage was not applied; combination was not created：${venueError}`,
        "leverage_apply_failed",
        "b",
        { exchange: "aster", error: venueError },
      ),
    );
    render(<ArbitrageTradingView />);
    await waitForCreateReady();
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    const displayed = await screen.findAllByText(
      `aster leverage was not applied; combination was not created：${venueError}`,
    );
    expect(displayed).toHaveLength(2);
    expect(
      screen.getByRole("alert").textContent?.split(venueError).length,
    ).toBe(2);
  }, 15000);

  it("does not reapply the same prefillRequestId over a user-edited leg", async () => {
    setup();
    const params = new URLSearchParams(
      "mode=arbitrage&legAExchange=binance&legAContract=spot&legABase=BTC&legAQuote=USDT&legBExchange=binance&legBContract=perpetual&legBBase=BTC&legBQuote=USDT&prefillRequestId=req-1",
    );
    const view = render(<ArbitrageTradingView searchParams={params} />);
    await waitFor(() => {
      const types = screen.getAllByRole("combobox", { name: "产品类型" });
      expect((types[0] as HTMLSelectElement).value).toBe("spot");
    });
    fireEvent.change(screen.getAllByRole("combobox", { name: "交易所" })[0]!, {
      target: { value: "okx" },
    });
    view.rerender(
      <ArbitrageTradingView searchParams={new URLSearchParams(params.toString())} />,
    );
    expect(
      (screen.getAllByRole("combobox", { name: "交易所" })[0] as HTMLSelectElement)
        .value,
    ).toBe("okx");
  }, 15000);

  it("reapplies identical legs when prefillRequestId changes and clears the selected chart", async () => {
    setup();
    const first = new URLSearchParams(
      "mode=arbitrage&legAExchange=binance&legAContract=spot&legABase=BTC&legAQuote=USDT&legBExchange=binance&legBContract=perpetual&legBBase=BTC&legBQuote=USDT&prefillRequestId=req-1",
    );
    const view = render(<ArbitrageTradingView searchParams={first} />);
    await waitFor(() => {
      const types = screen.getAllByRole("combobox", { name: "产品类型" });
      expect((types[0] as HTMLSelectElement).value).toBe("spot");
    });
    fireEvent.change(screen.getAllByRole("combobox", { name: "交易所" })[0]!, {
      target: { value: "okx" },
    });
    fireEvent.click(combinationRow("arb-1"));
    await waitFor(() =>
      expect(combinationRow("arb-1").className).toContain("ring-primary/30"),
    );

    const second = new URLSearchParams(first.toString());
    second.set("prefillRequestId", "req-2");
    view.rerender(<ArbitrageTradingView searchParams={second} />);
    await waitFor(() => {
      const exchanges = screen.getAllByRole("combobox", { name: "交易所" });
      expect((exchanges[0] as HTMLSelectElement).value).toBe("binance");
      expect((exchanges[1] as HTMLSelectElement).value).toBe("binance");
    });
    const types = screen.getAllByRole("combobox", { name: "产品类型" });
    expect((types[0] as HTMLSelectElement).value).toBe("spot");
    expect((types[1] as HTMLSelectElement).value).toBe("perpetual");
    expect(combinationRow("arb-1").className).not.toContain("ring-primary/30");
  }, 15000);

  it("ignores a late account response after the view is hidden", async () => {
    setup();
    const resolvers: Array<(value: typeof accounts) => void> = [];
    mocks.fetchTradingAccounts.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolvers.push(resolve);
        }),
    );
    function Harness({ hidden }: { hidden: boolean }) {
      return (
        <Activity mode={hidden ? "hidden" : "visible"}>
          <ArbitrageTradingView />
        </Activity>
      );
    }
    const view = render(<Harness hidden={false} />);
    await waitFor(() => expect(resolvers.length).toBe(1));
    view.rerender(<Harness hidden />);
    resolvers[0]?.(accounts);
    await new Promise((resolve) => setTimeout(resolve, 20));
    view.rerender(<Harness hidden={false} />);
    await waitFor(() => expect(resolvers.length).toBe(2));
    const empty = screen.getAllByRole("combobox", { name: "交易所" });
    expect((empty[0] as HTMLSelectElement).value).toBe("");
    resolvers[1]?.(accounts);
    await waitFor(() => {
      const exchanges = screen.getAllByRole("combobox", { name: "交易所" });
      expect((exchanges[0] as HTMLSelectElement).value).toBe("binance");
    });
  }, 15000);

  it("clears polling timers when hidden", async () => {
    setup();
    const clearSpy = vi.spyOn(window, "clearTimeout");
    function Harness({ hidden }: { hidden: boolean }) {
      return (
        <Activity mode={hidden ? "hidden" : "visible"}>
          <ArbitrageTradingView />
        </Activity>
      );
    }
    const view = render(<Harness hidden={false} />);
    await waitFor(() => expect(mocks.fetchArbitrageCombinations).toHaveBeenCalled());
    const before = clearSpy.mock.calls.length;
    view.rerender(<Harness hidden />);
    expect(clearSpy.mock.calls.length).toBeGreaterThan(before);
    clearSpy.mockRestore();
  }, 15000);

  it("polls every 4s until a response contains closing, then 1s until closed", async () => {
    vi.useFakeTimers();
    setup();
    const closing = {
      ...combination,
      status: "closing" as const,
      updatedAt: "2026-08-22T10:02:00Z",
    };
    const closed = {
      ...closing,
      status: "closed" as const,
      updatedAt: "2026-08-22T10:03:00Z",
      closedAt: "2026-08-22T10:03:00Z",
    };
    let runningItems: ArbitrageCombination[] = [combination];
    let closedItems: ArbitrageCombination[] = [];
    mocks.fetchArbitrageCombinations.mockImplementation(async (view: string) => ({
      items: view === "running" ? runningItems : closedItems,
      total: view === "running" ? runningItems.length : closedItems.length,
      nextCursor: "",
    }));
    render(<ArbitrageTradingView />);
    await flushMicrotasks();
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(2);

    await advanceTimers(3999);
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(2);
    runningItems = [closing];
    await advanceTimers(1);
    await flushMicrotasks();
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(4);

    await advanceTimers(999);
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(4);
    await advanceTimers(1);
    await flushMicrotasks();
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(6);

    runningItems = [];
    closedItems = [closed];
    await advanceTimers(1000);
    await flushMicrotasks();
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(8);

    await advanceTimers(3999);
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(8);
    await advanceTimers(1);
    await flushMicrotasks();
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(10);
  });

  it("does not overlap combination polls while one request is in flight", async () => {
    vi.useFakeTimers();
    setup();
    let release!: () => void;
    const hang = new Promise<void>((resolve) => {
      release = resolve;
    });
    mocks.fetchArbitrageCombinations.mockImplementation(async () => {
      await hang;
      return { items: [], total: 0, nextCursor: "" };
    });
    render(<ArbitrageTradingView />);
    await flushMicrotasks();
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(2);
    await advanceTimers(10_000);
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(2);
    release();
    await flushMicrotasks();
  });

  it("uses 4s after a failed poll and 1s after a later success that still has closing", async () => {
    vi.useFakeTimers();
    setup();
    const closing = {
      ...combination,
      status: "closing" as const,
      updatedAt: "2026-08-22T10:02:00Z",
    };
    let failNext = true;
    mocks.fetchArbitrageCombinations.mockImplementation(async (view: string) => {
      if (failNext) {
        throw new Error("trader service unavailable");
      }
      return {
        items: view === "running" ? [closing] : [],
        total: view === "running" ? 1 : 0,
        nextCursor: "",
      };
    });
    render(<ArbitrageTradingView />);
    await flushMicrotasks();
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(2);

    failNext = false;
    await advanceTimers(3999);
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(2);
    await advanceTimers(1);
    await flushMicrotasks();
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(4);

    await advanceTimers(999);
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(4);
    await advanceTimers(1);
    await flushMicrotasks();
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(6);
  });

  it("stops combination polling while hidden and polls immediately when visible", async () => {
    vi.useFakeTimers();
    setup();
    let hidden = false;
    vi.spyOn(document, "visibilityState", "get").mockImplementation(() =>
      hidden ? "hidden" : "visible",
    );
    render(<ArbitrageTradingView />);
    await flushMicrotasks();
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(2);

    hidden = true;
    document.dispatchEvent(new Event("visibilitychange"));
    await advanceTimers(8000);
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(2);

    hidden = false;
    document.dispatchEvent(new Event("visibilitychange"));
    await flushMicrotasks();
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(4);
  });

  it("applies the close response immediately without an extra list fetch, then polls in 1s", async () => {
    vi.useFakeTimers();
    setup();
    vi.stubGlobal("confirm", () => true);
    render(<ArbitrageTradingView />);
    await flushMicrotasks();
    await advanceTimers(0);
    await flushMicrotasks();
    expect(screen.getByRole("button", { name: "关闭" })).toBeTruthy();
    const before = mocks.fetchArbitrageCombinations.mock.calls.length;
    fireEvent.click(screen.getByRole("button", { name: "关闭" }));
    await flushMicrotasks();
    expect(mocks.closeArbitrageCombination).toHaveBeenCalledWith("arb-1");
    expect(screen.getByRole("button", { name: "关闭中" })).toBeTruthy();
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(before);
    await advanceTimers(999);
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(before);
    await advanceTimers(1);
    await flushMicrotasks();
    expect(mocks.fetchArbitrageCombinations.mock.calls.length).toBe(before + 2);
  });

  it("keeps local closing when the first fast poll fails, then uses 4s until a closed snapshot arrives", async () => {
    vi.useFakeTimers();
    setup();
    vi.stubGlobal("confirm", () => true);
    const closed = {
      ...combination,
      status: "closed" as const,
      updatedAt: "2026-08-22T10:03:00Z",
      closedAt: "2026-08-22T10:03:00Z",
    };
    let failPoll = false;
    mocks.fetchArbitrageCombinations.mockImplementation(async (view: string) => {
      if (failPoll) {
        throw new Error("trader service unavailable");
      }
      return {
        items: view === "running" ? [combination] : [],
        total: view === "running" ? 1 : 0,
        nextCursor: "",
      };
    });
    render(<ArbitrageTradingView />);
    await flushMicrotasks();
    fireEvent.click(screen.getByRole("button", { name: "关闭" }));
    await flushMicrotasks();
    expect(screen.getByRole("button", { name: "关闭中" })).toBeTruthy();
    const afterClose = mocks.fetchArbitrageCombinations.mock.calls.length;

    failPoll = true;
    await advanceTimers(1000);
    await flushMicrotasks();
    expect(mocks.fetchArbitrageCombinations.mock.calls.length).toBe(
      afterClose + 2,
    );
    expect(screen.getByRole("button", { name: "关闭中" })).toBeTruthy();
    expect(screen.queryByText("关闭失败")).toBeNull();

    failPoll = false;
    mocks.fetchArbitrageCombinations.mockImplementation(async (view: string) => ({
      items: view === "closed" ? [closed] : [],
      total: view === "closed" ? 1 : 0,
      nextCursor: "",
    }));
    await advanceTimers(3999);
    expect(mocks.fetchArbitrageCombinations.mock.calls.length).toBe(
      afterClose + 2,
    );
    await advanceTimers(1);
    await flushMicrotasks();
    fireEvent.click(screen.getByRole("button", { name: "已关闭 1" }));
    expect(screen.getByText("arb-1")).toBeTruthy();
    expect(screen.getByText("已关闭")).toBeTruthy();

    const afterClosed = mocks.fetchArbitrageCombinations.mock.calls.length;
    await advanceTimers(3999);
    expect(mocks.fetchArbitrageCombinations.mock.calls.length).toBe(afterClosed);
    await advanceTimers(1);
    await flushMicrotasks();
    expect(mocks.fetchArbitrageCombinations.mock.calls.length).toBe(
      afterClosed + 2,
    );
  });

  it("schedules a 1s poll after close even if a request was already in flight", async () => {
    vi.useFakeTimers();
    setup();
    vi.stubGlobal("confirm", () => true);
    let pendingHang: Promise<void> | null = null;
    mocks.fetchArbitrageCombinations.mockImplementation(async (view: string) => {
      if (pendingHang) await pendingHang;
      return {
        items: view === "running" ? [combination] : [],
        total: view === "running" ? 1 : 0,
        nextCursor: "",
      };
    });
    render(<ArbitrageTradingView />);
    await flushMicrotasks();
    expect(screen.getByRole("button", { name: "关闭" })).toBeTruthy();
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(2);

    let releaseHang!: () => void;
    pendingHang = new Promise<void>((resolve) => {
      releaseHang = resolve;
    });
    await advanceTimers(4000);
    await flushMicrotasks();
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(4);

    fireEvent.click(screen.getByRole("button", { name: "关闭" }));
    await flushMicrotasks();
    expect(screen.getByRole("button", { name: "关闭中" })).toBeTruthy();
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(4);

    releaseHang();
    await flushMicrotasks();
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(4);
    await advanceTimers(999);
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(4);
    await advanceTimers(1);
    await flushMicrotasks();
    expect(mocks.fetchArbitrageCombinations).toHaveBeenCalledTimes(6);
  });
});

describe("resolveBasisSpreadQuery", () => {
  it("maps same-venue spot/perpetual and cross-venue perpetual pairs", () => {
    const spot = { ...instrument, contractType: "spot" as const };
    const sameVenue = resolveBasisSpreadQuery(spot, instrument);
    expect(sameVenue).toEqual({
      venue: "binance",
      baseAsset: "BTC",
      quoteAsset: "USDT",
      formula: "Perpetual Ask / Spot Ask - 1 · BTC/USDT",
    });

    const okx = { ...instrument, id: 8, exchange: "okx" };
    expect(resolveBasisSpreadQuery(instrument, okx)).toEqual({
      venue: "okx",
      compareVenue: "binance",
      baseAsset: "BTC",
      quoteAsset: "USDT",
      venueSymbol: "BTCUSDT",
      compareVenueSymbol: "BTCUSDT",
      formula: "OKX Ask / BINANCE Ask - 1 · BTC/USDT",
    });
    expect(resolveBasisSpreadQuery(spot, okx)).toEqual({
      venue: "okx",
      compareVenue: "binance",
      baseAsset: "BTC",
      quoteAsset: "USDT",
      venueSymbol: "BTCUSDT",
      compareVenueSymbol: "BTCUSDT",
      formula: "OKX Ask / BINANCE Ask - 1 · BTC/USDT",
    });
    expect(resolveArbitrageYieldMode(spot, instrument)).toBe("basis");
    expect(resolveArbitrageYieldMode(instrument, okx)).toBe("cross");
  });

  it("maps every perpetual USDC venue against USDT venues", () => {
    const hyperliquid = {
      ...instrument,
      id: 9,
      exchange: "hyperliquid",
      exchangeSymbol: "BTC",
      quoteAsset: "USDC",
      settleAsset: "USDC",
    };
    expect(resolveBasisSpreadQuery(hyperliquid, instrument)).toEqual({
      venue: "binance",
      compareVenue: "hyperliquid",
      baseAsset: "BTC",
      quoteAsset: "USDT",
      venueSymbol: "BTCUSDT",
      compareVenueSymbol: "BTCUSDC",
      formula: "BINANCE Ask / HYPERLIQUID Ask - 1 · BTC/USDT",
    });
    expect(
      resolveBasisSpreadQuery(
        { ...hyperliquid, contractType: "spot" as const },
        instrument,
      ),
    ).toBeNull();

    const lighter = {
      ...hyperliquid,
      id: 10,
      exchange: "lighter",
      exchangeSymbol: "BTC",
    };
    const aster = {
      ...instrument,
      id: 11,
      exchange: "aster",
      exchangeSymbol: "BTCUSDT",
    };
    expect(resolveBasisSpreadQuery(lighter, instrument)).not.toBeNull();
    expect(resolveBasisSpreadQuery(lighter, aster)).toMatchObject({
      venue: "aster",
      compareVenue: "lighter",
      quoteAsset: "USDT",
    });
    expect(
      resolveBasisSpreadQuery(lighter, {
        ...hyperliquid,
      }),
    ).toMatchObject({
      venue: "hyperliquid",
      compareVenue: "lighter",
      quoteAsset: "USDC",
    });
  });

  it("maps Hyperliquid HIP-3 venueSymbol with the xyz ClickHouse prefix", () => {
    const hyperliquidHip3 = {
      ...instrument,
      id: 12,
      exchange: "hyperliquid",
      exchangeSymbol: "xyz:HYUNDAI",
      baseAsset: "HYUNDAI",
      quoteAsset: "USDC",
      settleAsset: "USDC",
    };
    const binance = {
      ...instrument,
      id: 13,
      exchange: "binance",
      exchangeSymbol: "HYUNDAIUSDT",
      baseAsset: "HYUNDAI",
      quoteAsset: "USDT",
    };
    expect(resolveBasisSpreadQuery(binance, hyperliquidHip3)).toEqual({
      venue: "hyperliquid",
      compareVenue: "binance",
      baseAsset: "HYUNDAI",
      quoteAsset: "USDT",
      venueSymbol: "XYZHYUNDAIUSDC",
      compareVenueSymbol: "HYUNDAIUSDT",
      formula: "HYPERLIQUID Ask / BINANCE Ask - 1 · HYUNDAI/USDT",
    });
  });
});

describe("sortArbitrageCombinationsByCreatedAt", () => {
  it("does not mutate the response and uses id as the tie-breaker", () => {
    const input: ArbitrageCombination[] = [
      { ...combination, id: "b" },
      { ...combination, id: "a" },
    ];
    const sorted = sortArbitrageCombinationsByCreatedAt(input);
    expect(sorted.map((item) => item.id)).toEqual(["a", "b"]);
    expect(input.map((item) => item.id)).toEqual(["b", "a"]);
  });
});

describe("arbitrage yield helpers", () => {
  it("annualizes basis averages and matches funding rows", () => {
    expect(annualizeBasisAvgBps(11.25, "24h")).toBeCloseTo(41.0625);
    expect(annualizeBasisAvgBps(11.25, "7d")).toBeCloseTo(5.86607, 5);
    const binance = fundingRate("Binance", "BTCUSDT", 0.02, 0.1);
    const okx = fundingRate("OKX", "BTC-USDT-SWAP", 0.05, 0.21);
    expect(matchFundingOpportunity([binance, okx], instrument)?.exchange).toBe(
      "Binance",
    );
    const yield_ = crossExchangeWindowYield(binance, okx);
    expect(yield_?.value24h).toBeCloseTo(0.03 * 365);
    expect(yield_?.value7d).toBeCloseTo((0.11 * 365) / 7);
  });

  it("annualizes dual-perp 24h funding differential and perpetual-only for spot-perp", () => {
    const binance = fundingRate("Binance", "BTCUSDT", 0.02, 0.1);
    const okx = fundingRate("OKX", "BTC-USDT-SWAP", 0.05, 0.21);
    expect(combinationFundingAnnualized24h(combination, fundingResolverFromItems([binance, okx]))).toEqual(
      {
        value: expect.closeTo(0.03 * 365),
        complete: true,
      },
    );
    expect(combinationFundingAnnualized24h(combination, fundingResolverFromItems([binance]))).toBeNull();
    expect(
      combinationFundingAnnualized24h(
        combination,
        fundingResolverFromItems([
          { ...binance, history24hComplete: false },
          okx,
        ]),
      ),
    ).toEqual({
      value: expect.closeTo(0.03 * 365),
      complete: false,
    });
    const spotPerp = {
      ...combination,
      legA: { ...combination.legA, contractType: "spot" as const },
    };
    expect(combinationFundingAnnualized24h(spotPerp, fundingResolverFromItems([binance, okx]))).toEqual({
      value: expect.closeTo(0.05 * 365),
      complete: true,
    });
    const bothSpot = {
      ...combination,
      legA: { ...combination.legA, contractType: "spot" as const },
      legB: { ...combination.legB, contractType: "spot" as const },
    };
    expect(combinationFundingAnnualized24h(bothSpot, fundingResolverFromItems([binance, okx]))).toBeNull();
  });

  it("uses the requestKey row instead of first same-venue fallback", () => {
    const exact = fundingRate("Binance", "BTCUSDT", 0.02, 0.1);
    const assigned = fundingRate("Binance", "BTCUSDT_QM", 0.5, 0.5);
    const okx = fundingRate("OKX", "BTC-USDT-SWAP", 0.05, 0.21);
    const missingSymbolCombo = {
      ...combination,
      legA: { ...combination.legA, exchangeSymbol: "BTCUSDT_MISSING" },
    };
    const byKey = new Map([
      [fundingRequestKey(exact), exact],
      [fundingRequestKey(missingSymbolCombo.legA), assigned],
      [fundingRequestKey(okx), okx],
    ]);
    const resolve = (
      leg: Pick<typeof combination.legA, "exchange" | "exchangeSymbol">,
    ) => byKey.get(fundingRequestKey(leg));
    expect(
      matchFundingOpportunity([exact, assigned, okx], missingSymbolCombo.legA)
        ?.exchangeSymbol,
    ).toBe("BTCUSDT");
    expect(
      combinationFundingAnnualized24h(missingSymbolCombo, resolve),
    ).toEqual({
      value: expect.closeTo((0.05 - 0.5) * 365),
      complete: true,
    });
  });
});
