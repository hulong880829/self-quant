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

import { ArbitrageTradingView } from "./arbitrage-trading";

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
];

const instruments = [
  {
    id: 7, exchange: "binance", contractType: "perpetual", exchangeSymbol: "BTCUSDT",
    baseAsset: "BTC", quoteAsset: "USDT", settleAsset: "USDT",
    contractSize: "1", priceTick: "0.1", quantityStep: "0.001",
  },
];

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
  completedNotional: "2500",
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
  mocks.fetchTraderInstruments.mockResolvedValue(instruments);
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
    expect(await screen.findByText("+13.25 bps")).toBeTruthy();
    expect(screen.getByRole("img", { name: "价差走势静态预览" })).toBeTruthy();
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
  });

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

  it("creates a bidirectional combination without fixed leg sides", async () => {
    setup();
    render(<ArbitrageTradingView />);
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
});
