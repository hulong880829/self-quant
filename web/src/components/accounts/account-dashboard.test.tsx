// @vitest-environment jsdom

import * as React from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  fetchTradingAccounts: vi.fn(),
  fetchTradingAccountSnapshot: vi.fn(),
  fetchProductGroupSnapshot: vi.fn(),
  applyTradingAccountProfile: vi.fn(),
  createTradingAccount: vi.fn(),
  deleteTradingAccount: vi.fn(),
}));

vi.mock("@/lib/api/accounts", async () => {
  const actual =
    await vi.importActual<typeof import("@/lib/api/accounts")>(
      "@/lib/api/accounts",
    );
  return { ...actual, ...mocks };
});

import { AccountDashboard } from "./account-dashboard";
import { emptyTradingAccountFees } from "@/lib/api/accounts";

const okFees = {
  spot: { status: "ok", maker: "0.001", taker: "0.001" },
  contract: { status: "ok", maker: "0.0002", taker: "0.0004" },
  updatedAt: "2026-08-30T00:00:00Z",
  syncStatus: "ok",
  syncError: "",
  stale: false,
  unsupportedMarkets: [],
};

const binanceAccount = {
  id: 7,
  productName: "Funding Arb",
  exchange: "Binance" as const,
  exchangeSlug: "binance",
  accountName: "main",
  hasPassphrase: false,
  fees: okFees,
  createdAt: "2026-01-01T00:00:00Z",
  updatedAt: "2026-01-01T00:00:00Z",
};

describe("AccountDashboard account profile", () => {
  beforeEach(() => {
    mocks.fetchTradingAccounts.mockResolvedValue([binanceAccount]);
    mocks.fetchTradingAccountSnapshot.mockResolvedValue({
      tradingAccountId: 7,
      productName: "Funding Arb",
      exchange: "binance",
      accountName: "main",
      accountEquityUsd: "100",
      availableFundsUsd: "80",
      riskPercent: "10",
      positions: [],
      sourceUpdatedAt: "2026-01-01T00:00:00Z",
      serverTime: "2026-01-01T00:00:00Z",
      stale: false,
      lastError: "",
    });
    mocks.applyTradingAccountProfile.mockResolvedValue({
      tradingAccountId: 7,
      productName: "Funding Arb",
      accountName: "main",
      exchange: "binance",
      overallStatus: "manual_required",
      steps: [
        {
          step: "unified_account",
          status: "compliant",
          code: "",
          message: "Portfolio Margin is enabled",
        },
        {
          step: "multi_asset_cross_margin",
          status: "applied",
          code: "",
          message: "cross margin enabled",
        },
        {
          step: "one_way_position",
          status: "manual_required",
          code: "OPEN_ORDERS",
          message: "cancel open orders manually",
        },
      ],
    });
    vi.stubGlobal("confirm", vi.fn(() => true));
  });

  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
    vi.unstubAllGlobals();
  });

  it("shows the global warning and applies only the selected account", async () => {
    render(<AccountDashboard />);

    expect(
      await screen.findByText(/平台 CEX 交易账户仅支持统一账户/),
    ).toBeTruthy();
    fireEvent.click(await screen.findByRole("button", { name: "一键检查并设置" }));

    expect(window.confirm).toHaveBeenCalledWith(
      "确认检查并设置交易账户「main」？只修改当前账户；平台不会自动撤单、平仓、还款或迁移资产。",
    );
    await waitFor(() =>
      expect(mocks.applyTradingAccountProfile).toHaveBeenCalledWith(7),
    );
    expect(await screen.findByText("统一账户")).toBeTruthy();
    expect(screen.getByText("已符合")).toBeTruthy();
    expect(screen.getByText("已设置")).toBeTruthy();
    expect(screen.getByText("需人工处理")).toBeTruthy();
    expect(screen.getByText(/OPEN_ORDERS · cancel open orders manually/)).toBeTruthy();
  });

  it("maps wallet DEX form fields and hides account-profile actions", async () => {
    const hyperliquidAccount = {
      ...binanceAccount,
      id: 11,
      exchange: "Hyperliquid" as const,
      exchangeSlug: "hyperliquid",
      walletAddress: "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
    };
    mocks.fetchTradingAccounts.mockResolvedValue([hyperliquidAccount]);
    mocks.createTradingAccount.mockResolvedValue({
      ...hyperliquidAccount,
      id: 12,
      exchange: "Aster" as const,
      exchangeSlug: "aster",
      accountName: "agent",
    });

    render(<AccountDashboard />);
    expect(
      await screen.findByText(/0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266/),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "一键检查并设置" })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "添加账户" }));
    fireEvent.change(screen.getByDisplayValue("Binance"), {
      target: { value: "Aster" },
    });
    fireEvent.change(screen.getByPlaceholderText("例如 Funding Arb"), {
      target: { value: "DEX" },
    });
    fireEvent.change(screen.getByPlaceholderText("例如 main"), {
      target: { value: "agent" },
    });
    fireEvent.change(screen.getByPlaceholderText("0x…"), {
      target: { value: "0x70997970C51812dc3A010C7d01b50e0d17dc79C8" },
    });
    fireEvent.change(screen.getByLabelText("私钥"), {
      target: {
        value:
          "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80",
      },
    });
    fireEvent.click(screen.getByRole("button", { name: "确认添加" }));

    await waitFor(() =>
      expect(mocks.createTradingAccount).toHaveBeenCalledWith(
        expect.objectContaining({
          exchange: "Aster",
          walletAddress: "0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
          privateKey:
            "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80",
        }),
      ),
    );
  });

  it("does not call the API when confirmation is canceled", async () => {
    vi.mocked(window.confirm).mockReturnValue(false);
    render(<AccountDashboard />);
    fireEvent.click(await screen.findByRole("button", { name: "一键检查并设置" }));
    expect(mocks.applyTradingAccountProfile).not.toHaveBeenCalled();
  });

  it("switches fee summaries from list data and never shows API keys", async () => {
    const okxAccount = {
      ...binanceAccount,
      id: 8,
      exchange: "OKX" as const,
      exchangeSlug: "okx",
      accountName: "okx-main",
      fees: { ...emptyTradingAccountFees(), syncStatus: "pending" },
    };
    const polymarketAccount = {
      ...binanceAccount,
      id: 9,
      exchange: "Polymarket" as const,
      exchangeSlug: "polymarket",
      accountName: "poly",
      fees: {
        ...emptyTradingAccountFees(),
        spot: { status: "unsupported", maker: "", taker: "" },
        contract: { status: "unsupported", maker: "", taker: "" },
        syncStatus: "ok",
        unsupportedMarkets: ["spot", "contract"],
      },
    };
    mocks.fetchTradingAccounts.mockResolvedValue([
      binanceAccount,
      okxAccount,
      polymarketAccount,
    ]);
    mocks.fetchTradingAccountSnapshot.mockImplementation(async (id: number) => ({
      tradingAccountId: id,
      productName: "Funding Arb",
      exchange: id === 7 ? "binance" : id === 8 ? "okx" : "polymarket",
      accountName: id === 7 ? "main" : id === 8 ? "okx-main" : "poly",
      accountEquityUsd: "100",
      availableFundsUsd: "80",
      riskPercent: "10",
      positions: [],
      sourceUpdatedAt: "2026-01-01T00:00:00Z",
      serverTime: "2026-01-01T00:00:00Z",
      stale: false,
      lastError: "",
    }));

    render(<AccountDashboard />);
    expect(await screen.findByText(/现货 Maker 0.10% \/ Taker 0.10%/)).toBeTruthy();
    expect(screen.getByText(/合约 Maker 0.02% \/ Taker 0.04%/)).toBeTruthy();
    expect(document.body.textContent).not.toContain("abcd****");
    expect(document.body.textContent).not.toContain("Key ");
    const listCalls = mocks.fetchTradingAccounts.mock.calls.length;

    fireEvent.click(screen.getByRole("button", { name: /okx-main/ }));
    expect(await screen.findByText(/手续费待同步/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /poly/ }));
    expect(await screen.findByText(/暂不支持/)).toBeTruthy();
    expect(mocks.fetchTradingAccounts).toHaveBeenCalledTimes(listCalls);
  });
});
