import { describe, expect, it } from "vitest";

import {
  groupAccountsByProduct,
  mapTradingAccountDto,
  normalizeExchange,
} from "./accounts";

describe("normalizeExchange", () => {
  it("maps slugs and display names", () => {
    expect(normalizeExchange("binance")).toBe("Binance");
    expect(normalizeExchange("OKX")).toBe("OKX");
    expect(normalizeExchange("hyperliquid")).toBe("Hyperliquid");
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
      apiKeyMasked: "abcd****mnop",
      hasPassphrase: true,
      createdAt: "2026-01-01T00:00:00Z",
      updatedAt: "2026-01-01T00:00:00Z",
    });
    expect(mapped).toMatchObject({
      id: 7,
      productName: "Funding Arb",
      exchange: "Binance",
      accountName: "main",
      apiKeyMasked: "abcd****mnop",
      hasPassphrase: true,
    });
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
        apiKeyMasked: "****",
        hasPassphrase: false,
        createdAt: "",
        updatedAt: "",
      },
      {
        id: 1,
        productName: "Alpha",
        exchange: "Binance",
        exchangeSlug: "binance",
        accountName: "a1",
        apiKeyMasked: "****",
        hasPassphrase: false,
        createdAt: "",
        updatedAt: "",
      },
      {
        id: 3,
        productName: "Alpha",
        exchange: "Bybit",
        exchangeSlug: "bybit",
        accountName: "a2",
        apiKeyMasked: "****",
        hasPassphrase: true,
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
});
