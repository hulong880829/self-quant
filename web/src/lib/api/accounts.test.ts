import { afterEach, describe, expect, it, vi } from "vitest";

import {
  ACCOUNT_FUNDS_MAX_AGE_MS,
  accountSnapshotNeedsLiveRefresh,
  applyTradingAccountProfile,
  createTradingAccount,
  emptyTradingAccountFees,
  fetchCachedTradingAccountSnapshot,
  fetchLiveTradingAccountSnapshot,
  formatAccountFeeSummary,
  formatFeeRatePercent,
  groupAccountsByProduct,
  mapTradingAccountDto,
  normalizeExchange,
  tradingAccountStatusLabel,
  walletDexBindPayload,
  type TradingAccount,
} from "./accounts";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("normalizeExchange", () => {
  it("maps slugs and display names", () => {
    expect(normalizeExchange("binance")).toBe("Binance");
    expect(normalizeExchange("OKX")).toBe("OKX");
    expect(normalizeExchange("hyperliquid")).toBe("Hyperliquid");
    expect(normalizeExchange("aster")).toBe("Aster");
    expect(normalizeExchange("lighter")).toBe("Lighter");
  });
});

describe("walletDexBindPayload", () => {
  it("maps wallet address and private key onto apiKey/apiSecret", () => {
    expect(
      walletDexBindPayload({
        productName: "DEX",
        exchange: "Aster",
        accountName: "main",
        apiKey: "",
        apiSecret: "",
        walletAddress: "0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
        privateKey:
          "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80",
      }),
    ).toMatchObject({
      apiKey: "0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
      apiSecret:
        "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80",
    });
  });

  it("leaves CEX payloads unchanged", () => {
    const payload = {
      productName: "CEX",
      exchange: "binance",
      accountName: "main",
      apiKey: "key",
      apiSecret: "secret",
    };
    expect(walletDexBindPayload(payload)).toEqual(payload);
  });
});

describe("createTradingAccount", () => {
  it("posts mapped wallet fields without adding walletAddress to the body", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          data: {
            id: 9,
            productName: "DEX",
            exchange: "hyperliquid",
            exchangeSlug: "hyperliquid",
            accountName: "main",
            hasPassphrase: false,
            walletAddress: "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
            createdAt: "2026-01-01T00:00:00Z",
            updatedAt: "2026-01-01T00:00:00Z",
          },
        }),
        { status: 201, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    await createTradingAccount({
      productName: "DEX",
      exchange: "Hyperliquid",
      accountName: "main",
      apiKey: "",
      apiSecret: "",
      walletAddress: "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
      privateKey:
        "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80",
      vaultAddress: "0x90F79bf6EB2c4f870365E785982E1f101E93b906",
    });

    const body = JSON.parse(
      String(fetchMock.mock.calls[0]?.[1]?.body ?? "{}"),
    ) as Record<string, string>;
    expect(body).toMatchObject({
      exchange: "hyperliquid",
      apiKey: "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
      apiSecret:
        "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80",
      vaultAddress: "0x90F79bf6EB2c4f870365E785982E1f101E93b906",
    });
    expect(body.tradingApiSecret).toBe("");
    expect(body.signingAddress).toBe("");
    expect(body.accountIndex).toBeUndefined();
    expect(body).not.toHaveProperty("walletAddress");
  });
});

describe("mapTradingAccountDto", () => {
  it("maps DTO fields without secrets", () => {
    const mapped = mapTradingAccountDto({
      id: 7,
      productName: " Funding Arb ",
      exchange: "binance",
      exchangeSlug: "binance",
      accountName: " main ",
      hasPassphrase: true,
      spotFee: { status: "ok", maker: "0.001", taker: "0.001" },
      contractFee: { status: "ok", maker: "0.0002", taker: "0.0004" },
      feeSyncStatus: "ok",
      feeUpdatedAt: "2026-08-30T00:00:00Z",
      createdAt: "2026-01-01T00:00:00Z",
      updatedAt: "2026-01-01T00:00:00Z",
    });
    expect(mapped).toMatchObject({
      id: 7,
      productName: "Funding Arb",
      exchange: "Binance",
      accountName: "main",
      hasPassphrase: true,
      fees: {
        spot: { status: "ok", maker: "0.001", taker: "0.001" },
        contract: { status: "ok", maker: "0.0002", taker: "0.0004" },
        updatedAt: "2026-08-30T00:00:00Z",
        syncStatus: "ok",
        syncError: "",
        stale: false,
        unsupportedMarkets: [],
      },
    });
  });
});

describe("tradingAccountStatusLabel", () => {
  it("keeps checking and untradable accounts visible", () => {
    const checking: TradingAccount = {
      id: 11,
      productName: "funding-arb",
      exchange: "Hyperliquid",
      exchangeSlug: "hyperliquid",
      accountName: "hulong-hy",
      hasPassphrase: false,
      fees: emptyTradingAccountFees(),
      credentialsPresent: true,
      tradingReady: false,
      tradingStatus: "checking",
      createdAt: "",
      updatedAt: "",
    };
    expect(tradingAccountStatusLabel(checking)).toBe("hulong-hy（交易能力检查中）");
    expect(
      tradingAccountStatusLabel({
        ...checking,
        id: 12,
        exchange: "Aster",
        exchangeSlug: "aster",
        accountName: "hulong-aster",
        tradingStatus: "wallet_unauthorized",
      }),
    ).toBe("hulong-aster（不可交易）");
  });
});

describe("groupAccountsByProduct", () => {
  it("builds a two-level product tree", () => {
    const groups = groupAccountsByProduct([
      {
        id: 2,
        productName: "Beta",
        exchange: "OKX",
        exchangeSlug: "okx",
        accountName: "b1",
        hasPassphrase: false,
        fees: emptyTradingAccountFees(),
        createdAt: "",
        updatedAt: "",
      },
      {
        id: 1,
        productName: "Alpha",
        exchange: "Binance",
        exchangeSlug: "binance",
        accountName: "a1",
        hasPassphrase: false,
        fees: emptyTradingAccountFees(),
        createdAt: "",
        updatedAt: "",
      },
      {
        id: 3,
        productName: "Alpha",
        exchange: "Bybit",
        exchangeSlug: "bybit",
        accountName: "a2",
        hasPassphrase: true,
        fees: emptyTradingAccountFees(),
        createdAt: "",
        updatedAt: "",
      },
    ]);
    expect(groups.map((group) => group.productName)).toEqual(["Alpha", "Beta"]);
    expect(groups[0]?.accounts.map((item) => item.accountName)).toEqual([
      "a1",
      "a2",
    ]);
  });

  it("keeps bound Hyperliquid and Aster accounts for funding-arb", () => {
    const groups = groupAccountsByProduct([
      {
        id: 11,
        productName: "funding-arb",
        exchange: "Hyperliquid",
        exchangeSlug: "hyperliquid",
        accountName: "hulong-hy",
        hasPassphrase: false,
        fees: emptyTradingAccountFees(),
        credentialsPresent: true,
        tradingReady: false,
        tradingStatus: "checking",
        createdAt: "",
        updatedAt: "",
      },
      {
        id: 12,
        productName: "funding-arb",
        exchange: "Aster",
        exchangeSlug: "aster",
        accountName: "hulong-aster",
        hasPassphrase: false,
        fees: emptyTradingAccountFees(),
        credentialsPresent: true,
        tradingReady: false,
        tradingStatus: "checking",
        createdAt: "",
        updatedAt: "",
      },
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0]?.productName).toBe("funding-arb");
    expect(groups[0]?.accounts.map((item) => item.accountName).sort()).toEqual([
      "hulong-aster",
      "hulong-hy",
    ]);
    expect(groups[0]?.accounts.some((item) => item.exchangeSlug === "lighter")).toBe(
      false,
    );
  });
});

describe("formatFeeRatePercent", () => {
  it("formats decimal rates as two-place percents", () => {
    expect(formatFeeRatePercent("0.001", "ok")).toBe("0.10%");
    expect(formatFeeRatePercent("0", "ok")).toBe("0.00%");
    expect(formatFeeRatePercent("-0.0002", "ok")).toBe("-0.02%");
    expect(formatFeeRatePercent("0", "unsupported")).toBe("暂不支持");
  });
});

describe("formatAccountFeeSummary", () => {
  it("covers pending, failed, stale, and unsupported copy", () => {
    expect(
      formatAccountFeeSummary({
        ...emptyTradingAccountFees(),
        syncStatus: "pending",
      }),
    ).toBe("手续费待同步");
    expect(
      formatAccountFeeSummary({
        ...emptyTradingAccountFees(),
        spot: { status: "unsupported", maker: "", taker: "" },
        contract: { status: "unsupported", maker: "", taker: "" },
      }),
    ).toBe("暂不支持");
    expect(
      formatAccountFeeSummary(
        {
          spot: { status: "ok", maker: "0.001", taker: "0.001" },
          contract: { status: "ok", maker: "0.0002", taker: "0.0004" },
          updatedAt: "2026-08-30T00:00:00Z",
          syncStatus: "failed",
          syncError: "timeout",
          stale: true,
          unsupportedMarkets: [],
        },
        Date.parse("2026-08-30T00:10:00Z"),
      ),
    ).toBe(
      "现货 Maker 0.10% / Taker 0.10% · 合约 Maker 0.02% / Taker 0.04% · 同步失败 · 已过期 · 10 分钟前更新",
    );
  });
});

describe("applyTradingAccountProfile", () => {
  it("posts only the selected account id and preserves step results", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          data: {
            tradingAccountId: 7,
            productName: "Funding Arb",
            accountName: "main",
            exchange: "binance",
            overallStatus: "manual_required",
            steps: [
              {
                step: "one_way_position",
                status: "manual_required",
                code: "OPEN_ORDERS",
                message: "cancel orders manually",
              },
            ],
          },
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const result = await applyTradingAccountProfile(7);
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/trader/accounts/7/account-profile/apply",
      expect.objectContaining({ method: "POST", credentials: "include" }),
    );
    expect(result.steps[0]).toMatchObject({
      status: "manual_required",
      code: "OPEN_ORDERS",
    });
  });
});

describe("fetchCachedTradingAccountSnapshot", () => {
  it("maps 204 to missing without throwing", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
    await expect(fetchCachedTradingAccountSnapshot(7)).resolves.toEqual({
      status: "missing",
    });
    expect(String(fetchMock.mock.calls[0]?.[0])).toContain(
      "/api/v1/trading-accounts/7/snapshot?cacheOnly=true",
    );
  });

  it("returns a snapshot on 200", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          tradingAccountId: 7,
          availableFundsUsd: "1234.56",
          stale: true,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);
    await expect(fetchCachedTradingAccountSnapshot(7)).resolves.toEqual({
      status: "ok",
      snapshot: expect.objectContaining({
        tradingAccountId: 7,
        availableFundsUsd: "1234.56",
        stale: true,
      }),
    });
  });
});

describe("accountSnapshotNeedsLiveRefresh", () => {
  const sourceUpdatedAt = "2026-01-01T00:00:00.000Z";

  it("does not refresh when the snapshot is younger than 10 minutes", () => {
    expect(
      accountSnapshotNeedsLiveRefresh({
        sourceUpdatedAt,
        serverTime: "2026-01-01T00:09:59.000Z",
      }),
    ).toBe(false);
  });

  it("does not refresh when the snapshot is exactly 10 minutes old", () => {
    expect(
      accountSnapshotNeedsLiveRefresh({
        sourceUpdatedAt,
        serverTime: new Date(
          Date.parse(sourceUpdatedAt) + ACCOUNT_FUNDS_MAX_AGE_MS,
        ).toISOString(),
      }),
    ).toBe(false);
  });

  it("refreshes when the snapshot is older than 10 minutes", () => {
    expect(
      accountSnapshotNeedsLiveRefresh({
        sourceUpdatedAt,
        serverTime: "2026-01-01T00:10:01.000Z",
      }),
    ).toBe(true);
  });

  it("refreshes when sourceUpdatedAt cannot be parsed", () => {
    expect(
      accountSnapshotNeedsLiveRefresh({
        sourceUpdatedAt: "not-a-date",
        serverTime: "2026-01-01T00:00:00.000Z",
      }),
    ).toBe(true);
  });
});

describe("fetchLiveTradingAccountSnapshot", () => {
  it("merges concurrent live requests for the same account", async () => {
    let liveCalls = 0;
    const urls: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        liveCalls += 1;
        urls.push(String(input));
        await new Promise((resolve) => setTimeout(resolve, 30));
        return new Response(
          JSON.stringify({
            tradingAccountId: 7,
            availableFundsUsd: "42",
            sourceUpdatedAt: "2026-01-01T00:00:00.000Z",
            serverTime: "2026-01-01T00:00:01.000Z",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        );
      }),
    );
    const [first, second] = await Promise.all([
      fetchLiveTradingAccountSnapshot(7),
      fetchLiveTradingAccountSnapshot(7),
    ]);
    expect(liveCalls).toBe(1);
    expect(first.availableFundsUsd).toBe("42");
    expect(second.availableFundsUsd).toBe("42");
    expect(urls).toHaveLength(1);
    expect(urls[0]).toContain("/api/v1/trading-accounts/7/snapshot");
    expect(urls[0]).not.toContain("cacheOnly");
  });

  it("does not cancel the shared live request when one waiter aborts", async () => {
    const controller = new AbortController();
    let liveCalls = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        liveCalls += 1;
        await new Promise((resolve) => setTimeout(resolve, 30));
        return new Response(
          JSON.stringify({
            tradingAccountId: 7,
            availableFundsUsd: "42",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        );
      }),
    );
    const first = fetchLiveTradingAccountSnapshot(7, controller.signal);
    const second = fetchLiveTradingAccountSnapshot(7);
    controller.abort();
    await expect(first).rejects.toMatchObject({ name: "AbortError" });
    await expect(second).resolves.toMatchObject({ availableFundsUsd: "42" });
    expect(liveCalls).toBe(1);
  });
});
