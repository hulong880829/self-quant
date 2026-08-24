// @vitest-environment jsdom

import * as React from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  fetchTradingAccounts: vi.fn(),
  fetchTraderInstruments: vi.fn(),
  fetchArbitrageCombinations: vi.fn(),
  fetchArbitrageCombination: vi.fn(),
  createArbitrageCombination: vi.fn(),
  closeArbitrageCombination: vi.fn(),
  fetchBasisSpreadHistory: vi.fn(),
  fetchFundingRates: vi.fn(),
}));

vi.mock("@/lib/api/accounts", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api/accounts")>("@/lib/api/accounts");
  return { ...actual, fetchTradingAccounts: mocks.fetchTradingAccounts };
});
vi.mock("@/lib/api/trader", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api/trader")>("@/lib/api/trader");
  return { ...actual, fetchTraderInstruments: mocks.fetchTraderInstruments };
});
vi.mock("@/lib/api/arbitrage", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api/arbitrage")>("@/lib/api/arbitrage");
  return {
    ...actual,
    fetchArbitrageCombinations: mocks.fetchArbitrageCombinations,
    fetchArbitrageCombination: mocks.fetchArbitrageCombination,
    createArbitrageCombination: mocks.createArbitrageCombination,
    closeArbitrageCombination: mocks.closeArbitrageCombination,
  };
});
vi.mock("@/lib/api/spread", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api/spread")>("@/lib/api/spread");
  return { ...actual, fetchBasisSpreadHistory: mocks.fetchBasisSpreadHistory };
});
vi.mock("@/lib/api/funding", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api/funding")>("@/lib/api/funding");
  return { ...actual, fetchFundingRates: mocks.fetchFundingRates };
});

import {
  annualizeBasisAvgBps,
  ArbitrageTradingView,
  crossExchangeWindowYield,
  matchFundingOpportunity,
  resolveArbitrageYieldMode,
  resolveBasisSpreadQuery,
} from "./arbitrage-trading";
import type { FundingOpportunity } from "@/types/market";

const accounts = [
  {
    id: 3, productName: "核心账户", exchange: "Binance", exchangeSlug: "binance",
    accountName: "Binance Main", apiKeyMasked: "", hasPassphrase: false,
    createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z",
  },
  {
    id: 4, productName: "核心账户", exchange: "OKX", exchangeSlug: "okx",
    accountName: "OKX Main", apiKeyMasked: "", hasPassphrase: true,
    createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z",
  },
  {
    id: 5, productName: "核心账户", exchange: "Binance", exchangeSlug: "binance",
    accountName: "Binance Sub", apiKeyMasked: "", hasPassphrase: false,
    createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z",
  },
];

const instrument = {
    id: 7, exchange: "binance", contractType: "perpetual" as const, exchangeSymbol: "BTCUSDT",
    baseAsset: "BTC", quoteAsset: "USDT", settleAsset: "USDT",
    contractSize: "1", priceTick: "0.1", quantityStep: "0.001",
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
): FundingOpportunity {
  return {
    id: `${exchange}-${exchangeSymbol}`,
    exchange,
    exchangeSymbol,
    symbol: "BTCUSDT",
    baseAsset: "BTC",
    quoteAsset: "USDT",
    positionQuantity: 1,
    positionNotional: 1000,
    dailyVolume: 1_000_000,
    annualizedRate: 10,
    currentFundingRate: 0.01,
    nextFundingRate: 0.01,
    settlementIntervalHours: 8,
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

const combination = {
  id: "arb-1",
  productName: "核心账户",
  status: "running",
  legA: {
    tradingAccountId: 3, accountName: "Binance Main", exchange: "binance",
    instrumentId: 7, exchangeSymbol: "BTCUSDT", baseAsset: "BTC", quoteAsset: "USDT",
  },
  legB: {
    tradingAccountId: 4, accountName: "OKX Main", exchange: "okx",
    instrumentId: 7, exchangeSymbol: "BTC-USDT-SWAP", baseAsset: "BTC", quoteAsset: "USDT",
  },
  askThresholdBps: "12",
  bidThresholdBps: "-8",
  targetNotional: "10000",
  positionNotional: "2500",
  cumulativeTurnoverNotional: "2500",
  consecutiveFailures: 0,
  nextRetryAt: "",
  positionUncertain: false,
  orderNotional: "500",
  maxDeltaNotional: "100",
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

function setup() {
  mocks.fetchTradingAccounts.mockResolvedValue(accounts);
  mocks.fetchTraderInstruments.mockImplementation(
    async (accountID: number, contractType: "spot" | "perpetual") => {
      const binance = accountID === 3 || accountID === 5;
      return [{
        ...instrument,
        id: accountID === 3 ? 7 : accountID === 5 ? 9 : 8,
        exchange: binance ? "binance" : "okx",
        contractType,
        exchangeSymbol: binance
          ? "BTCUSDT"
          : contractType === "spot" ? "BTC-USDT" : "BTC-USDT-SWAP",
      }];
    },
  );
  mocks.fetchBasisSpreadHistory.mockImplementation(async (_venue, _base, _quote, range) => ({
    status: "updated",
    history: { ...spreadHistory, range },
    etag: `"spread-${range}"`,
  }));
  mocks.fetchFundingRates.mockResolvedValue({
    status: "updated",
    snapshot: {
      data: [
        fundingRate("Binance", "BTCUSDT", 0.02, 0.10),
        fundingRate("OKX", "BTC-USDT-SWAP", 0.05, 0.21),
      ],
      meta: { total: 2, snapshotVersion: "1", serverTime: "2026-08-22T10:01:00Z" },
      hasStaleSources: false,
    },
    etag: '"funding-1"',
  });
  mocks.fetchArbitrageCombinations.mockImplementation(async (view: string) => ({
    items: view === "running" ? [combination] : [],
    total: view === "running" ? 1 : 0,
    nextCursor: "",
  }));
  mocks.fetchArbitrageCombination.mockResolvedValue({
    ...combination,
    recentExecutions: [],
    recentEvents: [],
  });
  mocks.createArbitrageCombination.mockResolvedValue(combination);
  mocks.closeArbitrageCombination.mockResolvedValue({ ...combination, status: "closing" });
}

describe("ArbitrageTradingView", () => {
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
    vi.unstubAllGlobals();
  });

  it("polls both views and expands detail directly below the selected row", async () => {
    setup();
    render(<ArbitrageTradingView />);
    expect(await screen.findByText("+13.25 bps", {}, { timeout: 15000 })).toBeTruthy();
    await waitFor(() => expect(mocks.fetchBasisSpreadHistory).toHaveBeenCalled());
    expect(await screen.findByRole("img", { name: "Best Ask 价差走势" })).toBeTruthy();
    expect(screen.getByText("OKX Ask / BINANCE Ask - 1 · BTC/USDT")).toBeTruthy();
    expect(screen.getByText("+12.50 bps")).toBeTruthy();
    expect(screen.getByText("+11.25 bps")).toBeTruthy();
    expect(screen.getByText("95.0%")).toBeTruthy();
    expect(await screen.findByText("24H 窗口年化")).toBeTruthy();
    expect(screen.getByText("7D 窗口年化")).toBeTruthy();
    expect(screen.getByText("+11.0%")).toBeTruthy();
    expect(screen.getByText("+5.7%")).toBeTruthy();
    expect(mocks.fetchFundingRates).toHaveBeenCalled();
    expect(screen.getByText("ASK 12 bps")).toBeTruthy();
    expect(screen.getByText("BID -8 bps")).toBeTruthy();
    expect(mocks.fetchBasisSpreadHistory).toHaveBeenCalledWith(
      "okx",
      "BTC",
      "USDT",
      "24h",
      null,
      expect.any(AbortSignal),
      "binance",
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
    expect(mocks.fetchArbitrageCombination).toHaveBeenCalledWith("arb-1");
    fireEvent.click(screen.getByRole("button", { name: "收起" }));
    await waitFor(() => expect(screen.queryByText("买 Leg A / 卖 Leg B")).toBeNull());
  }, 15000);

  it("requires confirmation before closing", async () => {
    setup();
    const confirm = vi.fn(() => false);
    vi.stubGlobal("confirm", confirm);
    render(<ArbitrageTradingView />);
    await screen.findByText("+13.25 bps");
    fireEvent.click(screen.getByRole("button", { name: "关闭" }));
    expect(confirm).toHaveBeenCalled();
    expect(mocks.closeArbitrageCombination).not.toHaveBeenCalled();

    confirm.mockReturnValue(true);
    fireEvent.click(screen.getByRole("button", { name: "关闭" }));
    await waitFor(() => expect(mocks.closeArbitrageCombination).toHaveBeenCalledWith("arb-1"));
  });

  it("keeps create enabled when Ask threshold is negative", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await screen.findByText(/BTC\/USDT 配对有效/);
    const askInput = screen.getByText("Ask Threshold").closest("label")?.querySelector("input");
    expect(askInput).toBeTruthy();
    fireEvent.change(askInput!, { target: { value: "-70" } });
    expect((screen.getByRole("button", { name: "创建套利组合" }) as HTMLButtonElement).disabled)
      .toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    await waitFor(() => expect(mocks.createArbitrageCombination).toHaveBeenCalledWith(
      expect.objectContaining({
        askThresholdBps: "-70",
        bidThresholdBps: "-8",
      }),
    ));
  });

  it("creates a bidirectional combination without fixed leg sides", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await screen.findByText(/BTC\/USDT 配对有效/);
    await waitFor(() =>
      expect((screen.getByRole("button", { name: "创建套利组合" }) as HTMLButtonElement).disabled)
        .toBe(false),
    );
    expect(screen.queryByText("交易方向")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "创建套利组合" }));
    await waitFor(() => expect(mocks.createArbitrageCombination).toHaveBeenCalledWith(
      expect.objectContaining({
        legAAccountId: 3,
        legBAccountId: 4,
        askThresholdBps: "12",
        bidThresholdBps: "-8",
        preferredLeg: "a",
        executionMode: "maker_then_hedge",
      }),
    ));
  });

  it("does not request unsupported spot-to-spot history", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await screen.findByText("+13.25 bps", {}, { timeout: 15000 });
    await waitFor(() => expect(mocks.fetchBasisSpreadHistory).toHaveBeenCalled());
    expect(await screen.findByRole("img", { name: "Best Ask 价差走势" })).toBeTruthy();
    mocks.fetchBasisSpreadHistory.mockClear();
    const productTypes = screen.getAllByRole("combobox", { name: "产品类型" });
    fireEvent.change(productTypes[0]!, { target: { value: "spot" } });
    fireEvent.change(productTypes[1]!, { target: { value: "spot" } });
    expect(
      await screen.findByText("当前组合类型暂无 Best Ask 价差历史"),
    ).toBeTruthy();
    expect(mocks.fetchBasisSpreadHistory).not.toHaveBeenCalled();
  }, 15000);

  it("shows basis annualized yield for same-venue spot/perpetual pairs", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await screen.findByText("+13.25 bps", {}, { timeout: 15000 });
    const exchanges = screen.getAllByRole("combobox", { name: "交易所" });
    fireEvent.change(exchanges[1]!, { target: { value: "binance" } });
    const productTypes = screen.getAllByRole("combobox", { name: "产品类型" });
    fireEvent.change(productTypes[0]!, { target: { value: "spot" } });
    expect(await screen.findByText("Perpetual Ask / Spot Ask - 1 · BTC/USDT")).toBeTruthy();
    expect(await screen.findByText("24H 差值年化")).toBeTruthy();
    expect(screen.getByText("7D 差值年化")).toBeTruthy();
    expect(screen.getByText("+41.1%")).toBeTruthy();
    expect(screen.getByText("+5.9%")).toBeTruthy();
  }, 15000);

  it("blocks create when max unhedged exposure exceeds order notional", async () => {
    setup();
    render(<ArbitrageTradingView />);
    await screen.findByText("+13.25 bps", {}, { timeout: 15000 });
    const maxDelta = screen.getByText("最大未对冲敞口").closest("label")?.querySelector("input");
    expect(maxDelta).toBeTruthy();
    fireEvent.change(maxDelta!, { target: { value: "600" } });
    expect(screen.getByText("最大未对冲敞口不能大于单笔订单金额")).toBeTruthy();
    expect((screen.getByRole("button", { name: "创建套利组合" }) as HTMLButtonElement).disabled)
      .toBe(true);
  }, 15000);
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
      formula: "OKX Ask / BINANCE Ask - 1 · BTC/USDT",
    });
    expect(resolveArbitrageYieldMode(spot, instrument)).toBe("basis");
    expect(resolveArbitrageYieldMode(instrument, okx)).toBe("cross");
  });
});

describe("arbitrage yield helpers", () => {
  it("annualizes basis averages and matches funding rows", () => {
    expect(annualizeBasisAvgBps(11.25, "24h")).toBeCloseTo(41.0625);
    expect(annualizeBasisAvgBps(11.25, "7d")).toBeCloseTo(5.86607, 5);
    const binance = fundingRate("Binance", "BTCUSDT", 0.02, 0.10);
    const okx = fundingRate("OKX", "BTC-USDT-SWAP", 0.05, 0.21);
    expect(matchFundingOpportunity([binance, okx], instrument)?.exchange).toBe("Binance");
    const yield_ = crossExchangeWindowYield(binance, okx);
    expect(yield_?.value24h).toBeCloseTo(0.03 * 365);
    expect(yield_?.value7d).toBeCloseTo((0.11 * 365) / 7);
  });
});
