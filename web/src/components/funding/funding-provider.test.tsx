// @vitest-environment jsdom

import * as React from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const fetchFundingRates = vi.hoisted(() => vi.fn());
const fetchFundingSpreads = vi.hoisted(() => vi.fn());
const fetchTradefiPerps = vi.hoisted(() => vi.fn());

vi.mock("@/lib/api/funding", () => ({
  fetchFundingRates,
  fetchFundingSpreads,
  fetchTradefiPerps,
}));

import { FundingProvider, useFundingSnapshot } from "./funding-provider";

function rates(version: string) {
  return {
    status: "updated" as const,
    etag: `rates-${version}`,
    snapshot: {
      data: [],
      meta: { total: 0, snapshotVersion: version, serverTime: "2026-10-08T01:00:00Z" },
      hasStaleSources: false,
    },
  };
}

function spreads(version: string) {
  return {
    status: "updated" as const,
    etag: `spreads-${version}`,
    snapshot: {
      data: [],
      meta: { total: 0, snapshotVersion: version, serverTime: "2026-10-08T01:00:00Z" },
      hasStaleSources: false,
    },
  };
}

function tradefi(version: string, etag: string) {
  return {
    status: "updated" as const,
    etag,
    snapshot: {
      data: [{ id: version, stockSymbol: "AAPL" }],
      meta: { total: 1, snapshotVersion: version, serverTime: "2026-10-08T01:00:00Z" },
    },
  };
}

function Probe() {
  const value = useFundingSnapshot();
  return (
    <div>
      <div data-testid="rates">{value.snapshot?.meta.snapshotVersion ?? ""}</div>
      <div data-testid="spreads">{value.spreadSnapshot?.meta.snapshotVersion ?? ""}</div>
      <div data-testid="tradefi">{value.tradefiSnapshot?.meta.snapshotVersion ?? ""}</div>
      <div data-testid="error">{value.tradefiError ?? ""}</div>
      <button type="button" onClick={value.retry}>
        重试
      </button>
    </div>
  );
}

describe("FundingProvider TradFi errors", () => {
  afterEach(() => {
    cleanup();
    fetchFundingRates.mockReset();
    fetchFundingSpreads.mockReset();
    fetchTradefiPerps.mockReset();
  });

  it("keeps the snapshot and ETag, then clears the error on a new snapshot or 304", async () => {
    fetchFundingRates.mockResolvedValue(rates("rates-1"));
    fetchFundingSpreads.mockResolvedValue(spreads("spreads-1"));
    fetchTradefiPerps.mockResolvedValueOnce(tradefi("tradefi-1", "etag-1"));
    render(
      <FundingProvider>
        <Probe />
      </FundingProvider>,
    );
    await waitFor(() => expect(screen.getByTestId("tradefi").textContent).toBe("tradefi-1"));

    fetchTradefiPerps.mockRejectedValueOnce(new Error("parse failed"));
    fireEvent.click(screen.getByRole("button", { name: "重试" }));
    await waitFor(() =>
      expect(screen.getByTestId("error").textContent).toBe("TradeFi-Perps 数据加载失败，请重试"),
    );
    expect(screen.getByTestId("tradefi").textContent).toBe("tradefi-1");
    expect(fetchTradefiPerps).toHaveBeenLastCalledWith("etag-1", expect.any(AbortSignal));

    fetchTradefiPerps.mockResolvedValueOnce(tradefi("tradefi-2", "etag-2"));
    fireEvent.click(screen.getByRole("button", { name: "重试" }));
    await waitFor(() => expect(screen.getByTestId("tradefi").textContent).toBe("tradefi-2"));
    expect(screen.getByTestId("error").textContent).toBe("");

    fetchTradefiPerps.mockRejectedValueOnce(new Error("parse failed"));
    fireEvent.click(screen.getByRole("button", { name: "重试" }));
    await waitFor(() => expect(screen.getByTestId("error").textContent).not.toBe(""));
    fetchTradefiPerps.mockResolvedValueOnce({ status: "unchanged", etag: "etag-2" });
    fireEvent.click(screen.getByRole("button", { name: "重试" }));
    await waitFor(() => expect(screen.getByTestId("error").textContent).toBe(""));
    expect(screen.getByTestId("tradefi").textContent).toBe("tradefi-2");
    expect(fetchTradefiPerps).toHaveBeenLastCalledWith("etag-2", expect.any(AbortSignal));
  });

  it("updates funding and spreads when TradFi fails", async () => {
    fetchFundingRates.mockResolvedValueOnce(rates("rates-1"));
    fetchFundingSpreads.mockResolvedValueOnce(spreads("spreads-1"));
    fetchTradefiPerps.mockResolvedValueOnce(tradefi("tradefi-1", "etag-1"));
    render(
      <FundingProvider>
        <Probe />
      </FundingProvider>,
    );
    await waitFor(() => expect(screen.getByTestId("rates").textContent).toBe("rates-1"));

    fetchFundingRates.mockResolvedValueOnce(rates("rates-2"));
    fetchFundingSpreads.mockResolvedValueOnce(spreads("spreads-2"));
    fetchTradefiPerps.mockRejectedValueOnce(new Error("parse failed"));
    fireEvent.click(screen.getByRole("button", { name: "重试" }));
    await waitFor(() => expect(screen.getByTestId("rates").textContent).toBe("rates-2"));
    expect(screen.getByTestId("spreads").textContent).toBe("spreads-2");
    expect(screen.getByTestId("tradefi").textContent).toBe("tradefi-1");
    expect(screen.getByTestId("error").textContent).toBe("TradeFi-Perps 数据加载失败，请重试");
  });
});
