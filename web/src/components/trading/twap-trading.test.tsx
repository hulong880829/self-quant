// @vitest-environment jsdom

import * as React from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const apiMocks = vi.hoisted(() => ({
  fetchTradingAccounts: vi.fn(),
  fetchTraderInstruments: vi.fn(),
  fetchTraderTwapPage: vi.fn(),
  createTraderTwap: vi.fn(),
  fetchTraderTwap: vi.fn(),
  fetchTraderTwapOrders: vi.fn(),
  cancelTraderTwap: vi.fn(),
}));

vi.mock("@/lib/api/accounts", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api/accounts")>("@/lib/api/accounts");
  return { ...actual, fetchTradingAccounts: apiMocks.fetchTradingAccounts };
});

vi.mock("@/lib/api/trader", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api/trader")>("@/lib/api/trader");
  return {
    ...actual,
    fetchTraderInstruments: apiMocks.fetchTraderInstruments,
    fetchTraderTwapPage: apiMocks.fetchTraderTwapPage,
    createTraderTwap: apiMocks.createTraderTwap,
    fetchTraderTwap: apiMocks.fetchTraderTwap,
    fetchTraderTwapOrders: apiMocks.fetchTraderTwapOrders,
    cancelTraderTwap: apiMocks.cancelTraderTwap,
  };
});

import { TwapTradingView } from "./twap-trading";

const accounts = [
  {
    id: 3,
    productName: "核心账户",
    exchange: "Binance",
    exchangeSlug: "binance",
    accountName: "Binance Main",
    apiKeyMasked: "abc****",
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
    apiKeyMasked: "def****",
    hasPassphrase: true,
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
  },
];

const btc = {
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

const runningTwap = {
  id: "twap-1",
  tradingAccountId: 3,
  productName: "核心账户",
  exchange: "binance",
  instrumentId: 7,
  exchangeSymbol: "BTCUSDT",
  baseAsset: "BTC",
  quoteAsset: "USDT",
  side: "buy",
  orderType: "maker",
  totalQty: "2",
  filledQty: "0.5",
  averagePrice: "60000",
  startAt: "2026-08-19T00:00:00Z",
  endAt: "2026-08-19T01:00:00Z",
  intervalSeconds: 60,
  maxQty: "0.1",
  limitPrice: "61000",
  orderTimeoutSeconds: 30,
  status: "running",
  currentSlice: 0,
  currentAttempt: 0,
};

function setup() {
  apiMocks.fetchTradingAccounts.mockResolvedValue(accounts);
  apiMocks.fetchTraderInstruments.mockResolvedValue([btc]);
  apiMocks.fetchTraderTwapPage.mockResolvedValue({ items: [runningTwap], nextCursor: "" });
  apiMocks.createTraderTwap.mockResolvedValue(runningTwap);
  apiMocks.fetchTraderTwap.mockResolvedValue(runningTwap);
  apiMocks.fetchTraderTwapOrders.mockResolvedValue([{
    id: "child-1",
    createdAt: "2026-08-19T00:01:00Z",
    side: "buy",
    orderType: "limit",
    quantity: "0.1",
    filledQuantity: "0.1",
    baseAsset: "BTC",
    status: "filled",
  }]);
  apiMocks.cancelTraderTwap.mockResolvedValue({ ...runningTwap, status: "canceled" });
}

describe("TwapTradingView", () => {
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
  });

  it("uses product, exchange and account as a dependent selection chain", async () => {
    setup();
    render(<TwapTradingView />);
    await waitFor(() => {
      expect(screen.getByDisplayValue("Binance Main")).toBeTruthy();
      expect(apiMocks.fetchTraderInstruments).toHaveBeenCalledWith(3, "perpetual");
    });

    fireEvent.change(screen.getByDisplayValue("Binance"), { target: { value: "okx" } });
    await waitFor(() => {
      expect(screen.getByDisplayValue("OKX Main")).toBeTruthy();
      expect(apiMocks.fetchTraderInstruments).toHaveBeenCalledWith(4, "perpetual");
    });
  });

  it("lists all owner accounts while counting only the selected account", async () => {
    setup();
    render(<TwapTradingView />);
    await waitFor(() => {
      expect(apiMocks.fetchTraderTwapPage).toHaveBeenCalledWith(
        undefined,
        expect.objectContaining({ view: "running", limit: 50 }),
      );
      expect(apiMocks.fetchTraderTwapPage).toHaveBeenCalledWith(
        3,
        expect.objectContaining({ view: "running", limit: 5 }),
      );
    });
    expect(screen.getAllByText("Binance Main").length).toBeGreaterThan(0);
  });

  it("validates parameters and creates only after confirmation", async () => {
    setup();
    render(<TwapTradingView />);
    await waitFor(() => expect((screen.getByRole("button", { name: "创建 TWAP 计划" }) as HTMLButtonElement).disabled).toBe(false));

    fireEvent.click(screen.getByRole("button", { name: "创建 TWAP 计划" }));
    expect(screen.getByText("请输入有效的总数量")).toBeTruthy();

    fireEvent.change(screen.getByRole("textbox", { name: "总数量" }), { target: { value: "2" } });
    fireEvent.change(screen.getByRole("spinbutton", { name: "执行间隔" }), { target: { value: "120" } });
    fireEvent.change(screen.getByRole("spinbutton", { name: "Maker 委托超时" }), { target: { value: "45" } });
    fireEvent.click(screen.getByRole("button", { name: "创建 TWAP 计划" }));
    expect(await screen.findByRole("button", { name: "确认创建" })).toBeTruthy();
    expect(apiMocks.createTraderTwap).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "确认创建" }));
    await waitFor(() => expect(apiMocks.createTraderTwap).toHaveBeenCalledWith(expect.objectContaining({
      tradingAccountId: 3,
      instrumentId: 7,
      totalQty: "2",
      intervalSeconds: 120,
      orderType: "maker",
      orderTimeoutSeconds: 45,
    })));
  });

  it("shows progress, details, child orders and cancellation", async () => {
    setup();
    render(<TwapTradingView />);
    expect(await screen.findByText("25.0%")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "详情" }));
    expect(await screen.findByText("filled")).toBeTruthy();
    expect(apiMocks.fetchTraderTwapOrders).toHaveBeenCalledWith("twap-1");

    fireEvent.click(screen.getByRole("button", { name: "取消计划" }));
    await waitFor(() => expect(apiMocks.cancelTraderTwap).toHaveBeenCalledWith("twap-1"));
  });

  it("hides maker timeout for market child orders", async () => {
    setup();
    render(<TwapTradingView />);
    await waitFor(() => expect(screen.getByRole("button", { name: "Market 市价" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Market 市价" }));
    expect(screen.queryByRole("spinbutton", { name: "Maker 委托超时" })).toBeNull();
  });
});
