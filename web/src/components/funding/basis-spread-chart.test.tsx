// @vitest-environment jsdom

import * as React from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { BasisSpreadFetchResult, BasisSpreadHistory } from "@/lib/api/spread";

const fetchBasisSpreadHistory = vi.fn<
  (
    venue: string,
    baseAsset: string,
    quoteAsset: string,
    range: string,
    etag?: string | null,
    signal?: AbortSignal,
    compareVenue?: string,
    venueSymbol?: string,
    compareVenueSymbol?: string,
  ) => Promise<BasisSpreadFetchResult>
>();

vi.mock("@/lib/api/spread", () => ({
  fetchBasisSpreadHistory: (
    venue: string,
    baseAsset: string,
    quoteAsset: string,
    range: string,
    etag?: string | null,
    signal?: AbortSignal,
    compareVenue?: string,
    venueSymbol?: string,
    compareVenueSymbol?: string,
  ) => fetchBasisSpreadHistory(
    venue, baseAsset, quoteAsset, range, etag, signal,
    compareVenue, venueSymbol, compareVenueSymbol,
  ),
}));

import {
  BasisSpreadPanel,
  resetBasisSpreadCacheForTests,
  spreadYDomain,
} from "./basis-spread-chart";

function availableHistory(overrides: Partial<BasisSpreadHistory> = {}): BasisSpreadHistory {
  return {
    venue: "binance",
    baseAsset: "BTC",
    quoteAsset: "USDT",
    canonicalSymbol: "BTCUSDT",
    range: "24h",
    resolutionSeconds: 60,
    availability: "available",
    asOf: "2026-08-22T07:00:00Z",
    points: [
      {
        ts: "2026-08-22T06:58:00Z",
        spreadBps: 10,
        spotAsk: 100,
        perpetualAsk: 100.1,
        samples: 2,
      },
      {
        ts: "2026-08-22T06:59:00Z",
        spreadBps: 12.5,
        spotAsk: 100,
        perpetualAsk: 100.125,
        samples: 2,
      },
    ],
    summary: {
      currentBps: 12.5,
      minBps: 8,
      maxBps: 15,
      avgBps: 11,
      coverage: 0.97,
    },
    ...overrides,
  };
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

beforeEach(() => {
  fetchBasisSpreadHistory.mockReset();
  resetBasisSpreadCacheForTests();
});

describe("BasisSpreadPanel", () => {
  it("renders bps summary and chart after load", async () => {
    fetchBasisSpreadHistory.mockResolvedValue({
      status: "updated",
      etag: '"spread-1"',
      history: availableHistory(),
    });
    render(<BasisSpreadPanel venue="Binance" baseAsset="BTC" quoteAsset="USDT" />);
    await waitFor(() => {
      expect(screen.getByText("+12.50 bps")).not.toBeNull();
    });
    expect(screen.getByText("覆盖率")).not.toBeNull();
    expect(screen.getByLabelText("期现 Best Ask 价差走势，北京时间，缺失时段断线显示")).not.toBeNull();
    expect(fetchBasisSpreadHistory).toHaveBeenCalledWith(
      "Binance",
      "BTC",
      "USDT",
      "24h",
      null,
      expect.any(AbortSignal),
      undefined,
      undefined,
      undefined,
    );
  });

  it("uses cross-exchange title and compareVenue when provided", async () => {
    fetchBasisSpreadHistory.mockResolvedValue({
      status: "updated",
      etag: '"spread-x"',
      history: availableHistory(),
    });
    render(
      <BasisSpreadPanel
        venue="Hyperliquid"
        compareVenue="Binance"
        baseAsset="BTC"
        quoteAsset="USDT"
        venueSymbol="BTCUSDC"
        compareVenueSymbol="BTCUSDT"
      />,
    );
    await waitFor(() => {
      expect(screen.getByText("+12.50 bps")).not.toBeNull();
    });
    expect(screen.getByLabelText("跨所 Best Ask 价差")).not.toBeNull();
    expect(screen.getByText("Hyperliquid Ask / Binance Ask - 1 · BTC/USDT")).not.toBeNull();
    expect(fetchBasisSpreadHistory).toHaveBeenCalledWith(
      "Hyperliquid",
      "BTC",
      "USDT",
      "24h",
      null,
      expect.any(AbortSignal),
      "Binance",
      "BTCUSDC",
      "BTCUSDT",
    );
  });

  it("loads a new range immediately when the period changes", async () => {
    fetchBasisSpreadHistory.mockImplementation(async (_venue, _base, _quote, range) => ({
      status: "updated",
      etag: `"spread-${range}"`,
      history: availableHistory({ range: range as BasisSpreadHistory["range"] }),
    }));
    render(<BasisSpreadPanel venue="Binance" baseAsset="BTC" quoteAsset="USDT" />);
    await waitFor(() => {
      expect(screen.getByText("+12.50 bps")).not.toBeNull();
    });
    fireEvent.click(screen.getByRole("button", { name: "1h" }));
    await waitFor(() => {
      expect(fetchBasisSpreadHistory).toHaveBeenCalledWith(
        "Binance",
        "BTC",
        "USDT",
        "1h",
        null,
        expect.any(AbortSignal),
        undefined,
        undefined,
        undefined,
      );
    });
  });

  it("shows the spot-unavailable copy without fabricating a zero spread", async () => {
    fetchBasisSpreadHistory.mockResolvedValue({
      status: "updated",
      etag: '"missing"',
      history: availableHistory({
        availability: "unavailable",
        points: [],
        summary: {
          currentBps: 0,
          minBps: 0,
          maxBps: 0,
          avgBps: 0,
          coverage: 0,
        },
      }),
    });
    render(<BasisSpreadPanel venue="Hyperliquid" baseAsset="BTC" quoteAsset="USDT" />);
    await waitFor(() => {
      expect(screen.getByText("该交易所暂无对应 Spot BBO 数据")).not.toBeNull();
    });
    expect(screen.queryByText("当前")).toBeNull();
  });

  it("shows an empty period message when no paired buckets exist", async () => {
    fetchBasisSpreadHistory.mockResolvedValue({
      status: "updated",
      etag: '"empty"',
      history: availableHistory({
        points: [],
        summary: {
          currentBps: 0,
          minBps: 0,
          maxBps: 0,
          avgBps: 0,
          coverage: 0,
        },
      }),
    });
    render(<BasisSpreadPanel venue="Binance" baseAsset="BTC" quoteAsset="USDT" />);
    await waitFor(() => {
      expect(screen.getByText("当前周期暂无配对价差")).not.toBeNull();
    });
  });

  it("refreshes in the background every 30 seconds", async () => {
    vi.useFakeTimers({ toFake: ["setInterval"] });
    fetchBasisSpreadHistory.mockResolvedValue({
      status: "updated",
      etag: '"spread-refresh"',
      history: availableHistory(),
    });
    render(<BasisSpreadPanel venue="Bybit" baseAsset="SOL" quoteAsset="USDT" />);
    await waitFor(() => {
      expect(fetchBasisSpreadHistory).toHaveBeenCalledTimes(1);
    });
    resetBasisSpreadCacheForTests();
    await vi.advanceTimersByTimeAsync(30_000);
    expect(fetchBasisSpreadHistory).toHaveBeenCalledTimes(2);
  });

  it("retries after an error and aborts the in-flight request on unmount", async () => {
    fetchBasisSpreadHistory
      .mockRejectedValueOnce(new Error("期现价差 API 请求失败 (503)"))
      .mockResolvedValueOnce({
        status: "updated",
        etag: '"ok"',
        history: availableHistory(),
      });
    const view = render(<BasisSpreadPanel venue="Binance" baseAsset="BTC" quoteAsset="USDT" />);
    await waitFor(() => {
      expect(screen.getByText("期现价差 API 请求失败 (503)")).not.toBeNull();
    });
    fireEvent.click(screen.getByRole("button", { name: /重试/ }));
    await waitFor(() => {
      expect(screen.getByText("+12.50 bps")).not.toBeNull();
    });

    let captured: AbortSignal | undefined;
    fetchBasisSpreadHistory.mockImplementation(async (_v, _b, _q, _r, _e, signal) => {
      captured = signal;
      return new Promise(() => undefined);
    });
    fireEvent.click(screen.getByRole("button", { name: "4h" }));
    await waitFor(() => {
      expect(captured).toBeDefined();
    });
    view.unmount();
    expect(captured?.aborted).toBe(true);
  });

  it("fits an all-negative series without a forced zero axis", async () => {
    fetchBasisSpreadHistory.mockResolvedValue({
      status: "updated",
      etag: '"neg"',
      history: availableHistory({
        points: [
          {
            ts: "2026-08-22T06:00:00Z",
            spreadBps: -165.32,
            spotAsk: 2400,
            perpetualAsk: 2360,
            samples: 2,
          },
          {
            ts: "2026-08-22T07:00:00Z",
            spreadBps: -864.64,
            spotAsk: 2544,
            perpetualAsk: 2324,
            samples: 2,
          },
        ],
        summary: {
          currentBps: -864.64,
          minBps: -864.64,
          maxBps: -165.32,
          avgBps: -515,
          coverage: 1,
        },
      }),
    });
    render(<BasisSpreadPanel venue="Binance" baseAsset="ETH" quoteAsset="USDT" />);
    await waitFor(() => {
      expect(screen.getByLabelText("期现 Best Ask 价差走势，北京时间，缺失时段断线显示")).not.toBeNull();
    });
    const yLabels = [...screen.getByLabelText("期现 Best Ask 价差走势，北京时间，缺失时段断线显示")
      .querySelectorAll("text")]
      .map((node) => node.textContent ?? "")
      .filter((label) => /^-?\d+\.\d+$/.test(label))
      .map(Number);
    expect(yLabels.every((value) => value < 0)).toBe(true);
    expect(Math.min(...yLabels)).toBeLessThanOrEqual(-864.64);
    expect(Math.max(...yLabels)).toBeGreaterThanOrEqual(-165.32);
  });
});

describe("spreadYDomain", () => {
  it("pads data min/max without forcing zero", () => {
    const negative = spreadYDomain([-864.64, -165.32]);
    expect(negative.max).toBeLessThan(0);
    expect(negative.min).toBeLessThan(-864.64);
    expect(negative.max).toBeGreaterThan(-165.32);

    const positive = spreadYDomain([8, 15]);
    expect(positive.min).toBeGreaterThan(0);

    const crossed = spreadYDomain([-12, 20]);
    expect(crossed.min).toBeLessThan(0);
    expect(crossed.max).toBeGreaterThan(0);

    const single = spreadYDomain([-40]);
    expect(single.max - single.min).toBeGreaterThanOrEqual(1);
  });
});
