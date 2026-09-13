import { expect, type Page, test } from "@playwright/test";

const fundingRow = {
  id: "binance-btcusdt",
  exchange: "Binance",
  exchangeSymbol: "BTCUSDT",
  symbol: "BTCUSDT",
  baseAsset: "BTC",
  quoteAsset: "USDT",
  positionQuantity: "100",
  positionNotional: "6000000",
  dailyVolume: "120000000",
  annualizedRate: "0.1",
  currentFundingRate: "0.0001",
  nextFundingRate: null,
  settlementIntervalHours: 8,
  nextFundingAt: "2026-08-07T20:00:00Z",
  cumulative24h: "0.0003",
  cumulative7d: "0.0021",
  latestPrice: "60000",
  priceChange24h: "0.01",
  sourceUpdatedAt: "2026-08-07T12:00:00Z",
  stale: false,
  index: { name: "BINANCE_INDEX", value: "60000", weight: "100" },
};

const spreadLeg = (exchange: string, rate: string) => ({
  exchange,
  exchangeSymbol: exchange === "Binance" ? "BTCUSDT" : "BTC-USDT-SWAP",
  globalSymbol: "BTCUSDT",
  baseAsset: "BTC",
  quoteAsset: "USDT",
  fundingRate: rate,
  settlementIntervalHours: 8,
  nextFundingAt: "2026-08-07T20:00:00Z",
  positionNotional: "5000000",
  dailyVolume: "90000000",
  latestPrice: "60000",
  sourceUpdatedAt: "2026-08-07T12:00:00Z",
  stale: false,
});

function isTradeWrite(url: string, method: string) {
  if (method === "GET" || method === "HEAD" || method === "OPTIONS") return false;
  return (
    url.includes("/api/v1/trader/orders") ||
    url.includes("/api/v1/trader/twaps") ||
    url.includes("/api/v1/trader/arbitrage-combinations")
  );
}

async function mockWorkspace(page: Page) {
  let session = {
    authenticated: true,
    username: "e2e-user",
    permission: "user",
  };
  const writes: string[] = [];

  await page.route("**/api/v1/**", async (route) => {
    const request = route.request();
    const url = request.url();
    const method = request.method();
    if (isTradeWrite(url, method)) {
      writes.push(`${method} ${url}`);
    }
    if (url.includes("/api/v1/auth/session")) {
      await route.fulfill({ json: session });
      return;
    }
    if (url.includes("/api/v1/auth/logout")) {
      session = { authenticated: false, username: "", permission: "" };
      await route.fulfill({ json: { ok: true } });
      return;
    }
    if (url.includes("/api/v1/auth/login")) {
      session = {
        authenticated: true,
        username: "e2e-user",
        permission: "user",
      };
      await route.fulfill({ json: session });
      return;
    }
    if (url.includes("/api/v1/ai/credentials/openrouter")) {
      await route.fulfill({
        json: {
          data: {
            provider: "openrouter",
            apiKeyMasked: "sk-o****test",
            status: "valid",
            lastError: "",
            lastTestedAt: "2026-08-07T12:00:00Z",
          },
        },
      });
      return;
    }
    if (url.includes("/api/v1/ai/conversations")) {
      await route.fulfill({ json: { data: [], nextCursor: "" } });
      return;
    }
    if (url.includes("/api/v1/funding-rates") && !url.includes("/lookup") && !url.includes("/history")) {
      await route.fulfill({
        json: {
          data: [fundingRow],
          meta: {
            total: 1,
            snapshotVersion: "e2e",
            serverTime: "2026-08-07T12:00:01Z",
          },
        },
      });
      return;
    }
    if (url.includes("/api/v1/funding-spreads")) {
      await route.fulfill({
        json: {
          data: [
            {
              id: "BTCUSDT-binance-okx",
              symbol: "BTCUSDT",
              baseAsset: "BTC",
              quoteAsset: "USDT",
              longLeg: spreadLeg("Binance", "0.0001"),
              shortLeg: spreadLeg("OKX", "0.0002"),
              spreadAnnualized: "0.1",
              spread24hAnnualized: "0.08",
              spread7dAnnualized: "0.05",
              minPositionNotional: "5000000",
              minDailyVolume: "90000000",
              sourceUpdatedAt: "2026-08-07T12:00:00Z",
              stale: false,
            },
          ],
          meta: {
            total: 1,
            snapshotVersion: "e2e-spread",
            serverTime: "2026-08-07T12:00:01Z",
          },
        },
      });
      return;
    }
    if (url.includes("/history")) {
      await route.fulfill({
        json: {
          data: [{ rate: "0.0001", settledAt: "2026-08-07T08:00:00Z" }],
          meta: { exchange: "binance", exchangeSymbol: "BTCUSDT", total: 1 },
        },
      });
      return;
    }
    if (url.includes("/api/v1/trading-accounts") && url.includes("/snapshot")) {
      await route.fulfill({
        json: {
          status: "ok",
          snapshot: {
            tradingAccountId: 3,
            productName: "核心账户",
            exchange: "binance",
            accountName: "Binance UTA",
            accountEquityUsd: "2000",
            availableFundsUsd: "1234.56",
            riskPercent: "0",
            positions: [],
            sourceUpdatedAt: "2026-01-01T00:00:00.000Z",
            serverTime: "2026-01-01T00:01:00.000Z",
            stale: false,
            lastError: "",
          },
        },
      });
      return;
    }
    if (url.includes("/trading-readiness")) {
      await route.fulfill({
        json: {
          data: { tradingReady: true, tradingStatus: "ready" },
        },
      });
      return;
    }
    if (url.includes("/api/v1/trading-accounts") && method === "GET") {
      await route.fulfill({
        json: {
          data: [
            {
              id: 3,
              productName: "核心账户",
              exchange: "Binance",
              exchangeSlug: "binance",
              accountName: "Binance UTA",
              hasPassphrase: false,
              tradingReady: true,
              tradingStatus: "ready",
              createdAt: "2026-01-01T00:00:00Z",
              updatedAt: "2026-01-01T00:00:00Z",
            },
            {
              id: 4,
              productName: "核心账户",
              exchange: "OKX",
              exchangeSlug: "okx",
              accountName: "OKX UTA",
              hasPassphrase: true,
              tradingReady: true,
              tradingStatus: "ready",
              createdAt: "2026-01-01T00:00:00Z",
              updatedAt: "2026-01-01T00:00:00Z",
            },
          ],
          meta: { total: 2 },
        },
      });
      return;
    }
    if (url.includes("/api/v1/trader/accounts/") && url.includes("/instruments")) {
      const parsed = new URL(url);
      const accountId = Number(parsed.pathname.match(/accounts\/(\d+)/)?.[1] ?? "3");
      const contractType =
        parsed.searchParams.get("type") === "spot" ? "spot" : "perpetual";
      const binance = accountId === 3;
      await route.fulfill({
        json: {
          data: [
            {
              id: contractType === "spot" ? (binance ? 6 : 80) : binance ? 7 : 8,
              exchange: binance ? "binance" : "okx",
              contractType,
              exchangeSymbol: binance
                ? "BTCUSDT"
                : contractType === "spot"
                  ? "BTC-USDT"
                  : "BTC-USDT-SWAP",
              baseAsset: "BTC",
              quoteAsset: "USDT",
              settleAsset: "USDT",
              contractSize: "1",
              priceTick: "0.1",
              quantityStep: "0.001",
            },
          ],
          capabilities: {
            products: ["spot", "perpetual"],
            quoteAssets: ["USDT"],
            timeInForce: ["GTC", "IOC", "POST_ONLY"],
            postOnly: true,
            reduceOnly: true,
            makerTwap: true,
            privateOrderStream: false,
            oneWayOnly: false,
          },
        },
      });
      return;
    }
    if (url.includes("/api/v1/trader/orders")) {
      await route.fulfill({ json: { data: [], meta: { nextCursor: "" } } });
      return;
    }
    if (url.includes("/api/v1/trader/twaps")) {
      await route.fulfill({ json: { data: [], meta: { nextCursor: "" } } });
      return;
    }
    if (url.includes("/api/v1/trader/arbitrage-combinations")) {
      await route.fulfill({ json: { data: [], meta: { total: 0, nextCursor: "" } } });
      return;
    }
    if (url.includes("/api/v1/funding-rates/lookup")) {
      const body = request.postDataJSON() as { keys?: unknown[] } | null;
      const keys = Array.isArray(body?.keys) ? body.keys : [];
      await route.fulfill({
        json: {
          results: keys.map(() => ({ status: "missing" })),
          meta: {
            snapshotVersion: "e2e",
            serverTime: "2026-08-07T12:00:01Z",
          },
        },
      });
      return;
    }
    if (url.includes("/api/v1/basis-spreads/")) {
      await route.fulfill({
        json: {
          venue: "binance",
          compareVenue: "okx",
          baseAsset: "BTC",
          quoteAsset: "USDT",
          canonicalSymbol: "BTCUSDT",
          range: "24h",
          resolutionSeconds: 60,
          availability: "unavailable",
          asOf: "2026-08-07T12:00:01Z",
          points: [],
          summary: {
            currentBps: 0,
            minBps: 0,
            maxBps: 0,
            avgBps: 0,
            coverage: 0,
          },
        },
      });
      return;
    }
    await route.fulfill({ json: { data: [], meta: { total: 0 } } });
  });

  return { writes };
}

function fundingSlot(page: Page) {
  return page.locator("[data-workspace-slot='funding']");
}

function tradingSlot(page: Page) {
  return page.locator("[data-workspace-slot='trading']");
}

test.describe("workspace keepalive", () => {
  test("keeps funding filters after a round trip through trading", async ({ page }) => {
    const { writes } = await mockWorkspace(page);
    await page.goto("/funding", { waitUntil: "domcontentloaded" });
    await expect(page.getByRole("link", { name: "Crypto 资金费" })).toBeVisible();
    await expect(fundingSlot(page).getByText("搜索币种")).toBeVisible();
    const search = fundingSlot(page).getByPlaceholder("BTC、ETH、USDT...");
    await search.click();
    await search.fill("");
    await search.pressSequentially("BTC");
    await expect(search).toHaveValue("BTC");
    await expect(page.locator("[data-workspace-epoch]")).toHaveAttribute(
      "data-workspace-epoch",
      "0",
    );

    await page.getByRole("link", { name: "实盘交易" }).click();
    await expect(page.getByRole("heading", { name: "实盘交易" })).toBeVisible();
    await tradingSlot(page).getByPlaceholder("输入委托数量").fill("0.001");

    await page.getByRole("link", { name: "Crypto 资金费" }).click();
    await expect(page.locator("[data-workspace-epoch]")).toHaveAttribute(
      "data-workspace-epoch",
      "0",
    );
    await expect(fundingSlot(page).getByPlaceholder("BTC、ETH、USDT...")).toHaveValue("BTC");
    expect(writes).toEqual([]);
  });

  test("keeps trading drafts after a round trip through funding", async ({ page }) => {
    const { writes } = await mockWorkspace(page);
    await page.goto("/trading", { waitUntil: "domcontentloaded" });
    await expect(page.getByRole("heading", { name: "实盘交易" })).toBeVisible();
    await tradingSlot(page).getByPlaceholder("输入委托数量").fill("0.123");
    await page.getByRole("link", { name: "Crypto 资金费" }).click();
    await expect(fundingSlot(page).getByText("搜索币种")).toBeVisible();
    await page.getByRole("link", { name: "实盘交易" }).click();
    await expect(tradingSlot(page).getByPlaceholder("输入委托数量")).toHaveValue("0.123");
    expect(writes).toEqual([]);
  });

  test("keeps manual and TWAP drafts when switching views", async ({ page }) => {
    const { writes } = await mockWorkspace(page);
    await page.goto("/trading", { waitUntil: "domcontentloaded" });
    await tradingSlot(page).getByPlaceholder("输入委托数量").fill("0.5");
    await page.getByRole("button", { name: /TWAP/ }).click();
    await tradingSlot(page).getByRole("textbox", { name: "总数量" }).fill("2");
    await page.getByRole("button", { name: /手动交易/ }).click();
    await expect(tradingSlot(page).getByPlaceholder("输入委托数量")).toHaveValue("0.5");
    await page.getByRole("button", { name: /TWAP/ }).click();
    await expect(tradingSlot(page).getByRole("textbox", { name: "总数量" })).toHaveValue("2");
    expect(writes).toEqual([]);
  });

  test("opens arbitrage from a funding opportunity prefill", async ({ page }) => {
    const { writes } = await mockWorkspace(page);
    await page.goto("/funding", { waitUntil: "domcontentloaded" });
    await page.getByRole("button", { name: "单所" }).click();
    await fundingSlot(page).locator("tr", { hasText: "BTC" }).first().click();
    await fundingSlot(page).getByRole("button", { name: "开启交易" }).click();
    await expect(page.getByRole("heading", { name: "实盘交易" })).toBeVisible();
    await expect(tradingSlot(page).getByText("目标仓位", { exact: true })).toBeVisible();
    await expect(page).toHaveURL(/mode=arbitrage/);
    await expect(page).toHaveURL(/prefillRequestId=/);
    const firstUrl = new URL(page.url());
    const firstRequestId = firstUrl.searchParams.get("prefillRequestId");
    expect(firstRequestId).toBeTruthy();
    await expect(tradingSlot(page).getByRole("combobox", { name: "交易所" }).nth(0)).toHaveValue("binance");
    await expect(tradingSlot(page).getByRole("combobox", { name: "交易所" }).nth(1)).toHaveValue("binance");
    await expect(tradingSlot(page).getByRole("combobox", { name: "产品类型" }).nth(0)).toHaveValue("spot");
    await expect(tradingSlot(page).getByRole("combobox", { name: "产品类型" }).nth(1)).toHaveValue("perpetual");
    await expect(tradingSlot(page).getByRole("combobox", { name: "账户" }).nth(0)).toHaveValue("3");
    await expect(tradingSlot(page).getByRole("combobox", { name: "账户" }).nth(1)).toHaveValue("3");
    await expect(tradingSlot(page).getByRole("combobox", { name: "Symbol" }).nth(0)).toHaveValue("6");
    await expect(tradingSlot(page).getByRole("combobox", { name: "Symbol" }).nth(1)).toHaveValue("7");
    await expect(tradingSlot(page).getByText(/BTC\/USDT 配对有效/)).toBeVisible();

    await tradingSlot(page).getByRole("combobox", { name: "交易所" }).nth(0).selectOption("okx");
    await expect(tradingSlot(page).getByRole("combobox", { name: "交易所" }).nth(0)).toHaveValue("okx");

    await page.getByRole("link", { name: "Crypto 资金费" }).click();
    await expect(fundingSlot(page).getByText("搜索币种")).toBeVisible();
    await page.getByRole("button", { name: "单所" }).click();
    await fundingSlot(page).locator("tr", { hasText: "BTC" }).first().click();
    await fundingSlot(page).getByRole("button", { name: "开启交易" }).click();
    await expect(page.getByRole("heading", { name: "实盘交易" })).toBeVisible();
    await expect(page).toHaveURL(/prefillRequestId=/);
    const secondRequestId = new URL(page.url()).searchParams.get("prefillRequestId");
    expect(secondRequestId).toBeTruthy();
    expect(secondRequestId).not.toBe(firstRequestId);
    await expect(tradingSlot(page).getByRole("combobox", { name: "交易所" }).nth(0)).toHaveValue("binance");
    await expect(tradingSlot(page).getByRole("combobox", { name: "交易所" }).nth(1)).toHaveValue("binance");
    await expect(tradingSlot(page).getByRole("combobox", { name: "产品类型" }).nth(0)).toHaveValue("spot");
    await expect(tradingSlot(page).getByRole("combobox", { name: "产品类型" }).nth(1)).toHaveValue("perpetual");
    await expect(tradingSlot(page).getByRole("combobox", { name: "Symbol" }).nth(0)).toHaveValue("6");
    await expect(tradingSlot(page).getByText(/BTC\/USDT 配对有效/)).toBeVisible();
    await expect(tradingSlot(page).locator("tr.ring-primary\\/30")).toHaveCount(0);
    expect(writes).toEqual([]);
  });

  test("hard refresh returns to default workspace state", async ({ page }) => {
    await mockWorkspace(page);
    await page.goto("/funding", { waitUntil: "domcontentloaded" });
    await page.getByPlaceholder("BTC、ETH、USDT...").fill("ETH");
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(page.getByPlaceholder("BTC、ETH、USDT...")).toHaveValue("");
  });

  test("logout then login drops trading drafts", async ({ page }) => {
    await mockWorkspace(page);
    await page.goto("/trading", { waitUntil: "domcontentloaded" });
    await tradingSlot(page).getByPlaceholder("输入委托数量").fill("0.001");
    await page.getByRole("button", { name: /e2e-user/ }).click();
    await page.getByText("退出登录").click();
    await expect(page.getByRole("link", { name: "Crypto 资金费" })).toBeVisible();
    await page.getByRole("button", { name: "登录", exact: true }).click();
    await page.getByPlaceholder("请输入账户名").fill("e2e-user");
    await page.getByPlaceholder("请输入密码").fill("password");
    await page.locator("form").getByRole("button", { name: "登录" }).click();
    await page.getByRole("link", { name: "实盘交易" }).click();
    await expect(tradingSlot(page).getByPlaceholder("输入委托数量")).toHaveValue("");
  });
});
