import { expect, type Page, test } from "@playwright/test";

const leg = (
  tradingAccountId: number,
  accountName: string,
  exchange: string,
  exchangeSymbol: string,
  instrumentId: number,
) => ({
  tradingAccountId,
  accountName,
  exchange,
  contractType: "perpetual",
  instrumentId,
  exchangeSymbol,
  baseAsset: "BTC",
  quoteAsset: "USDT",
});

function combination(overrides: Record<string, unknown> = {}) {
  return {
    id: "arb-spread",
    productName: "核心账户",
    status: "running",
    legA: leg(3, "Binance Main", "binance", "BTCUSDT", 7),
    legB: leg(4, "OKX Main", "okx", "BTC-USDT-SWAP", 8),
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
    combinedPositionAnnualized: "0.2",
    fundingHistoryComplete: true,
    legAVenueBaselineBasePosition: "0",
    legBVenueBaselineBasePosition: "0",
    venueBaselineCapturedAt: "2026-08-22T10:00:00Z",
    legAExpectedBasePosition: "5",
    legBExpectedBasePosition: "-5",
    legAVenueBasePosition: "5",
    legBVenueBasePosition: "-5",
    legAVenueNotional: "500",
    legBVenueNotional: "505",
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
    ...overrides,
  };
}

const spreadCombo = combination();
const oneShotCombo = combination({
  id: "arb-oneshot",
  runMode: "one_shot",
  entryDirection: "ask",
  oneShotPhase: "waiting_exit",
  exitPolicy: "time",
  exitAfterSeconds: 3600,
  scheduledExitAt: "2026-08-22T11:00:00Z",
});

async function mockArbitragePage(page: Page) {
  await page.addInitScript(
    ({ spreadCombo, oneShotCombo }) => {
      const originalFetch = window.fetch.bind(window);
      const respond = (status: number, body: unknown) =>
        new Response(JSON.stringify(body), {
          status,
          headers: { "content-type": "application/json" },
        });
      window.fetch = async (input, init) => {
        const url =
          typeof input === "string"
            ? input
            : input instanceof URL
              ? input.href
              : input.url;
        const method = (
          init?.method ||
          (typeof input !== "string" &&
          !(input instanceof URL) &&
          "method" in input
            ? input.method
            : "GET") ||
          "GET"
        ).toUpperCase();
        if (url.includes("/api/v1/auth/session")) {
          return respond(200, {
            authenticated: true,
            username: "e2e-user",
            permission: "admin",
          });
        }
        if (url.includes("/snapshot")) {
          const accountId = url.includes("/trading-accounts/3/") ? 3 : 4;
          return respond(200, {
            tradingAccountId: accountId,
            productName: "核心账户",
            exchange: accountId === 3 ? "binance" : "okx",
            accountName: accountId === 3 ? "Binance Main" : "OKX Main",
            accountEquityUsd: "2000",
            availableFundsUsd: accountId === 3 ? "1234.56" : "80",
            riskPercent: "0",
            positions: [],
            sourceUpdatedAt: "2026-01-01T00:00:00Z",
            serverTime: "2026-01-01T00:00:00Z",
            stale: false,
            lastError: "",
          });
        }
        if (url.includes("/api/v1/trading-accounts") && method === "GET") {
          return respond(200, {
            data: [
              {
                id: 3,
                productName: "核心账户",
                exchange: "binance",
                exchangeSlug: "binance",
                accountName: "Binance Main",
                hasPassphrase: false,
                createdAt: "2026-01-01T00:00:00Z",
                updatedAt: "2026-01-01T00:00:00Z",
              },
              {
                id: 4,
                productName: "核心账户",
                exchange: "okx",
                exchangeSlug: "okx",
                accountName: "OKX Main",
                hasPassphrase: true,
                createdAt: "2026-01-01T00:00:00Z",
                updatedAt: "2026-01-01T00:00:00Z",
              },
            ],
          });
        }
        if (url.includes("/instruments")) {
          const binance = url.includes("/accounts/3/");
          return respond(200, {
            data: [
              {
                id: binance ? 7 : 8,
                exchange: binance ? "binance" : "okx",
                contractType: "perpetual",
                exchangeSymbol: binance ? "BTCUSDT" : "BTC-USDT-SWAP",
                baseAsset: "BTC",
                quoteAsset: "USDT",
                settleAsset: "USDT",
                contractSize: "1",
                priceTick: "0.1",
                quantityStep: "0.001",
              },
            ],
          });
        }
        if (url.includes("/funding-rates/lookup")) {
          return respond(200, {
            snapshotVersion: "1",
            serverTime: "2026-08-22T10:01:00Z",
            results: [],
          });
        }
        if (url.includes("/basis-spreads/")) {
          return respond(200, {
            status: "updated",
            history: {
              venue: "okx",
              compareVenue: "binance",
              baseAsset: "BTC",
              quoteAsset: "USDT",
              canonicalSymbol: "BTCUSDT",
              range: "24h",
              resolutionSeconds: 60,
              availability: "available",
              asOf: "2026-08-22T10:01:00Z",
              points: [],
              summary: {
                currentBps: 12.5,
                minBps: 10,
                maxBps: 12.5,
                avgBps: 11.25,
                coverage: 0.95,
              },
            },
          });
        }
        if (url.includes("/arbitrage-combinations/arb-spread")) {
          if (method === "PATCH") {
            const body = JSON.parse(String(init?.body ?? "{}")) as Record<
              string,
              string
            >;
            return respond(200, { data: { ...spreadCombo, ...body } });
          }
          return respond(200, {
            data: {
              ...spreadCombo,
              orders: [],
              recentExecutions: [],
              recentEvents: [],
            },
          });
        }
        if (url.includes("/arbitrage-combinations/arb-oneshot")) {
          return respond(200, {
            data: {
              ...oneShotCombo,
              orders: [],
              recentExecutions: [],
              recentEvents: [],
            },
          });
        }
        if (url.includes("/arbitrage-combinations") && method === "POST") {
          return respond(422, {
            error: "okx available margin is insufficient",
            code: "insufficient_margin",
            leg: "b",
            details: { exchange: "okx" },
          });
        }
        if (url.includes("/arbitrage-combinations") && method === "GET") {
          const view = new URL(url, "http://127.0.0.1").searchParams.get("view");
          const items = view === "running" ? [spreadCombo, oneShotCombo] : [];
          return respond(200, {
            data: items,
            meta: { total: items.length, nextCursor: "" },
          });
        }
        return originalFetch(input, init);
      };
    },
    { spreadCombo, oneShotCombo },
  );
}

test.describe("arbitrage run mode browser checks", () => {
  test.use({ viewport: { width: 1536, height: 864 } });

  test("shows automatic sizing, one_shot fields, 422-by-leg, and edit rules", async ({
    page,
  }) => {
    test.setTimeout(60000);
    await mockArbitragePage(page);
    await page.goto("/trading?mode=arbitrage", { waitUntil: "load" });
    await expect(page.getByText("正在确认登录状态…")).toHaveCount(0, {
      timeout: 30000,
    });
    await expect(page.getByText("BTC/USDT 配对有效")).toBeVisible({
      timeout: 15000,
    });
    await expect(page.getByText("单笔订单金额", { exact: true })).toHaveCount(0);
    await expect(page.getByText("订单金额自动计算")).toHaveCount(0);
    await expect(page.getByText("可用资金").first()).toBeVisible();

    await expect(page.getByRole("combobox", { name: "运行模式" })).toHaveValue("one_shot");
    await expect(page.getByRole("checkbox", { name: "启用8h资金费年化提前退出" })).toBeVisible();
    await expect(page.getByRole("combobox", { name: "入场方向" })).toHaveCount(0);
    await expect(page.getByText("固定买 Leg A / 卖 Leg B")).toBeVisible();
    await expect(page.getByText("开仓阈值")).toHaveCount(0);

    await page.getByRole("button", { name: "创建套利组合" }).click();
    await expect(page.getByText("okx available margin is insufficient")).toHaveCount(2);

    await page.getByRole("combobox", { name: "运行模式" }).selectOption("spread");
    await page.getByText("arb-spread").click();
    await expect(page.getByRole("button", { name: "编辑目标仓位" })).toBeVisible();
    await page.getByRole("button", { name: "编辑目标仓位" }).click();
    const target = page.getByRole("textbox", { name: "编辑目标仓位" });
    await target.fill("12000");
    await target.press("Enter");
    await expect(page.getByText("12000")).toBeVisible();

    await page.getByText("arb-oneshot").click();
    await expect(page.getByText("预计退出时间")).toBeVisible();
    await expect(page.getByText("未启用")).toBeVisible();
    await expect(page.getByRole("button", { name: "编辑目标仓位" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "编辑开仓阈值" })).toHaveCount(0);
  });
});
