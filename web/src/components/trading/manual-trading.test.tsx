// @vitest-environment jsdom

import * as React from "react";
import { Activity } from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const apiMocks = vi.hoisted(() => ({
  fetchTradingAccounts: vi.fn(),
  inspectTradingReadiness: vi.fn(),
  fetchTraderInstruments: vi.fn(),
  fetchTraderOrderPage: vi.fn(),
  placeTraderOrder: vi.fn(),
  cancelTraderOrder: vi.fn(),
}));

vi.mock("@/lib/api/accounts", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api/accounts")>("@/lib/api/accounts");
  return {
    ...actual,
    fetchTradingAccounts: apiMocks.fetchTradingAccounts,
    inspectTradingReadiness: apiMocks.inspectTradingReadiness,
  };
});

vi.mock("@/lib/api/trader", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api/trader")>("@/lib/api/trader");
  return {
    ...actual,
    fetchTraderInstruments: apiMocks.fetchTraderInstruments,
    fetchTraderInstrumentCatalog: async (
      accountId: number,
      contractType: "spot" | "perpetual",
    ) => ({
      items: await apiMocks.fetchTraderInstruments(accountId, contractType),
      capabilities: {
        products: ["spot", "perpetual"],
        quoteAssets: [],
        timeInForce: ["GTC", "IOC", "POST_ONLY"],
        postOnly: true,
        reduceOnly: true,
        makerTwap: true,
        privateOrderStream: false,
        oneWayOnly: false,
      },
    }),
    fetchTraderOrderPage: apiMocks.fetchTraderOrderPage,
    placeTraderOrder: apiMocks.placeTraderOrder,
    cancelTraderOrder: apiMocks.cancelTraderOrder,
  };
});

import { ManualTradingView } from "./manual-trading";

const binanceAccount = {
  id: 3,
  productName: "核心账户",
  exchange: "Binance",
  exchangeSlug: "binance",
  accountName: "Binance UTA",
    hasPassphrase: false,
  credentialsPresent: true,
  credentialsVerified: true,
  tradingMode: "cex",
  tradingReady: true,
  tradingStatus: "ready",
  tradingUnavailableCode: "",
  tradingUnavailableReason: "",
  createdAt: "2026-01-01T00:00:00Z",
  updatedAt: "2026-01-01T00:00:00Z",
};

const okxAccount = {
  id: 4,
  productName: "核心账户",
  exchange: "OKX",
  exchangeSlug: "okx",
  accountName: "OKX UTA",
    hasPassphrase: true,
  credentialsPresent: true,
  credentialsVerified: true,
  tradingMode: "cex",
  tradingReady: true,
  tradingStatus: "ready",
  tradingUnavailableCode: "",
  tradingUnavailableReason: "",
  createdAt: "2026-01-01T00:00:00Z",
  updatedAt: "2026-01-01T00:00:00Z",
};

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

const eth = {
  ...btc,
  id: 8,
  exchangeSymbol: "ETHUSDT",
  baseAsset: "ETH",
};

const openOrder = {
  id: "ord-2",
  exchangeSymbol: "BTCUSDT",
  side: "buy",
  orderType: "limit",
  status: "open",
  price: "100",
  quantity: "0.001",
  filledQuantity: "0",
  venueOrderId: "venue-2",
  createdAt: "2026-08-17T00:00:00Z",
  updatedAt: "2026-08-17T00:00:00Z",
  lastReconciledAt: "2026-08-17T00:00:01Z",
  syncState: "synced",
  baseAsset: "BTC",
  quoteAsset: "USDT",
};

function setupHappyPath() {
  apiMocks.fetchTradingAccounts.mockResolvedValue([binanceAccount, okxAccount]);
  apiMocks.inspectTradingReadiness.mockResolvedValue({
    tradingReady: true,
    tradingStatus: "ready",
  });
  apiMocks.fetchTraderInstruments.mockResolvedValue([btc, eth]);
  apiMocks.fetchTraderOrderPage.mockResolvedValue({ items: [], nextCursor: "" });
  apiMocks.placeTraderOrder.mockResolvedValue({
    id: "ord-1",
    tradingAccountId: 3,
    side: "buy",
    orderType: "limit",
    status: "open",
    venueOrderId: "venue-9",
  });
}

describe("ManualTradingView", () => {
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      value: "visible",
    });
  });

  it("keeps the trading form in an internal scroller", async () => {
    setupHappyPath();
    const { container } = render(<ManualTradingView />);
    await waitFor(() => expect(screen.getByDisplayValue("Binance")).toBeTruthy());
    const scroller = container.querySelector("[data-manual-trading-scroll]");
    expect(scroller?.className).toContain("overflow-y-auto");
  });

  it("keeps bound accounts when instrument catalog fails", async () => {
    apiMocks.fetchTradingAccounts.mockResolvedValue([
      {
        ...binanceAccount,
        id: 24,
        exchange: "Hyperliquid",
        exchangeSlug: "hyperliquid",
        accountName: "hulong-hy",
        tradingReady: false,
        tradingStatus: "checking",
        credentialsPresent: true,
      },
    ]);
    apiMocks.inspectTradingReadiness.mockResolvedValue({
      tradingReady: false,
      tradingStatus: "checking",
      credentialsPresent: true,
    });
    apiMocks.fetchTraderInstruments.mockRejectedValue(new Error("catalog failed"));
    apiMocks.fetchTraderOrderPage.mockResolvedValue({ items: [], nextCursor: "" });
    render(<ManualTradingView />);
    expect(await screen.findByDisplayValue("hulong-hy（交易能力检查中）")).toBeTruthy();
    expect(screen.getByText("交易标的尚未同步")).toBeTruthy();
  });

  it("filters instruments by fuzzy search", async () => {
    setupHappyPath();
    render(<ManualTradingView />);
    const input = await waitFor(() => {
      const control = screen.getByRole("combobox", { name: "交易标的" }) as HTMLInputElement;
      expect(control.disabled).toBe(false);
      return control;
    }, { timeout: 3_000 });
    fireEvent.focus(input);
    fireEvent.change(input, { target: { value: "eth" } });
    expect(screen.getByRole("option", { name: "ETH / USDT · ETHUSDT" })).toBeTruthy();
    expect(screen.queryByRole("option", { name: "BTC / USDT · BTCUSDT" })).toBeNull();
  });

  it("loads product then account instruments and does not place on first submit", async () => {
    setupHappyPath();
    render(<ManualTradingView />);

    await waitFor(() => {
      expect(screen.getByDisplayValue("Binance")).toBeTruthy();
    });
    expect(apiMocks.fetchTraderInstruments).toHaveBeenCalledWith(3, "perpetual");

    fireEvent.change(screen.getByDisplayValue("Binance UTA"), {
      target: { value: "4" },
    });
    await waitFor(() => {
      expect(apiMocks.fetchTraderInstruments).toHaveBeenCalledWith(4, "perpetual");
    });

    fireEvent.change(screen.getByPlaceholderText("输入委托价格"), {
      target: { value: "100" },
    });
    fireEvent.change(screen.getByPlaceholderText("输入委托数量"), {
      target: { value: "0.001" },
    });
    fireEvent.click(screen.getByRole("button", { name: "提交订单" }));

    expect(await screen.findByRole("button", { name: "确认下单" })).toBeTruthy();
    expect(apiMocks.placeTraderOrder).not.toHaveBeenCalled();
  });

  it("requires quantity and a limit price before opening confirm", async () => {
    setupHappyPath();
    render(<ManualTradingView />);
    await waitFor(() => {
      expect((screen.getByRole("button", { name: "提交订单" }) as HTMLButtonElement).disabled).toBe(false);
    });
    fireEvent.click(screen.getByRole("button", { name: "提交订单" }));
    expect(screen.getByText("请输入有效数量")).toBeTruthy();
    fireEvent.change(screen.getByPlaceholderText("输入委托数量"), {
      target: { value: "0.001" },
    });
    fireEvent.click(screen.getByRole("button", { name: "提交订单" }));
    expect(screen.getByText("限价单需要有效价格")).toBeTruthy();
    expect(apiMocks.placeTraderOrder).not.toHaveBeenCalled();
  });

  it("shows a read-only market price and keeps quantity editable", async () => {
    setupHappyPath();
    render(<ManualTradingView />);
    await waitFor(() => expect(screen.getByDisplayValue("Binance")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "市价单" }));
    expect(screen.getByText("市价 / 按市场最优价")).toBeTruthy();
    expect(screen.queryByPlaceholderText("输入委托价格")).toBeNull();
    fireEvent.change(screen.getByPlaceholderText("输入委托数量"), {
      target: { value: "0.002" },
    });
    fireEvent.click(screen.getByRole("button", { name: "提交订单" }));
    expect(await screen.findByRole("button", { name: "确认下单" })).toBeTruthy();
  });

  it("places once after confirm and locks repeat clicks", async () => {
    setupHappyPath();
    let resolvePlace: (value: unknown) => void = () => undefined;
    apiMocks.placeTraderOrder.mockImplementation(
      () => new Promise((resolve) => {
        resolvePlace = resolve;
      }),
    );
    render(<ManualTradingView />);
    await waitFor(() => expect(screen.getByDisplayValue("Binance")).toBeTruthy());

    fireEvent.change(screen.getByPlaceholderText("输入委托价格"), {
      target: { value: "100" },
    });
    fireEvent.change(screen.getByPlaceholderText("输入委托数量"), {
      target: { value: "0.001" },
    });
    fireEvent.click(screen.getByRole("button", { name: "提交订单" }));
    fireEvent.click(await screen.findByRole("button", { name: "确认下单" }));
    fireEvent.click(screen.getByRole("button", { name: "提交中…" }));

    expect(apiMocks.placeTraderOrder).toHaveBeenCalledTimes(1);
    resolvePlace({
      id: "ord-1",
      tradingAccountId: 3,
      side: "buy",
      orderType: "limit",
      status: "open",
      venueOrderId: "venue-9",
    });
    expect(await screen.findByText(/订单 ord-1 已提交/)).toBeTruthy();
    expect(apiMocks.fetchTraderOrderPage).toHaveBeenLastCalledWith(
      3,
      expect.objectContaining({ view: "open" }),
    );
  });

  it("closes the confirm sheet when place order fails", async () => {
    setupHappyPath();
    apiMocks.placeTraderOrder.mockRejectedValue(new Error("exchange service unavailable"));
    render(<ManualTradingView />);
    await waitFor(() => expect(screen.getByDisplayValue("Binance")).toBeTruthy());
    fireEvent.change(screen.getByPlaceholderText("输入委托价格"), {
      target: { value: "100" },
    });
    fireEvent.change(screen.getByPlaceholderText("输入委托数量"), {
      target: { value: "0.001" },
    });
    fireEvent.click(screen.getByRole("button", { name: "提交订单" }));
    fireEvent.click(await screen.findByRole("button", { name: "确认下单" }));
    expect(await screen.findByText("exchange service unavailable")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "确认下单" })).toBeNull();
  });

  it("refreshes history after a filled place response", async () => {
    setupHappyPath();
    apiMocks.placeTraderOrder.mockResolvedValue({
      id: "ord-fill",
      tradingAccountId: 3,
      side: "buy",
      orderType: "market",
      status: "filled",
      venueOrderId: "venue-fill",
    });
    apiMocks.fetchTraderOrderPage.mockImplementation(async (
      _accountId: number,
      options: { view: "open" | "history" },
    ) => ({
      items: options.view === "history"
        ? [{ ...openOrder, id: "ord-fill", status: "filled", quantity: "0.001" }]
        : [],
      nextCursor: "",
    }));
    render(<ManualTradingView />);
    await waitFor(() => expect(screen.getByDisplayValue("Binance")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "市价单" }));
    fireEvent.change(screen.getByPlaceholderText("输入委托数量"), {
      target: { value: "0.001" },
    });
    fireEvent.click(screen.getByRole("button", { name: "提交订单" }));
    fireEvent.click(await screen.findByRole("button", { name: "确认下单" }));
    expect(await screen.findByText(/订单 ord-fill 已提交/)).toBeTruthy();
    await waitFor(() => {
      expect(apiMocks.fetchTraderOrderPage).toHaveBeenLastCalledWith(
        3,
        expect.objectContaining({ view: "history" }),
      );
    });
    expect(screen.getByText("filled")).toBeTruthy();
  });

  it("groups open limits into the working-order table", async () => {
    setupHappyPath();
    apiMocks.fetchTraderOrderPage.mockResolvedValue({ items: [openOrder], nextCursor: "" });
    render(<ManualTradingView />);
    expect(await screen.findByText("当前委托")).toBeTruthy();
    await waitFor(() => expect(screen.getAllByText("BTCUSDT").length).toBeGreaterThan(0));
    fireEvent.click(screen.getByRole("button", { name: "撤单" }));
    await waitFor(() => {
      expect(apiMocks.cancelTraderOrder).toHaveBeenCalledWith("ord-2");
      expect(apiMocks.fetchTraderOrderPage.mock.calls.length).toBeGreaterThan(1);
    });
  });

  it("does not refresh orders while the page is hidden", async () => {
    setupHappyPath();
    render(<ManualTradingView />);
    await waitFor(() => expect(apiMocks.fetchTraderOrderPage).toHaveBeenCalled());
    const calls = apiMocks.fetchTraderOrderPage.mock.calls.length;
    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      value: "hidden",
    });
    document.dispatchEvent(new Event("visibilitychange"));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(apiMocks.fetchTraderOrderPage).toHaveBeenCalledTimes(calls);
  });

  it("does not mark order sync delayed when a visibility refresh aborts the in-flight poll", async () => {
    setupHappyPath();
    apiMocks.fetchTraderOrderPage.mockImplementation(
      (
        _accountId: number,
        options: { signal?: AbortSignal } = {},
      ) =>
        new Promise((_resolve, reject) => {
          const fail = () =>
            reject(
              new DOMException("signal is aborted without reason", "AbortError"),
            );
          if (options.signal?.aborted) {
            fail();
            return;
          }
          options.signal?.addEventListener("abort", fail, { once: true });
        }),
    );
    render(<ManualTradingView />);
    await waitFor(() => expect(apiMocks.fetchTraderOrderPage).toHaveBeenCalled());
    expect(screen.getByText("同步中…")).toBeTruthy();
    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      value: "hidden",
    });
    document.dispatchEvent(new Event("visibilitychange"));
    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      value: "visible",
    });
    document.dispatchEvent(new Event("visibilitychange"));
    await waitFor(() =>
      expect(apiMocks.fetchTraderOrderPage.mock.calls.length).toBeGreaterThan(1),
    );
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(screen.queryByText("同步延迟")).toBeNull();
  });

  it("switches between current orders and history without per-order requests", async () => {
    setupHappyPath();
    apiMocks.fetchTraderOrderPage.mockImplementation(async (
      _accountId: number,
      options: { view: "open" | "history" },
    ) => ({
      items: options.view === "history"
        ? [{ ...openOrder, id: "history-1", status: "filled", quantity: "1.2000" }]
        : [openOrder],
      nextCursor: "",
    }));
    render(<ManualTradingView />);
    await screen.findByText("BTCUSDT");
    fireEvent.click(screen.getByRole("button", { name: "订单历史" }));
    await waitFor(() => expect(screen.getByText("1.2 BTC")).toBeTruthy());
    expect(apiMocks.fetchTraderOrderPage).toHaveBeenLastCalledWith(
      3,
      expect.objectContaining({ view: "history" }),
    );
  });

  it("closes the confirm sheet when the view is hidden and keeps inputs", async () => {
    setupHappyPath();
    function Harness({ hidden }: { hidden: boolean }) {
      return (
        <Activity mode={hidden ? "hidden" : "visible"}>
          <ManualTradingView />
        </Activity>
      );
    }
    const view = render(<Harness hidden={false} />);
    await waitFor(() => expect(screen.getByDisplayValue("Binance")).toBeTruthy());
    fireEvent.change(screen.getByPlaceholderText("输入委托价格"), {
      target: { value: "100" },
    });
    fireEvent.change(screen.getByPlaceholderText("输入委托数量"), {
      target: { value: "0.001" },
    });
    fireEvent.click(screen.getByRole("button", { name: "提交订单" }));
    expect(await screen.findByRole("button", { name: "确认下单" })).toBeTruthy();
    view.rerender(<Harness hidden />);
    expect(screen.queryByRole("button", { name: "确认下单" })).toBeNull();
    expect(
      (screen.getByPlaceholderText("输入委托数量") as HTMLInputElement)
        .value,
    ).toBe("0.001");
    view.rerender(<Harness hidden={false} />);
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "确认下单", hidden: true })).toBeNull(),
    );
    expect(
      (screen.getByPlaceholderText("输入委托数量") as HTMLInputElement).value,
    ).toBe("0.001");
  });
});
