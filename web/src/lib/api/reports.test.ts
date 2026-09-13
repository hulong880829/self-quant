import { afterEach, describe, expect, it, vi } from "vitest";

import {
  createProductCashFlow,
  fetchProductCashFlows,
  fetchProductReport,
  fetchReportProducts,
  mapProductReportResponse,
} from "./reports";

const product = {
  id: "product/one",
  name: "funding-arb",
  displayName: "Funding Arb",
  category: "Crypto",
  strategy: "资金费率套利",
  currency: "USDT",
  inceptionDate: "2026-08-01",
  status: "active",
};

const row = {
  date: "2026-08-12",
  absoluteReturn: "8450.32000000",
  dailyReturn: "0.0024",
  annualizedReturn: null,
  annualized7d: "0.2041",
  annualized30d: "0.1935",
  maxDrawdown: "-0.0218",
  capitalUtilization: "0.764",
  sharpe: "2.36",
  aum: "128450.00",
  volume24h: null,
  status: "provisional",
  partial: true,
};

const recomputingRow = {
  ...row,
  dailyReturn: null,
  status: "recomputing",
  partial: false,
};

const cashFlow = {
  id: "flow/1",
  productId: "product/one",
  type: "subscription",
  amount: "1000.25",
  currency: "USDT",
  occurredAt: "2026-08-12T06:00:00Z",
  flowDate: "2026-08-12",
  note: "manual",
  status: "confirmed",
  createdAt: "2026-08-12T06:01:00Z",
  updatedAt: "2026-08-12T06:01:00Z",
};

afterEach(() => {
  vi.unstubAllGlobals();
});

function jsonResponse(data: unknown) {
  return new Response(JSON.stringify(data), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

describe("report response mapping", () => {
  it("maps decimal strings, nullable metrics and quality metadata", () => {
    const report = mapProductReportResponse({
      data: {
        product,
        latest: row,
        rows: [row],
        aumSeries: [{ date: "2026-08-12", aum: "128450.00" }],
        status: "provisional",
        partial: true,
        errors: ["volume sync pending"],
        dataDate: "2026-08-12",
      },
      meta: { serverTime: "2026-08-12T01:05:00Z" },
    });

    expect(report.latest).toMatchObject({
      absoluteReturn: 8450.32,
      dailyReturn: 0.0024,
      annualizedReturn: null,
      volume24h: null,
    });
    expect(report.aumSeries).toEqual([{ date: "2026-08-12", aum: 128450 }]);
    expect(report).toMatchObject({ status: "provisional", partial: true });
  });

  it("maps recomputing status and keeps empty daily return", () => {
    const report = mapProductReportResponse({
      data: {
        product,
        latest: recomputingRow,
        rows: [recomputingRow],
        aumSeries: [{ date: "2026-08-12", aum: "128450.00" }],
        status: "recomputing",
        partial: false,
        errors: [],
        dataDate: "2026-08-12",
      },
    });
    expect(report.status).toBe("recomputing");
    expect(report.latest).toMatchObject({
      dailyReturn: null,
      status: "recomputing",
    });
  });

  it("rejects numeric JSON decimals", () => {
    expect(() =>
      mapProductReportResponse({
        data: {
          product,
          rows: [{ ...row, aum: 128450 }],
          aumSeries: [],
        },
      }),
    ).toThrow("decimal string");
  });

  it("normalizes AUM history oldest-first without reordering daily rows", () => {
    const latestRow = {
      ...row,
      date: "2026-08-16",
      aum: "9971.00",
      absoluteReturn: "2.74",
      dailyReturn: "0.000274",
    };
    const previousRow = {
      ...row,
      date: "2026-08-15",
      aum: "9968.26",
      absoluteReturn: "0",
      dailyReturn: "0",
    };
    const report = mapProductReportResponse({
      data: {
        product,
        latest: latestRow,
        rows: [latestRow, previousRow],
        aumSeries: [
          { date: "2026-08-16", aum: "9971.00" },
          { date: "2026-08-15", aum: "9968.26" },
        ],
      },
    });

    expect(report.rows.map((item) => item.date)).toEqual([
      "2026-08-16",
      "2026-08-15",
    ]);
    expect(report.aumSeries).toEqual([
      { date: "2026-08-15", aum: 9968.26 },
      { date: "2026-08-16", aum: 9971 },
    ]);
    const previousAum = report.aumSeries.at(-2)!.aum;
    const currentAum = report.latest!.aum;
    const yesterdayChange = (currentAum - previousAum) / previousAum;
    const firstAum = report.aumSeries[0]!.aum;
    const lastAum = report.aumSeries.at(-1)!.aum;
    const intervalChange = (lastAum - firstAum) / firstAum;
    expect(yesterdayChange).toBeCloseTo(0.00027487, 7);
    expect(intervalChange).toBeCloseTo(0.00027487, 7);
  });
});

describe("report endpoints", () => {
  it("loads the product list and encoded detail endpoint", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({ data: [product], meta: { total: 1 } }))
      .mockResolvedValueOnce(
        jsonResponse({
          data: { product, rows: [row], aumSeries: [] },
          meta: {},
        }),
      );
    vi.stubGlobal("fetch", fetchMock);

    await expect(fetchReportProducts()).resolves.toHaveLength(1);
    await expect(fetchProductReport("product/one")).resolves.toMatchObject({
      product: { id: "product/one" },
    });
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/reports/products");
    expect(fetchMock.mock.calls[1]?.[0]).toBe(
      "/api/v1/reports/products/product%2Fone",
    );
  });

  it("loads and creates cash flows with decimal strings", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({ data: [cashFlow], meta: { total: 1 } }))
      .mockResolvedValueOnce(
        jsonResponse({ data: { ...cashFlow, recomputeStatus: "queued" } }),
      );
    vi.stubGlobal("fetch", fetchMock);

    await expect(fetchProductCashFlows("product/one")).resolves.toEqual([
      expect.objectContaining({ flowDate: "2026-08-12" }),
    ]);
    const created = await createProductCashFlow("product/one", {
      type: "subscription",
      amount: "1000.25",
      currency: "USDT",
      occurredAt: "2026-08-12T06:00:00Z",
      note: "manual",
      status: "confirmed",
    });
    expect(created.flowDate).toBe("2026-08-12");
    expect(created.recomputeStatus).toBe("queued");
    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/api/v1/reports/products/product%2Fone/cash-flows",
    );
    expect(fetchMock.mock.calls[1]?.[1]).toMatchObject({ method: "POST" });
    expect(JSON.parse(String(fetchMock.mock.calls[1]?.[1]?.body))).toMatchObject({
      amount: "1000.25",
      status: "confirmed",
    });
  });
});
